// Package gitssh 提供平台唯一一条受守卫的 Git over SSH 出站传输：deploy key
// 认证、钉死的 host key，以及对**真实对端地址**的判定。
//
// **本包自身不授予任何写能力。** 它交出的是一个认证方法，出站要做的是
// 读还是写由调用方发起的 Git 操作决定。抽出来不是为了复用代码，是为了
// 让将来的写路径没有理由另建一条出站链路 —— 认证、host key 与目的地址
// 判定只存在这一份实现，任何一条出站都必须经由它。gitverify 的「只读、
// 永不推送」契约不因为这些函数搬进来而改变。
package gitssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"

	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/imkerbos/Distill/internal/secrets"
)

// sshUser 是 deploy key 认证时的用户名。Git 托管方一律用 git 这个账号，
// 真正的身份在密钥里，不在用户名里。
const sshUser = "git"

// ErrNoHostKeys 表示构造时没有拿到任何可用的已知 host key。
var ErrNoHostKeys = errors.New("gitssh: no usable host keys configured")

// ErrUnusableCredential 表示解析器给出了内容，但它不是一把可用的私钥。
//
// 单列一个哨兵而不是把底层错误往上传：go-git 的私钥解析错误可能带着密钥
// 内容片段，而错误会一路冒泡到调用方的结论映射（spec §2.5）。调用方据此
// 把它归成平台侧的凭据问题，而不是仓库侧拒绝 —— 归错了就会把排查引向
// 错误的负责人。
var ErrUnusableCredential = errors.New("gitssh: credential is not a usable private key")

// errUnknownHostKey 是 host key 不在配置清单里时的拒绝原因。
//
// 刻意不带主机名或 key 指纹：这个错误会一路冒泡到调用方的结论映射，而
// 结论是封闭枚举，任何自由文本都不应该有机会跟着走到 API 边界。
var errUnknownHostKey = errors.New("gitssh: host key not in the configured set")

// Transport 是一条已经钉好 host key、带着目的地址判定与出站超时的 SSH
// 传输配置。
//
// 它持有解析器、host key 校验回调与超时；**不持有私钥** —— 私钥每次
// 建认证方法时现取，只在该次调用栈里存在（spec §2.5）。
type Transport struct {
	resolver secrets.Resolver
	// allowed 是允许出站的私有网段，见 New 的说明。
	allowed  []netip.Prefix
	hostKeys cryptossh.HostKeyCallback
	timeout  time.Duration
}

// New 构造一个 Transport。
//
// hostKeys 是 known_hosts 格式的已知主机公钥，来自配置 —— 它不是机密，
// 与凭据分开存放（spec §2.2）。**没有 host key 就构造失败**，不存在
// 「未配置就不校验」的分支：回退等于接受任意中间人，而这条链路的终点
// 是生产集群的策略集合。
//
// timeout 必须为正。出站挂在操作者的保存动作上，一个没有超时的出站
// 请求会把界面永久挂住（spec §4）；宁可在启动时拒绝配置，也不要在
// 运行时才发现没有超时。
// allowedDestinations 是操作者显式登记的、允许出站的私有网段。
//
// 空表示"只放行公网"——也就是引入这个参数之前的行为（design doc
// 2026-09-01 §3.3）。它只影响私有地址那一档：回环、链路本地与云元数据
// 网段在这份清单之上，改不了。
func New(
	r secrets.Resolver, hostKeys []byte, timeout time.Duration,
	allowedDestinations []netip.Prefix,
) (*Transport, error) {
	if r == nil {
		return nil, errors.New("gitssh: nil secrets resolver")
	}
	if timeout <= 0 {
		return nil, errors.New("gitssh: outbound timeout must be positive")
	}
	cb, err := hostKeyCallback(hostKeys)
	if err != nil {
		return nil, err
	}
	return &Transport{resolver: r, hostKeys: cb, timeout: timeout, allowed: allowedDestinations}, nil
}

// Timeout 是这条传输的出站超时。
//
// 交出去而不是让调用方各存一份：超时是这条出站链路的属性，两处各配一个
// 就会出现「传输以为自己有 10 秒、调用方只给了 2 秒」这类对不上的情况。
func (t *Transport) Timeout() time.Duration { return t.timeout }

// Auth 取出 credentialRef 指向的私钥，做成一个已经钉好 host key、并且带着
// 目的地址判定的认证方法。
//
// 单独摘出来不是为了复用，是为了让「认证方法确实带着固定的 host key
// 回调」这件事可以被直接断言。测试用的 file:// 传输不协商 SSH，永远
// 走不到这个回调 —— 把这一段留在拨号处的内联代码里，它被换成
// InsecureIgnoreHostKey 也不会有任何测试变红。
//
// guardDestination 包在外层：出站唯一的拨号发生在这条链路上，而这个回调
// 是全链路唯一能拿到真实对端地址的位置（见 destination.go）。
//
// 私钥只在本调用栈里存在：解析完就交给 go-git，不落盘、不进日志、
// 不挂到 Transport 上（spec §2.5）。
//
// 解析器的错误原样上抛（调用方要分辨「引用取不到」与「取到了但不可用」）；
// 私钥解析失败一律换成 ErrUnusableCredential，底层错误不往上带。
func (t *Transport) Auth(ctx context.Context, credentialRef string) (*gogitssh.PublicKeys, error) {
	key, err := t.resolver.Resolve(ctx, credentialRef)
	if err != nil {
		return nil, err
	}

	auth, err := gogitssh.NewPublicKeys(sshUser, key, "")
	if err != nil {
		return nil, ErrUnusableCredential
	}
	auth.HostKeyCallback = guardDestination(t.hostKeys, t.allowed)
	return auth, nil
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
			return nil, errors.New("gitssh: malformed host key entry")
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
