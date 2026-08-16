package kubeclient

import (
	"errors"
	"net/netip"
)

// ErrBlockedDestination 是 apiserver 地址落在禁止网段时的拒绝原因。
//
// 刻意不带地址或网段：这个错误会冒泡到调用方并可能进日志，
// 内网地址属于敏感信息，不得随错误文本走出去（安全规范 §19、§22）。
var ErrBlockedDestination = errors.New("kubeclient: apiserver address is not permitted")

// blockedPrefixes 是禁止拨号的网段。
//
// **本判定刻意比 internal/gitssh 的宽。** 那里拒绝一切私有地址，
// 对 Git 远端是对的 —— 远端本该是互联网上的端点，落进内网就是一次 SSRF。
// 但私有 GKE 集群的 apiserver 端点本来就在 RFC1918 里，
// 照搬那套策略会让采集器连不上正常的生产集群，
// 于是这条守卫会被整体关掉，等于什么都没守。
//
// 这里只拦真正没有正当用途的目标：云元数据端点与链路本地地址。
// 一个 apiserver 永远不会在这些地址上。
var blockedPrefixes = []netip.Prefix{
	// IPv4 链路本地，含 169.254.169.254（GCP / AWS / OpenStack 元数据）。
	netip.MustParsePrefix("169.254.0.0/16"),
	// IPv6 链路本地。
	netip.MustParsePrefix("fe80::/10"),
	// Azure Instance Metadata / WireServer。全局可路由，所有 Is* 判定
	// 都会放行它，只能显式列出。
	netip.MustParsePrefix("168.63.129.16/32"),
	// 阿里云元数据 100.100.100.200 所在的 RFC 6598 共享地址段。
	// 它不是 RFC1918，IsPrivate 判不出来。
	netip.MustParsePrefix("100.64.0.0/10"),
	// AWS IPv6 元数据。
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// checkIP 判定一个 apiserver 地址是否允许拨号。
//
// 认不出形态时拒绝 —— 这一层的失败方向必须是关，不是开。
func checkIP(ip netip.Addr) error {
	if !ip.IsValid() {
		return ErrBlockedDestination
	}
	// 4-in-6 包装与 zone 都在这里自己剥，不假定调用方剥过（安全规范补充版
	// §26）。两者绕过的是同一条判定：blockedPrefixes 按 IPv4 写，而
	// netip.Prefix.Contains 既不拆 ::ffff: 包装、对带 zone 的地址也一律返回
	// false。落空就等于放行，而这两个网段是 168.63.129.16（Azure，全局可
	// 路由）与 100.64.0.0/10（阿里云元数据所在段）唯一的拦截手段 ——
	// fd00:ec2::254（AWS IPv6 元数据）是 ULA，同样只有网段比对拦得住。
	// 拒绝的理由必须在做判定的这一层成立，否则守不守得住取决于下一个
	// 调用方记不记得剥。
	ip = ip.Unmap().WithZone("")
	// 未指定地址、组播、IPv4 广播都不可能是一个 apiserver。
	if !ip.IsGlobalUnicast() && !ip.IsLoopback() {
		return ErrBlockedDestination
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return ErrBlockedDestination
		}
	}
	return nil
}
