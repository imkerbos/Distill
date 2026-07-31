package replay

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func policy(ns, name string, sel map[string]string, types []networkingv1.PolicyType) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: sel},
			PolicyTypes: types,
		},
	}
}

func apiPod(cluster, ns, name string, lbls map[string]string) PodRef {
	return PodRef{ClusterID: cluster, Namespace: ns, Name: name, Labels: lbls}
}

func TestSelectsPod(t *testing.T) {
	p := policy("payment", "deny-all", map[string]string{"app": "api"}, nil)

	tests := []struct {
		name string
		pod  PodRef
		want bool
	}{
		{"same ns and matching labels", apiPod("c1", "payment", "api-1", map[string]string{"app": "api"}), true},
		{"same ns non-matching labels", apiPod("c1", "payment", "worker-1", map[string]string{"app": "worker"}), false},
		{"different namespace", apiPod("c1", "batch", "api-1", map[string]string{"app": "api"}), false},
		{"different cluster", apiPod("c2", "payment", "api-1", map[string]string{"app": "api"}), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectsPod(p, "c1", tt.pod)
			if err != nil {
				t.Fatalf("selectsPod: %v", err)
			}
			if got != tt.want {
				t.Errorf("selectsPod = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectsPodEmptySelectorSelectsWholeNamespace(t *testing.T) {
	p := policy("payment", "default-deny", nil, nil)
	got, err := selectsPod(p, "c1", apiPod("c1", "payment", "anything", map[string]string{"x": "y"}))
	if err != nil {
		t.Fatalf("selectsPod: %v", err)
	}
	if !got {
		t.Error("empty podSelector must select every pod in the namespace")
	}
}

// PolicyTypes 未显式设置时，k8s 的默认推断是：始终含 Ingress；
// 仅当策略含 egress 规则时才含 Egress。推断错会让整个方向漏判。
func TestPolicyCoversInfersPolicyTypes(t *testing.T) {
	noTypes := policy("payment", "p", nil, nil)
	if !policyCovers(noTypes, DirectionIngress) {
		t.Error("policy without policyTypes must cover Ingress")
	}
	if policyCovers(noTypes, DirectionEgress) {
		t.Error("policy without policyTypes and without egress rules must not cover Egress")
	}

	withEgressRule := policy("payment", "p", nil, nil)
	withEgressRule.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if !policyCovers(withEgressRule, DirectionEgress) {
		t.Error("policy with egress rules must cover Egress even without explicit policyTypes")
	}

	explicit := policy("payment", "p", nil, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress})
	if policyCovers(explicit, DirectionIngress) {
		t.Error("explicit policyTypes must be honoured verbatim")
	}
	if !policyCovers(explicit, DirectionEgress) {
		t.Error("explicit Egress policyType must cover Egress")
	}
}

func TestIsolated(t *testing.T) {
	pod := apiPod("c1", "payment", "api-1", map[string]string{"app": "api"})

	ingressOnly := policy("payment", "ing", map[string]string{"app": "api"},
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress})

	got, err := isolated([]networkingv1.NetworkPolicy{ingressOnly}, "c1", pod, DirectionIngress)
	if err != nil {
		t.Fatalf("isolated: %v", err)
	}
	if !got {
		t.Error("pod selected by an ingress policy must be ingress-isolated")
	}

	got, err = isolated([]networkingv1.NetworkPolicy{ingressOnly}, "c1", pod, DirectionEgress)
	if err != nil {
		t.Fatalf("isolated: %v", err)
	}
	if got {
		t.Error("an ingress-only policy must not isolate egress")
	}
}

// 未被任何策略选中的 Pod 不隔离，流量默认放行。
func TestIsolatedUnselectedPodIsNotIsolated(t *testing.T) {
	pod := apiPod("c1", "payment", "worker-1", map[string]string{"app": "worker"})
	p := policy("payment", "ing", map[string]string{"app": "api"}, nil)

	got, err := isolated([]networkingv1.NetworkPolicy{p}, "c1", pod, DirectionIngress)
	if err != nil {
		t.Fatalf("isolated: %v", err)
	}
	if got {
		t.Error("pod not selected by any policy must not be isolated")
	}
}

// isolated 不再参与 Evaluate 的判定路径（隔离与候选匹配已合并为单趟遍历，
// 见 evaluateSide），但它仍是"某方向是否被隔离"这条语义的可执行定义，
// 覆盖率统计等下游会直接用它。因此它的错误路径也必须留有断言。
func TestIsolatedMalformedSelectorReturnsError(t *testing.T) {
	p := policy("payment", "broken", nil, nil)
	p.Spec.PodSelector = metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: "BogusOperator", Values: []string{"x"}},
		},
	}
	pod := apiPod("c1", "payment", "api-1", map[string]string{"app": "api"})

	got, err := isolated([]networkingv1.NetworkPolicy{p}, "c1", pod, DirectionIngress)
	if err == nil {
		t.Fatal("an unparseable pod selector must surface as an error, not a silent false")
	}
	if got {
		t.Error("isolated must be false on the error path")
	}
}
