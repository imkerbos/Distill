package cluster

import (
	"net/netip"
	"strings"
)

// ParsePrefixes 解析一条网段登记，支持逗号分隔的多段。
//
// **双栈集群的每个 Pod 有两个地址**，一个 IPv4、一个 IPv6。只登记得下一个的
// 话，走另一个协议族的连接会落进 EXTERNAL —— 平台把它当成出公网，于是生成
// 一条 ipBlock 规则而不是 selector 规则，放行面比实际需要的宽得多。
//
// 用逗号分隔而不是加一个新字段：单段登记原样继续工作，绝大多数集群是单栈，
// 不该因为支持多段就要求他们改写已有登记。
//
// **一段解析不出就整条作废**，不是"能用几段算几段"：部分可用的登记会让一部分
// 地址落进 UNKNOWN，而运维看到的是"大部分都对"，那条写错的网段要等到某条
// 流量归属错了才暴露。整条作废会立刻出现在「网段登记坏掉」的告警里。
//
// **这里是全平台唯一的网段登记解析。** 这份判据曾经有三份实现：fleet 的
// 构造、registry 的入库校验、baseline 的 LB 入口地址归类。前两份写下了
// "两者必须一致"的约定，第三份没参加，于是它对着 "10.128.0.0/20,fd00::/64"
// 调 netip.ParsePrefix 直接失败 —— 双栈集群上每一个 LoadBalancer 都推不出
// 对端，报出一串并不存在的缺口。约定靠注释维持不住，靠同一个函数才维持得住。
//
// ok 为 false 表示这个登记用不了（空、有空段、或任一段不是合法网段）。
func ParsePrefixes(cidrs string) (prefixes []netip.Prefix, ok bool) {
	if strings.TrimSpace(cidrs) == "" {
		return nil, false
	}
	parts := strings.Split(cidrs, ",")
	out := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		// 空段多半是手滑留下的多余逗号。静默忽略会让人以为填对了，
		// 而他少填的那一段正是双栈里的另一半。
		seg := strings.TrimSpace(part)
		if seg == "" {
			return nil, false
		}
		p, err := netip.ParsePrefix(seg)
		if err != nil {
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}
