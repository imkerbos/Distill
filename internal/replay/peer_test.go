package replay

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nsIndex(nss ...NamespaceRef) map[string]NamespaceRef {
	idx := make(map[string]NamespaceRef, len(nss))
	for _, ns := range nss {
		idx[ns.Name] = ns
	}
	return idx
}

func endpointOf(pod PodRef) Endpoint {
	return Endpoint{ClusterID: pod.ClusterID, IP: pod.IP, Pod: &pod}
}

func TestPeerSelectorMatches(t *testing.T) {
	namespaces := nsIndex(
		NamespaceRef{ClusterID: "c1", Name: "payment", Labels: map[string]string{"env": "prod"}},
		NamespaceRef{ClusterID: "c1", Name: "gateway", Labels: map[string]string{"env": "prod", "role": "edge"}},
	)

	gatewayPod := PodRef{ClusterID: "c1", Namespace: "gateway", Name: "gw-1", IP: "10.4.0.9",
		Labels: map[string]string{"app": "gateway"}}
	localPod := PodRef{ClusterID: "c1", Namespace: "payment", Name: "api-1", IP: "10.4.0.1",
		Labels: map[string]string{"app": "api"}}

	tests := []struct {
		name string
		peer networkingv1.NetworkPolicyPeer
		ep   Endpoint
		want bool
	}{
		{
			name: "podSelector alone scopes to the policy namespace",
			peer: networkingv1.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			},
			ep:   endpointOf(localPod),
			want: true,
		},
		{
			name: "podSelector alone does not reach other namespaces",
			peer: networkingv1.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
			},
			ep:   endpointOf(gatewayPod),
			want: false,
		},
		{
			name: "namespaceSelector alone selects every pod in matching namespaces",
			peer: networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
			},
			ep:   endpointOf(gatewayPod),
			want: true,
		},
		{
			name: "namespaceSelector does not match a non-selected namespace",
			peer: networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
			},
			ep:   endpointOf(localPod),
			want: false,
		},
		{
			name: "both selectors are ANDed",
			peer: networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
			},
			ep:   endpointOf(gatewayPod),
			want: true,
		},
		{
			name: "both selectors ANDed rejects mismatched pod",
			peer: networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
			},
			ep:   endpointOf(gatewayPod),
			want: false,
		},
		{
			name: "empty namespaceSelector selects all namespaces",
			peer: networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{},
			},
			ep:   endpointOf(gatewayPod),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason, err := peerSelectorMatches(tt.peer, "payment", "c1", tt.ep, namespaces)
			if err != nil {
				t.Fatalf("peerSelectorMatches: %v", err)
			}
			if got != tt.want {
				t.Errorf("peerSelectorMatches = %v, want %v", got, tt.want)
			}
			if reason != ReasonNone {
				t.Errorf("reason = %q, want ReasonNone; every namespace referenced here is in the fixture", reason)
			}
		})
	}
}

// 外部地址没有 ClusterID，本地 selector 本来就选不中它 —— 只有 ipBlock
// 能覆盖。这里的"不匹配"是正确结论，不是数据不足（§6.2）。
func TestPeerSelectorMatchesRequiresIdentity(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	ep := Endpoint{IP: "8.8.8.8"}

	got, reason, err := peerSelectorMatches(peer, "payment", "c1", ep, nil)
	if err != nil {
		t.Fatalf("peerSelectorMatches: %v", err)
	}
	if got {
		t.Error("endpoint without resolved identity must not match a selector-based peer")
	}
	if reason != ReasonNone {
		t.Errorf("reason = %q, want ReasonNone; there is no namespace snapshot lookup without a resolved pod", reason)
	}
}

// Endpoint.Pod 为 nil 有两种成因：外部地址，或快照缺失。压成同一个 false
// 会让后者变成一个可信的 DENY —— 本集群内的 Pod 本该能被 selector 选中，
// 选不中只说明我们不知道它是谁，不说明它不匹配。
func TestPeerSelectorMatchesUnresolvedLocalEndpointIsUnknown(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	ep := Endpoint{ClusterID: "c1", IP: "10.4.0.77"}

	got, reason, err := peerSelectorMatches(peer, "payment", "c1", ep, nil)
	if err != nil {
		t.Fatalf("peerSelectorMatches: %v", err)
	}
	if got {
		t.Error("an unresolved endpoint must not be reported as a match either")
	}
	if reason != ReasonSnapshotMissing {
		t.Errorf("reason = %q, want %q; an in-cluster endpoint with no identity is missing data, not a non-match",
			reason, ReasonSnapshotMissing)
	}
}

// 其他集群的端点即使身份未还原也不是数据不足：本地 selector 无论如何
// 都选不中它，判不匹配是正确结论。
func TestPeerSelectorMatchesUnresolvedRemoteEndpointIsADefiniteNonMatch(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	ep := Endpoint{ClusterID: "c2", IP: "172.16.0.9"}

	got, reason, err := peerSelectorMatches(peer, "payment", "c1", ep, nil)
	if err != nil {
		t.Fatalf("peerSelectorMatches: %v", err)
	}
	if got {
		t.Error("another cluster's endpoint must not match a local selector")
	}
	if reason != ReasonNone {
		t.Errorf("reason = %q, want ReasonNone; a local selector genuinely cannot reach another cluster", reason)
	}
}

// 命名空间快照缺失时不能猜：猜错会放行本应阻断的流量。这里必须直接
// 携带 ReasonSnapshotMissing 返回，而不是退化成一个无法区分的 false——
// 后者曾经让调用链把这种情况悄悄当成"不匹配"，产出一个可信的 DENY。
func TestPeerSelectorMatchesMissingNamespaceSnapshot(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{}}
	pod := PodRef{ClusterID: "c1", Namespace: "unknown-ns", Name: "x"}

	got, reason, err := peerSelectorMatches(peer, "payment", "c1", endpointOf(pod), nsIndex())
	if err != nil {
		t.Fatalf("peerSelectorMatches: %v", err)
	}
	if got {
		t.Error("missing namespace snapshot must not match")
	}
	if reason != ReasonSnapshotMissing {
		t.Errorf("reason = %q, want ReasonSnapshotMissing", reason)
	}
}
