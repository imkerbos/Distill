package replay

import (
	"fmt"
	"net/netip"

	networkingv1 "k8s.io/api/networking/v1"
)

// peerSelectorMatches 判断 peer 的 selector 组合是否匹配端点。
//
// Kubernetes NetworkPolicyPeer 语义：
//   - 仅 podSelector：匹配策略所在命名空间内、被该 selector 选中的 Pod
//   - 仅 namespaceSelector：匹配被选中命名空间内的全部 Pod
//   - 两者同时存在：取交集
//   - 两者均未设置：本函数不处理，由 ipBlock 分支负责
//
// 端点身份未还原（Pod 为 nil）时一律不匹配：selector 作用于标签，
// 没有标签就没有匹配依据，此时只有 ipBlock 能覆盖。
//
// 返回的 UnknownReason 非 ReasonNone 时表示匹配本身不可判定（如命名空间
// 快照缺失），由本函数直接携带原因返回，不再委托调用方重建——旧版本
// 只返回 bool，调用链里没有任何一层真正把这种情况变成 SNAPSHOT_MISSING，
// 结果是一条本可能放行的流量被静默判成一个可信的 DENY。
func peerSelectorMatches(
	peer networkingv1.NetworkPolicyPeer,
	policyNamespace string,
	clusterID string,
	ep Endpoint,
	namespaces map[string]NamespaceRef,
) (bool, UnknownReason, error) {
	if peer.PodSelector == nil && peer.NamespaceSelector == nil {
		return false, ReasonNone, nil
	}
	if ep.Pod == nil {
		return false, ReasonNone, nil
	}
	pod := ep.Pod
	if pod.ClusterID != clusterID {
		// NetworkPolicy 是集群本地对象：podSelector/namespaceSelector 只能
		// 选中发起策略所在集群里的 Pod。跨集群对端只能靠 ipBlock 匹配 IP，
		// 否则恰好同名的命名空间/标签会让本地策略误"选中"其他集群的 Pod
		// —— 这正是 selectsPod 对策略主体一侧做的同一件事，这里对 peer 侧
		// 补上，保持两侧语义对称。
		return false, ReasonNone, nil
	}

	if peer.NamespaceSelector == nil {
		// 仅 podSelector：限定在策略自身的命名空间内。
		if pod.Namespace != policyNamespace {
			return false, ReasonNone, nil
		}
	} else {
		ns, ok := namespaces[pod.Namespace]
		if !ok {
			// 快照缺失时不猜：猜错会放行本应阻断的流量。
			return false, ReasonSnapshotMissing, nil
		}
		matched, err := selectorMatches(peer.NamespaceSelector, ns.Labels)
		if err != nil {
			return false, ReasonNone, err
		}
		if !matched {
			return false, ReasonNone, nil
		}
	}

	if peer.PodSelector == nil {
		// 仅 namespaceSelector：命名空间内全部 Pod。
		return true, ReasonNone, nil
	}
	matched, err := selectorMatches(peer.PodSelector, pod.Labels)
	return matched, ReasonNone, err
}

// ipBlockMatches 判断 IP 是否落在 ipBlock 的 CIDR 内且未被 except 排除。
//
// 解析失败返回错误而非静默 false：一条写错的 CIDR 静默不匹配，会让
// 本该放行的流量被判成 DENY，进而生成一条错误的策略推荐。
func ipBlockMatches(block *networkingv1.IPBlock, ip string) (bool, error) {
	if block == nil {
		return false, nil
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false, fmt.Errorf("parse endpoint ip %q: %w", ip, err)
	}

	cidr, err := netip.ParsePrefix(block.CIDR)
	if err != nil {
		return false, fmt.Errorf("parse ipBlock cidr %q: %w", block.CIDR, err)
	}
	if !cidr.Contains(addr) {
		return false, nil
	}

	for _, e := range block.Except {
		ex, err := netip.ParsePrefix(e)
		if err != nil {
			return false, fmt.Errorf("parse ipBlock except %q: %w", e, err)
		}
		if ex.Contains(addr) {
			return false, nil
		}
	}
	return true, nil
}

// peerMatches 是 peer 匹配的统一入口。
//
// ipBlock 与 selector 在同一个 peer 内互斥（API 校验保证），因此
// 按字段存在性分派即可。
func peerMatches(
	peer networkingv1.NetworkPolicyPeer,
	policyNamespace string,
	clusterID string,
	ep Endpoint,
	namespaces map[string]NamespaceRef,
) (bool, UnknownReason, error) {
	if peer.IPBlock != nil {
		// ipBlock 按 IP 匹配，不看 Pod 归属的集群，也不涉及命名空间快照——
		// 一个 IP 要么落在 CIDR 内要么不落在，没有"不可判定"这一档。
		matched, err := ipBlockMatches(peer.IPBlock, ep.IP)
		return matched, ReasonNone, err
	}
	return peerSelectorMatches(peer, policyNamespace, clusterID, ep, namespaces)
}
