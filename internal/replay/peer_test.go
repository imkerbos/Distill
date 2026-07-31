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
			got, err := peerSelectorMatches(tt.peer, "payment", tt.ep, namespaces)
			if err != nil {
				t.Fatalf("peerSelectorMatches: %v", err)
			}
			if got != tt.want {
				t.Errorf("peerSelectorMatches = %v, want %v", got, tt.want)
			}
		})
	}
}

// 身份未还原的端点无法被 selector 匹配 —— 只有 ipBlock 能覆盖它。
func TestPeerSelectorMatchesRequiresIdentity(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	ep := Endpoint{IP: "8.8.8.8"}

	got, err := peerSelectorMatches(peer, "payment", ep, nil)
	if err != nil {
		t.Fatalf("peerSelectorMatches: %v", err)
	}
	if got {
		t.Error("endpoint without resolved identity must not match a selector-based peer")
	}
}

// 命名空间快照缺失时不能猜：猜错会放行本应阻断的流量。
func TestPeerSelectorMatchesMissingNamespaceSnapshot(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{}}
	pod := PodRef{ClusterID: "c1", Namespace: "unknown-ns", Name: "x"}

	got, err := peerSelectorMatches(peer, "payment", endpointOf(pod), nsIndex())
	if err != nil {
		t.Fatalf("peerSelectorMatches: %v", err)
	}
	if got {
		t.Error("missing namespace snapshot must not match; callers surface SNAPSHOT_MISSING")
	}
}
