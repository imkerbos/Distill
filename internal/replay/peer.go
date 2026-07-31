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
// 端点身份未还原（Pod 为 nil）时 selector 没有匹配依据，但"没有依据"分成
// 两种结论完全相反的情况，必须分开：本集群内的端点本该能被 selector 选中，
// 身份没还原是数据缺失；外部地址或其他集群的端点本来就选不中，判不匹配
// 是正确结论。见下方分支。
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
		if ep.ClusterID == clusterID {
			// 本集群内但身份未还原：selector 匹配无法判定，不是"不匹配"。
			return false, ReasonSnapshotMissing, nil
		}
		// 外部地址或其他集群：本地 selector 本就选不中，只能靠 ipBlock。
		// 见 §6.2 —— 这种情况判 DENY 是正确结论，不是数据不足。
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
// 解析失败一律不静默判 false：一条写错的 CIDR 静默不匹配，会让本该放行的
// 流量被判成 DENY，进而生成一条错误的策略推荐。
//
// 但两类解析失败的归属完全不同，必须分开返回：
//   - block.CIDR / block.Except 解析失败 —— 策略写错了，返回 error，由调用方
//     归入 POLICY_MALFORMED，修复方式是改 YAML
//   - ip 解析失败 —— 这是流量数据的问题（如流日志没带上源地址），返回
//     ReasonSnapshotMissing。§6.5 单列 POLICY_MALFORMED 就是为了不把人指错
//     子系统，把缺失的流量字段报成策略非法是同一个错误的反方向
func ipBlockMatches(block *networkingv1.IPBlock, ip string) (bool, UnknownReason, error) {
	if block == nil {
		return false, ReasonNone, nil
	}

	addr, ok := parseEndpointAddr(ip)
	if !ok {
		return false, ReasonSnapshotMissing, nil
	}

	cidr, err := netip.ParsePrefix(block.CIDR)
	if err != nil {
		return false, ReasonNone, fmt.Errorf("parse ipBlock cidr %q: %w", block.CIDR, err)
	}
	if !cidr.Contains(addr) {
		return false, ReasonNone, nil
	}

	for _, e := range block.Except {
		ex, err := netip.ParsePrefix(e)
		if err != nil {
			return false, ReasonNone, fmt.Errorf("parse ipBlock except %q: %w", e, err)
		}
		if ex.Contains(addr) {
			return false, ReasonNone, nil
		}
	}
	return true, ReasonNone, nil
}

// parseEndpointAddr 解析端点地址，失败只报"不可用"而不报错——调用方要把
// 它归到数据缺失，而不是策略非法。
//
// 归一化 IPv4-mapped IPv6：::ffff:10.0.0.1 与 10.0.0.1 是同一个地址，但
// netip 视为不同族，不 Unmap 会让它落不进任何 IPv4 CIDR，静默判出错误的
// DENY。Kubernetes 的 PodIP 目前一定是规范形式，但流日志的对端地址不是
// 本包能约束的输入。
func parseEndpointAddr(ip string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
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
		// 唯一的"不可判定"来自端点 IP 本身缺失或非法，由 ipBlockMatches 归类。
		return ipBlockMatches(peer.IPBlock, ep.IP)
	}
	return peerSelectorMatches(peer, policyNamespace, clusterID, ep, namespaces)
}
