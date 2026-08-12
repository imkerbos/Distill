package gitverify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
)

// sshUser 是 deploy key 认证时的用户名。Git 托管方一律用 git 这个账号，
// 真正的身份在密钥里，不在用户名里。
const sshUser = "git"

// ErrNoHostKeys 表示构造时没有拿到任何可用的已知 host key。
var ErrNoHostKeys = errors.New("gitverify: no usable host keys configured")

// errUnknownHostKey 是 host key 不在配置清单里时的拒绝原因。
//
// 刻意不带主机名或 key 指纹：这个错误会一路冒泡到 Classify，而结论是
// 封闭枚举，任何自由文本都不应该有机会跟着走到 API 边界。
var errUnknownHostKey = errors.New("gitverify: host key not in the configured set")

// Verifier 对 Git 绑定做只读校验。
//
// 它持有解析器、已固定的 host key 校验回调与出站超时；**不持有私钥**
// —— 私钥每次校验时现取，只在该次调用栈里存在（spec §2.5）。
type Verifier struct {
	resolver secrets.Resolver
	hostKeys cryptossh.HostKeyCallback
	timeout  time.Duration
}

// New 构造一个 Verifier。
//
// hostKeys 是 known_hosts 格式的已知主机公钥，来自配置 —— 它不是机密，
// 与凭据分开存放（spec §2.2）。**没有 host key 就构造失败**，不存在
// 「未配置就不校验」的分支：回退等于接受任意中间人，而这条链路的终点
// 是生产集群的策略集合。
//
// timeout 必须为正。出站挂在操作者的保存动作上，一个没有超时的出站
// 请求会把界面永久挂住（spec §4）；宁可在启动时拒绝配置，也不要在
// 运行时才发现没有超时。
func New(r secrets.Resolver, hostKeys []byte, timeout time.Duration) (*Verifier, error) {
	if r == nil {
		return nil, errors.New("gitverify: nil secrets resolver")
	}
	if timeout <= 0 {
		return nil, errors.New("gitverify: outbound timeout must be positive")
	}
	cb, err := hostKeyCallback(hostKeys)
	if err != nil {
		return nil, err
	}
	return &Verifier{resolver: r, hostKeys: cb, timeout: timeout}, nil
}

// Verify 对一个绑定做一次只读校验，返回封闭枚举的结论。
//
// **本函数只读，任何时候都不推送。** 它做的全部动作是一次浅的、单分支
// 的、写进内存存储的 clone，加一次 tree 查找。校验发生在操作者按下保存
// 时：如果它会写仓库，一次配置动作就等于产生了一次未经确认的下发
// （spec §3.1，CLAUDE.md 的 dry-run 要求）。
//
// 返回 OK 的含义**仅限于**：凭据可解析、仓库可达、认证通过、分支存在、
// policyPath 在该分支上存在。**它不表示 policyPath 可写。** 可写性不做
// 一次真正的写入是验证不了的，而这里不写。同理 PATH_MISSING 的含义只是
// 「该路径不存在」，不是「该路径不可写」。
//
// 私钥字节从 Resolver 取出后直接交给 go-git 的 SSH 认证，不落盘、不进
// 日志、不进错误信息，也不会留在 Verifier 上（spec §2.5）。
//
// b.RepoURL 必须是 SSH 形态，这条由保存路径保证（registry.validateGit），
// 不在这里重复判断：这个函数回答的是「查了之后发现了什么」，而一个非
// SSH 地址根本到不了查这一步。真让它进来的话，下面挂着的 SSH 认证方法
// 会被传输层当场拒掉 —— 一次拨号都没有，却会被归成「仓库不可达」。
// 那不是一个严格的结论，是一句关于网络的假话。
func (v *Verifier) Verify(ctx context.Context, b registry.GitBinding) registry.VerifyResult {
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	key, err := v.resolver.Resolve(ctx, b.CredentialRef)
	if err != nil {
		return Classify(err)
	}

	auth, err := v.sshAuth(key)
	if err != nil {
		// 取到了内容但它不是一把可用的私钥，仍然是平台侧的凭据问题，
		// 不是仓库侧拒绝 —— 归 CREDENTIAL_UNRESOLVED 才找得对人。
		// 底层错误可能带密钥内容片段，不透传。
		return registry.VerifyCredentialUnresolved
	}

	repo, err := git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
		URL:           b.RepoURL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(b.Branch),
		SingleBranch:  true,
		Depth:         1,
		// 不检出工作区，也没有工作区可检出：存储是 memory，仓库内容
		// 与私钥一样不落盘。
		NoCheckout: true,
		Tags:       git.NoTags,
	})
	if err != nil {
		return Classify(err)
	}

	tree, err := headTree(repo)
	if err != nil {
		return Classify(err)
	}

	if _, err := tree.FindEntry(normalizePolicyPath(b.PolicyPath)); err != nil {
		if isMissingEntry(err) {
			return registry.VerifyPathMissing
		}
		return Classify(err)
	}

	return registry.VerifyOK
}

// sshAuth 把私钥字节做成一个已经钉好 host key 的认证方法。
//
// 单独摘出来不是为了复用，是为了让「认证方法确实带着固定的 host key
// 回调」这件事可以被直接断言。测试用的 file:// 传输不协商 SSH，永远
// 走不到这个回调 —— 把这一行留在 Verify 里面，它被换成
// InsecureIgnoreHostKey 也不会有任何测试变红。
//
// key 只在本调用栈里存在：解析完就交给 go-git，不落盘、不进日志、
// 不挂到 Verifier 上（spec §2.5）。
func (v *Verifier) sshAuth(key []byte) (*gitssh.PublicKeys, error) {
	auth, err := gitssh.NewPublicKeys(sshUser, key, "")
	if err != nil {
		return nil, err
	}
	auth.HostKeyCallback = v.hostKeys
	return auth, nil
}

// headTree 取出 clone 下来的那个分支的顶层 tree。
func headTree(repo *git.Repository) (*object.Tree, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, err
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	return commit.Tree()
}

// normalizePolicyPath 去掉首尾斜杠。
//
// 操作者填 "/policies/prod" 或 "policies/prod/" 都是常见写法，而 tree
// 查找按精确路径匹配 —— 不归一化会把一次笔误报成 PATH_MISSING，那是
// 一个错误的结论，不是一个严格的结论。
func normalizePolicyPath(p string) string {
	return strings.Trim(p, "/")
}

// isMissingEntry 判断 tree 查找的错误是否表示「路径不存在」。
//
// 不并进 Classify：这三个错误只有在 tree 查找这一步才意味着路径缺失，
// 若放进通用映射，clone 阶段任何一个碰巧包着 ErrObjectNotFound 的错误
// 都会被报成 PATH_MISSING —— 那是把不确定说成了确定。
func isMissingEntry(err error) bool {
	return errors.Is(err, object.ErrEntryNotFound) ||
		errors.Is(err, object.ErrDirectoryNotFound) ||
		errors.Is(err, plumbing.ErrObjectNotFound)
}

// hostKeyCallback 用配置里的已知 host key 构造校验回调。
//
// 只接受精确匹配：带 marker（@revoked / @cert-authority）的条目、通配符
// 与哈希过的主机名都不会进集合，因而只会导致拒绝，不会导致放行。这一层
// 的失败方向必须是关，不是开。
func hostKeyCallback(known []byte) (cryptossh.HostKeyCallback, error) {
	authorized := make(map[string]map[string]struct{})

	rest := known
	for len(bytes.TrimSpace(rest)) > 0 {
		marker, hosts, pub, _, next, err := cryptossh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// 不用 %w：底层错误会带上原始行内容。
			return nil, errors.New("gitverify: malformed host key entry")
		}
		rest = next
		if marker != "" {
			continue
		}
		for _, h := range hosts {
			entry := knownhosts.Normalize(h)
			if authorized[entry] == nil {
				authorized[entry] = make(map[string]struct{})
			}
			authorized[entry][string(pub.Marshal())] = struct{}{}
		}
	}

	if len(authorized) == 0 {
		return nil, ErrNoHostKeys
	}

	return func(hostname string, _ net.Addr, key cryptossh.PublicKey) error {
		keys, ok := authorized[knownhosts.Normalize(hostname)]
		if !ok {
			return errUnknownHostKey
		}
		if _, ok := keys[string(key.Marshal())]; !ok {
			return errUnknownHostKey
		}
		return nil
	}, nil
}
