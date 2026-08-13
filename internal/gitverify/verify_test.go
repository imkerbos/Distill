package gitverify_test

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/imkerbos/Distill/internal/gitverify"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
)

// stubResolver 代替 Secret Manager：返回一把当场生成的私钥，或一个错误。
type stubResolver struct {
	key []byte
	err error
}

func (s stubResolver) Resolve(context.Context, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.key, nil
}

// 未配置 host key 时必须构造失败，而不是回退到不校验。
// 回退等于接受任意中间人，而这条链路的终点是生产集群的策略。
func TestNewRefusesToRunWithoutHostKeys(t *testing.T) {
	if _, err := gitverify.New(stubResolver{}, nil, 10*time.Second); err == nil {
		t.Fatal("New(nil host keys) = nil error, want refusal")
	}
	if _, err := gitverify.New(stubResolver{}, []byte("  \n\n"), 10*time.Second); err == nil {
		t.Fatal("New(blank host keys) = nil error, want refusal")
	}
	// 只有 @revoked 条目等于一把可用的 host key 都没有。
	revoked := append([]byte("@revoked "), hostKeyLine(t)...)
	if _, err := gitverify.New(stubResolver{}, revoked, 10*time.Second); err == nil {
		t.Fatal("New(only revoked host keys) = nil error, want refusal")
	}
}

func TestNewRefusesUnusableConfiguration(t *testing.T) {
	if _, err := gitverify.New(nil, hostKeyLine(t), 10*time.Second); err == nil {
		t.Fatal("New(nil resolver) = nil error, want refusal")
	}
	// 没有超时的出站会把操作者的保存动作永久挂住（spec §4）。
	if _, err := gitverify.New(stubResolver{}, hostKeyLine(t), 0); err == nil {
		t.Fatal("New(zero timeout) = nil error, want refusal")
	}
	if _, err := gitverify.New(stubResolver{}, []byte("not a known_hosts line\n"), 10*time.Second); err == nil {
		t.Fatal("New(malformed host keys) = nil error, want refusal")
	}
}

// 走真实的 clone 代码路径：仓库在 t.TempDir() 里，用 file:// 传输到达。
// SSH 认证与 host key 校验在这条传输上不参与，但 New 仍然是生产构造，
// 分支解析与错误映射都是真的。
func TestVerifyRepoReachesARealRepository(t *testing.T) {
	present := newBareTestRepo(t)
	empty := newEmptyBareRepo(t)
	missing := "file://" + filepath.Join(t.TempDir(), "nowhere")

	cases := []struct {
		name string
		repo registry.GitRepo
		want registry.RepoVerifyResult
	}{
		{
			name: "repository answers and the branch exists",
			repo: registry.GitRepo{URL: present, Branch: "master"},
			want: registry.RepoVerifyOK,
		},
		{
			name: "branch does not exist",
			repo: registry.GitRepo{URL: present, Branch: "release-2026"},
			want: registry.RepoVerifyBranchMissing,
		},
		{
			name: "repository has no branches at all",
			repo: registry.GitRepo{URL: empty, Branch: "master"},
			want: registry.RepoVerifyBranchMissing,
		},
		{
			name: "repository is not there",
			repo: registry.GitRepo{URL: missing, Branch: "master"},
			want: registry.RepoVerifyRepoUnreachable,
		},
	}

	v := newVerifier(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, at := v.VerifyRepo(context.Background(), c.repo)
			if got != c.want {
				t.Errorf("VerifyRepo() = %q, want %q", got, c.want)
			}
			// 无论结论是哪一个，这次校验都实际发生过，那个时刻是历史事实。
			if at == nil {
				t.Error("VerifyRepo() left verifiedAt nil after actually running a check")
			}
		})
	}
}

// 路径级只回答一个问题：policyPath 在不在那个分支上。
func TestVerifyPathReachesARealRepository(t *testing.T) {
	repo := registry.GitRepo{URL: newBareTestRepo(t), Branch: "master"}

	cases := []struct {
		name       string
		policyPath string
		want       registry.BindingVerifyResult
	}{
		{"directory is present", "policies/prod", registry.BindingVerifyOK},
		{"path is a file", "policies/prod/np.yaml", registry.BindingVerifyOK},
		{"path written with surrounding slashes", "/policies/prod/", registry.BindingVerifyOK},
		{"path absent under an existing directory", "policies/staging", registry.BindingVerifyPathMissing},
		{"path absent at the top level", "manifests", registry.BindingVerifyPathMissing},
	}

	v := newVerifier(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, at := v.VerifyPath(context.Background(), repo, registry.RepoVerifyOK, c.policyPath)
			if got != c.want {
				t.Errorf("VerifyPath() = %q, want %q", got, c.want)
			}
			if at == nil {
				t.Error("VerifyPath() left verifiedAt nil after actually looking the path up")
			}
		})
	}
}

// 仓库级没通过时，路径级不得给出 PATH_MISSING：
// 仓库都连不上还说路径不存在，是一句没有依据的结论。
//
// 前两个用例的仓库是**真的、可达的**，路径也真的缺或真的在 —— 也就是说
// 一旦 VerifyPath 不看传进来的仓库级结论，它会当场查出 PATH_MISSING 与
// OK 来。这两个用例因此是对「前提确实被遵守」的证明，而不是对一次本来
// 就查不成的调用的复述。第三个用例才是仓库真的不可达的形态。
func TestPathVerdictIsNotVerifiedWhenTheRepoItselfFailed(t *testing.T) {
	reachable := registry.GitRepo{URL: newBareTestRepo(t), Branch: "master"}
	unreachable := registry.GitRepo{URL: "file://" + filepath.Join(t.TempDir(), "nowhere"), Branch: "master"}

	cases := []struct {
		name       string
		repo       registry.GitRepo
		repoResult registry.RepoVerifyResult
		policyPath string
	}{
		{"path is absent but the repo verdict is auth failed", reachable,
			registry.RepoVerifyAuthFailed, "policies/staging"},
		{"path is present but the repo was never verified", reachable,
			registry.RepoVerifyNotVerified, "policies/prod"},
		{"repository never answered", unreachable,
			registry.RepoVerifyRepoUnreachable, "policies/prod"},
	}

	v := newVerifier(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, at := v.VerifyPath(context.Background(), c.repo, c.repoResult, c.policyPath)
			if got != registry.BindingVerifyNotVerified {
				t.Errorf("VerifyPath(repo verdict %q) = %q, want %q",
					c.repoResult, got, registry.BindingVerifyNotVerified)
			}
			// 路径级校验没有发生过，就没有那个时刻 —— 带时间戳的
			// NOT_VERIFIED 会在界面上显示成一次从未发生的校验。
			if at != nil {
				t.Errorf("VerifyPath() stamped verifiedAt %v for a check that never happened", *at)
			}
		})
	}
}

// 凭据取不到时不得发起出站，也不得报成仓库侧问题 —— 找错负责人比
// 没有结论更糟。
func TestVerifyReportsUnresolvableCredentialWithoutReachingOut(t *testing.T) {
	v, err := gitverify.New(stubResolver{err: secrets.ErrNotFound}, hostKeyLine(t), 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	repo := registry.GitRepo{URL: newBareTestRepo(t), Branch: "master"}
	if got, _ := v.VerifyRepo(context.Background(), repo); got != registry.RepoVerifyCredentialUnresolved {
		t.Fatalf("VerifyRepo() = %q, want %q", got, registry.RepoVerifyCredentialUnresolved)
	}
}

// 还没配 credentialRef 的绑定归「凭据取不到」，不归「仓库不可达」。
//
// 「绑定先记下来、凭据稍后再配」是一个受支持的常规状态（registry 的
// validateGit 明确放行空 credentialRef），所以这条路径操作者一定会走到。
// 报成仓库不可达会把他送去查网络，而该做的事是补一个 credential_ref
// —— spec §3.2 专门写了这两类不得混淆。
//
// 这里用真的 DirResolver 而不是 stubResolver：要验的是空引用穿过一个
// **真实**的解析器之后落在哪一档，stub 会把解析器自己的 ValidateRef
// 一起跳过，于是这条测试就只在测它自己。仓库是存在的，所以「没到达
// 仓库」这个结论若出现，只可能来自错误分类，不可能来自真实的不可达。
func TestVerifyReportsAMissingCredentialRefAsUnresolved(t *testing.T) {
	v, err := gitverify.New(secrets.NewDirResolver(t.TempDir()), hostKeyLine(t), 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	repo := registry.GitRepo{URL: newBareTestRepo(t), Branch: "master", CredentialRef: ""}
	if got, _ := v.VerifyRepo(context.Background(), repo); got != registry.RepoVerifyCredentialUnresolved {
		t.Fatalf("VerifyRepo(empty credentialRef) = %q, want %q", got, registry.RepoVerifyCredentialUnresolved)
	}
}

// 取到了内容但它不是一把私钥，仍然是平台侧的凭据问题。
func TestVerifyReportsUnusableCredential(t *testing.T) {
	v, err := gitverify.New(stubResolver{key: []byte("this is not a private key")}, hostKeyLine(t), 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	repo := registry.GitRepo{URL: newBareTestRepo(t), Branch: "master"}
	if got, _ := v.VerifyRepo(context.Background(), repo); got != registry.RepoVerifyCredentialUnresolved {
		t.Fatalf("VerifyRepo() = %q, want %q", got, registry.RepoVerifyCredentialUnresolved)
	}
}

// 校验只读：两个入口跑完之后，远端的引用与 commit 必须一个字节都没变。
func TestVerifyLeavesTheRepositoryUntouched(t *testing.T) {
	url := newBareTestRepo(t)
	dir := strings.TrimPrefix(url, "file://")
	before := refSnapshot(t, dir)

	v := newVerifier(t)
	for _, r := range []registry.GitRepo{
		{URL: url, Branch: "master"},
		{URL: url, Branch: "release-2026"},
	} {
		v.VerifyRepo(context.Background(), r)
	}
	for _, p := range []string{"policies/prod", "policies/staging"} {
		v.VerifyPath(context.Background(), registry.GitRepo{URL: url, Branch: "master"},
			registry.RepoVerifyOK, p)
	}

	if after := refSnapshot(t, dir); after != before {
		t.Fatalf("verification changed the remote refs:\nbefore %s\nafter  %s", before, after)
	}
}

// 目的地址判定必须管住**真实的那次连接**，不能只管 URL 里的字符串。
//
// 这条用例在回环地址上起一个真的 SSH 监听，并把它的 host key 钉进
// known_hosts —— 也就是说 host key 那一层是放行的，此刻唯一还能拦住这次
// 连接的东西就是目的地址判定。判定失效时客户端会一路走到认证，服务端的
// 认证回调随即被触发；判定生效时握手在密钥交换阶段就断了，服务端永远等
// 不到那一步。断言「服务端没被要求认证」因此是对「判定确实跑在了真实
// 连接上」的证明，而不是对判定函数自身的复述。
func TestVerifyRefusesToConnectToAnInternalAddress(t *testing.T) {
	authAttempted := make(chan struct{}, 1)
	addr, hostKey := startSSHListener(t, authAttempted)

	known := []byte(knownhosts.Normalize(addr) + " " + string(cryptossh.MarshalAuthorizedKey(hostKey)))
	v, err := gitverify.New(stubResolver{key: privateKeyPEM(t)}, known, 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	repo := registry.GitRepo{URL: "ssh://git@" + addr + "/policies.git", Branch: "master"}
	if got, _ := v.VerifyRepo(context.Background(), repo); got != registry.RepoVerifyRepoUnreachable {
		t.Errorf("VerifyRepo(loopback) = %q, want %q", got, registry.RepoVerifyRepoUnreachable)
	}

	// 路径级也会拨号，走的是同一条出站链路。它单独跑一次：光证明仓库级
	// 那个入口被判定管住了，不等于路径级那个入口也经由同一条链路出站。
	// 传 RepoVerifyOK 是为了让它真的走到拨号那一步。
	if got, _ := v.VerifyPath(context.Background(), repo, registry.RepoVerifyOK, "policies/prod"); got != registry.BindingVerifyNotVerified {
		t.Errorf("VerifyPath(loopback) = %q, want %q", got, registry.BindingVerifyNotVerified)
	}

	select {
	case <-authAttempted:
		t.Fatal("the platform authenticated against a loopback address: " +
			"the destination guard did not govern the real connection")
	case <-time.After(500 * time.Millisecond):
	}
}

// startSSHListener 在回环地址上起一个只做握手的 SSH 服务端，返回它的
// 地址与 host key。任何一次公钥认证尝试都会往 attempted 里投一个信号。
func startSSHListener(t *testing.T, attempted chan<- struct{}) (string, cryptossh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("wrap host key: %v", err)
	}

	cfg := &cryptossh.ServerConfig{
		PublicKeyCallback: func(cryptossh.ConnMetadata, cryptossh.PublicKey) (*cryptossh.Permissions, error) {
			select {
			case attempted <- struct{}{}:
			default:
			}
			return nil, errors.New("no")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				sc, chans, reqs, err := cryptossh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer func() { _ = sc.Close() }()
				go cryptossh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(cryptossh.Prohibited, "")
				}
			}()
		}
	}()

	return ln.Addr().String(), signer.PublicKey()
}

func newVerifier(t *testing.T) *gitverify.Verifier {
	t.Helper()
	v, err := gitverify.New(stubResolver{key: privateKeyPEM(t)}, hostKeyLine(t), 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return v
}

// privateKeyPEM 当场生成一把 ed25519 私钥，只在内存里。
func privateKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := cryptossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// hostKeyLine 生成一行 known_hosts 格式的已知主机公钥。
func hostKeyLine(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signerKey, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap host key: %v", err)
	}
	return append([]byte("git.example.com "), cryptossh.MarshalAuthorizedKey(signerKey)...)
}

// newBareTestRepo 建一个带内容的裸仓库，返回它的 file:// 地址。
//
// 先在工作区仓库里造出提交，再裸克隆一份当远端 —— 远端是裸的，形态与
// 真实的策略仓库一致。
func newBareTestRepo(t *testing.T) string {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	repo, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(seed, "policies", "prod"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seed, "policies", "prod", "np.yaml"),
		[]byte("kind: NetworkPolicy\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := w.Add("policies"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := w.Commit("seed", &git.CommitOptions{
		Author: &object.Signature{Name: "distill", Email: "distill@example.com", When: time.Unix(0, 0).UTC()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: "file://" + seed}); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	return "file://" + bare
}

// newEmptyBareRepo 建一个没有任何提交的裸仓库。
func newEmptyBareRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "empty.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	return "file://" + bare
}

// refSnapshot 把仓库当前的全部引用与其指向渲染成一个字符串，用于比对
// 校验前后远端有没有被改动。
func refSnapshot(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	iter, err := repo.References()
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	var lines []string
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		lines = append(lines, ref.String())
		return nil
	}); err != nil {
		t.Fatalf("iterate references: %v", err)
	}
	// 引用的遍历顺序不保证稳定（松散与打包的引用来源不同），排序后再比。
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
