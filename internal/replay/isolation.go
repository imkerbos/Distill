package replay

import (
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
)

// selectsPod 判断策略是否选中该 Pod。
//
// 策略只能选中同一集群、同一命名空间内的 Pod。集群维度不可省略：
// 不同集群的同名 namespace 是不同对象，忽略它会让跨集群流量被
// 本集群策略"选中"，判出看似合理实则错误的结论。
func selectsPod(p networkingv1.NetworkPolicy, clusterID string, pod PodRef) (bool, error) {
	if pod.ClusterID != clusterID || pod.Namespace != p.Namespace {
		return false, nil
	}
	ok, err := selectorMatches(&p.Spec.PodSelector, pod.Labels)
	if err != nil {
		return false, fmt.Errorf("policy %s/%s pod selector: %w", p.Namespace, p.Name, err)
	}
	return ok, nil
}

// policyCovers 判断策略是否作用于指定方向。
//
// PolicyTypes 未设置时按 Kubernetes 默认推断：始终含 Ingress；
// 仅当策略含 egress 规则时才额外含 Egress。
func policyCovers(p networkingv1.NetworkPolicy, dir Direction) bool {
	if len(p.Spec.PolicyTypes) > 0 {
		want := networkingv1.PolicyTypeIngress
		if dir == DirectionEgress {
			want = networkingv1.PolicyTypeEgress
		}
		for _, t := range p.Spec.PolicyTypes {
			if t == want {
				return true
			}
		}
		return false
	}

	if dir == DirectionIngress {
		return true
	}
	return len(p.Spec.Egress) > 0
}

// isolated 判断 Pod 在指定方向上是否进入隔离状态。
//
// 只要有任一策略在该方向选中该 Pod，该方向即被隔离：此后必须存在
// 匹配规则才放行，否则阻断。
func isolated(policies []networkingv1.NetworkPolicy, clusterID string, pod PodRef, dir Direction) (bool, error) {
	for _, p := range policies {
		if !policyCovers(p, dir) {
			continue
		}
		ok, err := selectsPod(p, clusterID, pod)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
