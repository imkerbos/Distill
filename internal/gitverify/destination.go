package gitverify

import (
	"errors"
	"net"
	"net/netip"

	cryptossh "golang.org/x/crypto/ssh"
)

// errBlockedDestination 是目的地址落在禁止网段时的拒绝原因。
//
// 与 errUnknownHostKey 同理，刻意不带地址、主机名或网段：这个错误会一路
// 冒泡到 Classify，而结论是封闭枚举；内网地址属于敏感信息，不得随错误
// 文本走到 API 响应或日志里（安全规范 §19、§22）。
var errBlockedDestination = errors.New("gitverify: destination address is not permitted")

// blockedPrefixes 是既不是私有地址、也不是链路本地地址，却仍然必须拒绝的
// 云元数据网段。
//
// 169.254.169.254（GCP / AWS / OpenStack）与 fd00:ec2::254（AWS IPv6）已经
// 分别落在链路本地与 ULA 里，不重复列。这里只列那些通用判定覆盖不到的。
var blockedPrefixes = []netip.Prefix{
	// Azure Instance Metadata / WireServer。它是一个全局可路由地址，
	// 所有 Is* 判定都会放行它，只能显式列出。
	netip.MustParsePrefix("168.63.129.16/32"),
	// 阿里云元数据 100.100.100.200 所在的 RFC 6598 共享地址段。它不是
	// RFC1918，IsPrivate 判不出来。
	netip.MustParsePrefix("100.64.0.0/10"),
}

// guardDestination 在 host key 校验之前先判定目的地址，不通过就中断握手。
//
// **挂在 host key 回调上是因为 go-git v5.19.2 的 SSH 传输没有可注入的
// dialer** —— plumbing/transport/ssh 的 dial() 直接调 proxy.Dial，拨号那
// 一步没有留任何钩子。整条链路上唯一能拿到**真实对端地址**的位置就是
// ssh.ClientConfig.HostKeyCallback：x/crypto/ssh 传给它的 net.Addr 取自
// 已建立连接的 RemoteAddr，不是 URL 里的字符串。
//
// 这一点是刻意的。先自己解析主机名、判定安全、再让传输层去解析第二次，
// 两次解析之间就是安全规范补充版 §24 说的 DNS rebinding 窗口；按对端地址
// 判就不存在这个窗口。它同时也挡住了 go-git 会读用户 ssh_config、把
// Hostname 改写到另一个地址的情况 —— 那种改写在 URL 上完全看不出来。
//
// 顺序不能颠倒：地址不合法就不该再去比对 host key。
func guardDestination(next cryptossh.HostKeyCallback) cryptossh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key cryptossh.PublicKey) error {
		if err := checkDestination(remote); err != nil {
			return err
		}
		return next(hostname, remote, key)
	}
}

// checkDestination 判定一个已连上的对端地址是否允许继续。
//
// 认不出地址形态时返回拒绝：这一层的失败方向必须是关，不是开。
func checkDestination(remote net.Addr) error {
	ip, ok := remoteIP(remote)
	if !ok {
		return errBlockedDestination
	}
	return checkIP(ip)
}

// checkIP 对目的 IP 做默认拒绝的判定。
//
// 只放行全局单播、且不在 blockedPrefixes 里的地址；回环、私有、链路本地、
// ULA、未指定、组播与广播一律拒（安全规范 §13）。调用方必须先 Unmap，
// 否则 ::ffff:127.0.0.1 这类写法会绕过按 IPv4 写的判定（补充版 §26）。
func checkIP(ip netip.Addr) error {
	if !ip.IsValid() {
		return errBlockedDestination
	}
	// IsGlobalUnicast 已经排除未指定、回环、组播（含链路本地与接口本地
	// 组播）、链路本地单播与 IPv4 广播；剩下要单独判的只有私有地址。
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return errBlockedDestination
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return errBlockedDestination
		}
	}
	return nil
}

// remoteIP 从连接的对端地址里取出 IP。
//
// 去掉 zone 与 4-in-6 包装后再交给判定：带 zone 的地址会让部分 Is* 判定
// 落进另一条分支，而 ::ffff:10.0.0.1 这种写法在按 IPv6 判时看不出是私有
// 地址（补充版 §26）。
func remoteIP(remote net.Addr) (netip.Addr, bool) {
	if remote == nil {
		return netip.Addr{}, false
	}

	var ip netip.Addr
	switch a := remote.(type) {
	case *net.TCPAddr:
		parsed, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			return netip.Addr{}, false
		}
		ip = parsed
	default:
		host, _, err := net.SplitHostPort(remote.String())
		if err != nil {
			return netip.Addr{}, false
		}
		parsed, err := netip.ParseAddr(host)
		if err != nil {
			return netip.Addr{}, false
		}
		ip = parsed
	}

	return ip.Unmap().WithZone(""), true
}
