package replay_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/replay"
)

// goldenCase 是一条求值语义的固化用例。
//
// 本文件对应 spec §6.2 的精确求值层清单：该层要求接近 100% 正确，
// 靠人工 review 保证不了，每条语义规则都必须在此固化。
type goldenCase struct {
	name          string
	policies      []networkingv1.NetworkPolicy
	flow          replay.Flow
	opts          []replay.Option
	wantVerdict   replay.Verdict
	wantConf      replay.Confidence
	wantUnknown   replay.UnknownReason
	wantCrossClus bool
}

func npIngress(ns, name string, sel map[string]string, rules []networkingv1.NetworkPolicyIngressRule) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: sel},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     rules,
		},
	}
}

func npEgress(ns, name string, sel map[string]string, rules []networkingv1.NetworkPolicyEgressRule) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: sel},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      rules,
		},
	}
}

func tcpPort(p int32) []networkingv1.NetworkPolicyPort {
	v := intstr.FromInt32(p)
	proto := corev1.ProtocolTCP
	return []networkingv1.NetworkPolicyPort{{Port: &v, Protocol: &proto}}
}

func TestGoldenEvaluationSemantics(t *testing.T) {
	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	worker := pod("payment", "worker-1", "10.4.0.2", map[string]string{"app": "worker"})

	apiWithNamedPort := api
	apiWithNamedPort.NamedPorts = []replay.NamedPort{
		{Name: "http", Port: 8080, Protocol: replay.ProtocolTCP},
	}

	external := replay.Endpoint{IP: "8.8.8.8"}

	cases := []goldenCase{
		{
			name:        "no policy allows everything",
			flow:        flowBetween(gw, api, 8080),
			wantVerdict: replay.VerdictAllow,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name:        "empty pod selector isolates the whole namespace",
			policies:    []networkingv1.NetworkPolicy{npIngress("payment", "deny", nil, nil)},
			flow:        flowBetween(gw, api, 8080),
			wantVerdict: replay.VerdictDeny,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name:        "pod not selected by any policy stays open",
			policies:    []networkingv1.NetworkPolicy{npIngress("payment", "deny-api", map[string]string{"app": "api"}, nil)},
			flow:        flowBetween(gw, worker, 8080),
			wantVerdict: replay.VerdictAllow,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name: "namespace selector allows the peer",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				npIngress("payment", "allow-edge", nil, []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
					}},
				}}),
			},
			flow:        flowBetween(gw, api, 8080),
			wantVerdict: replay.VerdictAllow,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name: "pod selector alone does not cross namespaces",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				npIngress("payment", "allow-local", nil, []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
					}},
				}}),
			},
			flow:        flowBetween(gw, api, 8080),
			wantVerdict: replay.VerdictDeny,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name: "port outside the rule is denied",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				npIngress("payment", "allow-8080", nil, []networkingv1.NetworkPolicyIngressRule{{
					Ports: tcpPort(8080),
				}}),
			},
			flow:        flowBetween(gw, api, 9090),
			wantVerdict: replay.VerdictDeny,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name: "ip block allows external egress",
			policies: []networkingv1.NetworkPolicy{
				npEgress("gateway", "deny", nil, nil),
				npEgress("gateway", "allow-dns-range", nil, []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "8.8.8.0/24"},
					}},
				}}),
			},
			flow: replay.Flow{
				Source: ep(gw), Dest: external, Protocol: replay.ProtocolTCP, Port: 443,
			},
			wantVerdict: replay.VerdictAllow,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name: "ip block except overrides the cidr",
			policies: []networkingv1.NetworkPolicy{
				npEgress("gateway", "deny", nil, nil),
				npEgress("gateway", "allow-public-except", nil, []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"8.8.8.0/24"}},
					}},
				}}),
			},
			flow: replay.Flow{
				Source: ep(gw), Dest: external, Protocol: replay.ProtocolTCP, Port: 443,
			},
			wantVerdict: replay.VerdictDeny,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name: "named port resolves against the destination pod",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				func() networkingv1.NetworkPolicy {
					name := intstr.FromString("http")
					proto := corev1.ProtocolTCP
					return npIngress("payment", "allow-http", nil, []networkingv1.NetworkPolicyIngressRule{{
						Ports: []networkingv1.NetworkPolicyPort{{Port: &name, Protocol: &proto}},
					}})
				}(),
			},
			flow:        flowBetween(gw, apiWithNamedPort, 8080),
			wantVerdict: replay.VerdictAllow,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name: "unresolvable named port yields unknown",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				func() networkingv1.NetworkPolicy {
					name := intstr.FromString("http")
					proto := corev1.ProtocolTCP
					return npIngress("payment", "allow-http", nil, []networkingv1.NetworkPolicyIngressRule{{
						Ports: []networkingv1.NetworkPolicyPort{{Port: &name, Protocol: &proto}},
					}})
				}(),
			},
			flow:        flowBetween(gw, api, 8080),
			wantVerdict: replay.VerdictUnknown,
			wantConf:    replay.ConfidenceTrusted,
			wantUnknown: replay.ReasonNamedPortUnresolved,
		},
		{
			name: "egress deny blocks even when ingress allows",
			policies: []networkingv1.NetworkPolicy{
				npEgress("gateway", "deny-egress", nil, nil),
				npIngress("payment", "allow-all", nil, []networkingv1.NetworkPolicyIngressRule{{}}),
			},
			flow:        flowBetween(gw, api, 8080),
			wantVerdict: replay.VerdictDeny,
			wantConf:    replay.ConfidenceTrusted,
		},
		{
			name:     "ccnp presence degrades the verdict",
			policies: nil,
			flow:     flowBetween(gw, api, 8080),
			opts:     []replay.Option{replay.WithCCNPPresent(true)},

			wantVerdict: replay.VerdictAllow,
			wantConf:    replay.ConfidenceDegraded,
		},
		{
			name:          "cross cluster peer is denied and flagged",
			policies:      []networkingv1.NetworkPolicy{npIngress("payment", "deny", nil, nil)},
			flow:          flowBetween(remotePod("gateway", "gw-1", "172.16.0.9"), api, 8080),
			wantVerdict:   replay.VerdictDeny,
			wantConf:      replay.ConfidenceTrusted,
			wantCrossClus: true,
		},
		{
			name:        "host network destination is out of scope",
			policies:    []networkingv1.NetworkPolicy{npIngress("kube-system", "deny", nil, nil)},
			flow:        flowBetween(api, hostNetworkPod("kube-system", "agent", "192.168.1.7"), 9100),
			wantVerdict: replay.VerdictAllow,
			wantConf:    replay.ConfidenceTrusted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := replay.NewEvaluator(testCluster, tc.policies, namespaces(), tc.opts...)
			got := e.Evaluate(tc.flow)

			if got.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %q, want %q", got.Verdict, tc.wantVerdict)
			}
			if got.Confidence != tc.wantConf {
				t.Errorf("Confidence = %q, want %q", got.Confidence, tc.wantConf)
			}
			if got.UnknownReason != tc.wantUnknown {
				t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, tc.wantUnknown)
			}
			if got.CrossCluster != tc.wantCrossClus {
				t.Errorf("CrossCluster = %v, want %v", got.CrossCluster, tc.wantCrossClus)
			}
			if !got.UnknownReason.Valid() {
				t.Errorf("UnknownReason %q is not a registered enum value", got.UnknownReason)
			}
		})
	}
}

// 每条判定都必须可解释：判定解释器是"平台可信"这件事的唯一落地形式。
func TestGoldenDecisionsAreExplainable(t *testing.T) {
	e := replay.NewEvaluator(testCluster,
		[]networkingv1.NetworkPolicy{npIngress("payment", "deny", nil, nil)}, namespaces())

	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(gw, api, 8080))
	if got.Reason.Direction == "" {
		t.Error("Reason.Direction must be set on every decision")
	}
	if got.Verdict == replay.VerdictDeny && !got.Reason.Isolated {
		t.Error("a DENY must record why: isolation or an unmatched rule")
	}
}
