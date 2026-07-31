package replay

import (
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
func peerSelectorMatches(
	peer networkingv1.NetworkPolicyPeer,
	policyNamespace string,
	ep Endpoint,
	namespaces map[string]NamespaceRef,
) (bool, error) {
	if peer.PodSelector == nil && peer.NamespaceSelector == nil {
		return false, nil
	}
	if ep.Pod == nil {
		return false, nil
	}
	pod := ep.Pod

	if peer.NamespaceSelector == nil {
		// 仅 podSelector：限定在策略自身的命名空间内。
		if pod.Namespace != policyNamespace {
			return false, nil
		}
	} else {
		ns, ok := namespaces[pod.Namespace]
		if !ok {
			// 快照缺失时不猜：猜错会放行本应阻断的流量。
			// 调用方负责把这种情况暴露为 SNAPSHOT_MISSING。
			return false, nil
		}
		matched, err := selectorMatches(peer.NamespaceSelector, ns.Labels)
		if err != nil {
			return false, err
		}
		if !matched {
			return false, nil
		}
	}

	if peer.PodSelector == nil {
		// 仅 namespaceSelector：命名空间内全部 Pod。
		return true, nil
	}
	return selectorMatches(peer.PodSelector, pod.Labels)
}
