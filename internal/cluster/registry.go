// Package cluster 维护 Fleet 内各集群的网段登记与 mesh 检测。
//
// 纯逻辑：零 I/O、零框架依赖，只依赖标准库。把一个 IP 还原成通信主体是
// 全平台每条结论的第一步，而它出错的方式是安静的 —— 归到错误的集群仍然
// 能查出结果、仍然不报错。因此本包的两条规则都是"宁可不答"：
// 网段重叠时返回 AMBIGUOUS 而不任选其一，登记不全时返回 UNKNOWN 而不
// 假定它在集群外。
package cluster

import (
	"fmt"
	"net/netip"
)

// Scope 表示一个 IP 在 Fleet 中的归属类别。
type Scope string

const (
	// ScopePod 表示该 IP 属于某集群的 Pod 网段。
	ScopePod Scope = "POD"
	// ScopeService 表示该 IP 属于某集群的 Service 网段。
	ScopeService Scope = "SERVICE"
	// ScopeNode 表示该 IP 属于某集群的 Node 网段。
	ScopeNode Scope = "NODE"
	// ScopeExternal 表示该 IP 不属于 Fleet 内任何已登记网段，
	// 且登记本身是完整的 —— 这是一个结论，不是一次放弃。
	ScopeExternal Scope = "EXTERNAL"
	// ScopeUnknown 表示登记不足以判定归属。伴随的 Reason 说明缺什么。
	ScopeUnknown Scope = "UNKNOWN"
	// ScopeAmbiguous 表示多个集群的网段同时命中该 IP，归属不唯一。
	ScopeAmbiguous Scope = "AMBIGUOUS"
)

// Reason 是 ScopeUnknown 的原因，封闭枚举。
//
// 不用自由文本：这个值会进快照、进统计口径，自由文本会让"有多少 IP
// 因为缺 Service 网段登记而无法归类"这种问题变成一次字符串匹配。
type Reason string

const (
	// ReasonNoClustersRegistered 表示登记表为空，没有任何判定依据。
	ReasonNoClustersRegistered Reason = "NO_CLUSTERS_REGISTERED"
	// ReasonPodCIDRUnregistered 表示有集群没登记 Pod 网段。
	ReasonPodCIDRUnregistered Reason = "POD_CIDR_UNREGISTERED"
	// ReasonServiceCIDRUnregistered 表示有集群没登记 Service 网段。
	ReasonServiceCIDRUnregistered Reason = "SERVICE_CIDR_UNREGISTERED"
	// ReasonNodeCIDRUnregistered 表示有集群没登记 Node 网段。
	ReasonNodeCIDRUnregistered Reason = "NODE_CIDR_UNREGISTERED"
)

// Cluster 是单个集群的网段登记。
//
// 三类网段都是切片：一个集群可以有多段 Pod 网段（GKE 的次级范围可以
// 追加），用单值表达会在扩容后静默漏掉新网段里的全部 Pod。
type Cluster struct {
	// ID 是集群在 Fleet 内的唯一标识。
	ID string
	// PodCIDRs 是该集群的 Pod 网段。
	PodCIDRs []netip.Prefix
	// ServiceCIDRs 是该集群的 Service 网段。
	ServiceCIDRs []netip.Prefix
	// NodeCIDRs 是该集群的节点网段。
	NodeCIDRs []netip.Prefix
}

// Registry 是全 Fleet 的集群网段登记表。
type Registry struct {
	clusters []Cluster
	// gap 是登记缺口，空表示登记完整。构造时算一次而非每次判定重算：
	// 它只取决于登记内容，与被判定的 IP 无关。
	gap Reason
}

// NewRegistry 用给定集群列表构造登记表。
//
// 复制入参切片：调用方在构造之后修改自己那份，不应该改变一个已经
// 交出去的判定依据。
func NewRegistry(cs []Cluster) *Registry {
	clusters := make([]Cluster, len(cs))
	copy(clusters, cs)
	return &Registry{clusters: clusters, gap: registrationGap(clusters)}
}

// Classification 是一次 IP 归属判定的结果。
type Classification struct {
	// Scope 是归属类别。
	Scope Scope
	// ClusterID 在 Scope 为 EXTERNAL、UNKNOWN 或 AMBIGUOUS 时为空。
	ClusterID string
	// Matches 列出命中的全部集群 ID，用于诊断 AMBIGUOUS。
	Matches []string
	// Reason 仅在 Scope 为 UNKNOWN 时非空。
	Reason Reason
}

// Classify 判定一个 IP 属于哪个集群的哪类网段。
func (r *Registry) Classify(ip string) (Classification, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return Classification{}, fmt.Errorf("parse ip %q: %w", ip, err)
	}
	// 带 zone 的地址一律报错，包括 ::ffff:10.4.1.7%eth0 这种 4-in-6 写法。
	// zone 是"本机某张网卡"的作用域，而本包比对的是全 Fleet 的网段：
	// 拿一个接口作用域的地址问它属于哪个集群，问题本身就没有意义。
	// 这一步必须在 Unmap 之前 —— Unmap 会顺手丢掉 4-in-6 地址的 zone，
	// 放在后面就成了"悄悄丢掉一部分输入再作答"，那是本包最不该有的形状。
	// 不作答而报错，与"认不出的地址报错、不当 EXTERNAL 静默吞掉"同源。
	if addr.Zone() != "" {
		return Classification{}, fmt.Errorf("parse ip %q: zoned address has no fleet-wide meaning", ip)
	}
	// 再拆 4-in-6 包装：netip.Prefix.Contains 不会自己拆，
	// ::ffff:10.4.1.7 于是一段都不命中。登记完整时那条路径给出的是
	// EXTERNAL —— 一个下游会当作事实使用的结论，而不是本包两条规则里
	// 的任何一次弃权。同一个地址不得因为写法不同换一个归属
	// （安全规范补充版 §26）。
	addr = addr.Unmap()

	var (
		matches []string
		scope   Scope
	)
	for _, c := range r.clusters {
		s, ok := c.scopeOf(addr)
		if !ok {
			continue
		}
		matches = append(matches, c.ID)
		scope = s
	}

	switch len(matches) {
	case 0:
		// 没命中任何登记网段有两种成因，而它们的处置完全不同：登记是全的，
		// 那这个 IP 确实在 Fleet 外；登记有缺口，那我们只是没有依据。
		// 把后者说成 EXTERNAL 是把"我们没登记"讲成"它在集群外"。
		if r.gap != "" {
			return Classification{Scope: ScopeUnknown, Reason: r.gap}, nil
		}
		return Classification{Scope: ScopeExternal}, nil
	case 1:
		return Classification{Scope: scope, ClusterID: matches[0], Matches: matches}, nil
	default:
		return Classification{Scope: ScopeAmbiguous, Matches: matches}, nil
	}
}

// scopeOf 返回该地址在本集群内的网段类别。
//
// 三类网段按 Pod → Service → Node 顺序匹配。真实集群里这三段互不重叠，
// 顺序不产生歧义；重叠只可能来自一次错误的登记，那属于登记本身的问题。
func (c Cluster) scopeOf(addr netip.Addr) (Scope, bool) {
	for _, p := range c.PodCIDRs {
		if p.Contains(addr) {
			return ScopePod, true
		}
	}
	for _, p := range c.ServiceCIDRs {
		if p.Contains(addr) {
			return ScopeService, true
		}
	}
	for _, p := range c.NodeCIDRs {
		if p.Contains(addr) {
			return ScopeNode, true
		}
	}
	return "", false
}

// registrationGap 返回登记表中最严重的缺口，登记完整时返回空。
//
// 按 Pod → Service → Node 排优先级并只报一条：Pod 网段是身份还原的主线，
// 缺它意味着连"这是不是我们的 Pod"都答不出；Service 与 Node 各自只影响
// 一类地址。报最严重的那条，操作者知道先补哪个。
func registrationGap(cs []Cluster) Reason {
	if len(cs) == 0 {
		return ReasonNoClustersRegistered
	}
	for _, c := range cs {
		if len(c.PodCIDRs) == 0 {
			return ReasonPodCIDRUnregistered
		}
	}
	for _, c := range cs {
		if len(c.ServiceCIDRs) == 0 {
			return ReasonServiceCIDRUnregistered
		}
	}
	for _, c := range cs {
		if len(c.NodeCIDRs) == 0 {
			return ReasonNodeCIDRUnregistered
		}
	}
	return ""
}
