package gitverify_test

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
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
// 分支解析、tree 查找与错误映射都是真的。
func TestVerifyReachesARealRepository(t *testing.T) {
	repo := newBareTestRepo(t)
	empty := newEmptyBareRepo(t)
	missing := "file://" + filepath.Join(t.TempDir(), "nowhere")

	cases := []struct {
		name    string
		binding registry.GitBinding
		want    registry.VerifyResult
	}{
		{
			name:    "branch and path both present",
			binding: registry.GitBinding{RepoURL: repo, Branch: "master", PolicyPath: "policies/prod"},
			want:    registry.VerifyOK,
		},
		{
			name:    "path is a file",
			binding: registry.GitBinding{RepoURL: repo, Branch: "master", PolicyPath: "policies/prod/np.yaml"},
			want:    registry.VerifyOK,
		},
		{
			name:    "path written with surrounding slashes",
			binding: registry.GitBinding{RepoURL: repo, Branch: "master", PolicyPath: "/policies/prod/"},
			want:    registry.VerifyOK,
		},
		{
			name:    "branch does not exist",
			binding: registry.GitBinding{RepoURL: repo, Branch: "release-2026", PolicyPath: "policies/prod"},
			want:    registry.VerifyBranchMissing,
		},
		{
			name:    "repository has no branches at all",
			binding: registry.GitBinding{RepoURL: empty, Branch: "master", PolicyPath: "policies/prod"},
			want:    registry.VerifyBranchMissing,
		},
		{
			name:    "path absent under an existing directory",
			binding: registry.GitBinding{RepoURL: repo, Branch: "master", PolicyPath: "policies/staging"},
			want:    registry.VerifyPathMissing,
		},
		{
			name:    "path absent at the top level",
			binding: registry.GitBinding{RepoURL: repo, Branch: "master", PolicyPath: "manifests"},
			want:    registry.VerifyPathMissing,
		},
		{
			name:    "repository is not there",
			binding: registry.GitBinding{RepoURL: missing, Branch: "master", PolicyPath: "policies/prod"},
			want:    registry.VerifyRepoUnreachable,
		},
	}

	v := newVerifier(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := v.Verify(context.Background(), c.binding); got != c.want {
				t.Errorf("Verify() = %q, want %q", got, c.want)
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
	binding := registry.GitBinding{RepoURL: newBareTestRepo(t), Branch: "master", PolicyPath: "policies/prod"}
	if got := v.Verify(context.Background(), binding); got != registry.VerifyCredentialUnresolved {
		t.Fatalf("Verify() = %q, want %q", got, registry.VerifyCredentialUnresolved)
	}
}

// 取到了内容但它不是一把私钥，仍然是平台侧的凭据问题。
func TestVerifyReportsUnusableCredential(t *testing.T) {
	v, err := gitverify.New(stubResolver{key: []byte("this is not a private key")}, hostKeyLine(t), 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	binding := registry.GitBinding{RepoURL: newBareTestRepo(t), Branch: "master", PolicyPath: "policies/prod"}
	if got := v.Verify(context.Background(), binding); got != registry.VerifyCredentialUnresolved {
		t.Fatalf("Verify() = %q, want %q", got, registry.VerifyCredentialUnresolved)
	}
}

// 校验只读：跑完之后远端的引用与 commit 必须一个字节都没变。
func TestVerifyLeavesTheRepositoryUntouched(t *testing.T) {
	url := newBareTestRepo(t)
	dir := strings.TrimPrefix(url, "file://")
	before := refSnapshot(t, dir)

	v := newVerifier(t)
	for _, b := range []registry.GitBinding{
		{RepoURL: url, Branch: "master", PolicyPath: "policies/prod"},
		{RepoURL: url, Branch: "release-2026", PolicyPath: "policies/prod"},
		{RepoURL: url, Branch: "master", PolicyPath: "policies/staging"},
	} {
		v.Verify(context.Background(), b)
	}

	if after := refSnapshot(t, dir); after != before {
		t.Fatalf("Verify() changed the remote refs:\nbefore %s\nafter  %s", before, after)
	}
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
