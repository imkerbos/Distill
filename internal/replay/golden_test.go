package replay_test

import (
	"strings"
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
// wantMatchedPolicy / wantMatchedRuleIdx 固化 §6.6：ALLOW 必须说得出命中
// 哪条 policy 的第几条 rule，其余结论必须明确"没有命中"。这两项每条用例
// 都要填 —— 只在部分用例上断言，等于把解释器的输出留在无人看管的状态，
// 而它是"平台可信"这件事唯一的落地形式。未命中一律填 -1。
type goldenCase struct {
	name               string
	policies           []networkingv1.NetworkPolicy
	flow               replay.Flow
	opts               []replay.Option
	wantVerdict        replay.Verdict
	wantConf           replay.Confidence
	wantUnknown        replay.UnknownReason
	wantCrossClus      bool
	wantMatchedPolicy  string
	wantMatchedRuleIdx int
	// wantDetailContains, when set, pins a substring Reason.Detail must
	// contain. Left empty on cases where Detail is not part of what this
	// row is fixing in place, so it opts out of the check rather than
	// asserting an empty string.
	wantDetailContains string
}

// assertUnknownReasonInvariant 固化 verdict 与 unknown_reason 的互相绑定：
// UNKNOWN 必须给出封闭枚举里的原因（§6.5 要统计 UNKNOWN 构成），而给出了
// 原因却不是 UNKNOWN 同样是缺陷 —— 那是一条"带着未决原因的确定结论"，
// 会被下游当作可信答案使用。
func assertUnknownReasonInvariant(t *testing.T, d replay.Decision) {
	t.Helper()
	if !d.UnknownReason.Valid() {
		t.Errorf("UnknownReason %q is not a registered enum value", d.UnknownReason)
	}
	if (d.Verdict == replay.VerdictUnknown) != (d.UnknownReason != replay.ReasonNone) {
		t.Errorf("Verdict = %q with UnknownReason = %q; UNKNOWN and a reason must imply each other",
			d.Verdict, d.UnknownReason)
	}
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
			name:               "no policy allows everything",
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name:               "empty pod selector isolates the whole namespace",
			policies:           []networkingv1.NetworkPolicy{npIngress("payment", "deny", nil, nil)},
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictDeny,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name:               "pod not selected by any policy stays open",
			policies:           []networkingv1.NetworkPolicy{npIngress("payment", "deny-api", map[string]string{"app": "api"}, nil)},
			flow:               flowBetween(gw, worker, 8080),
			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
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
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "payment/allow-edge",
			wantMatchedRuleIdx: 0,
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
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictDeny,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "port outside the rule is denied",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				npIngress("payment", "allow-8080", nil, []networkingv1.NetworkPolicyIngressRule{{
					Ports: tcpPort(8080),
				}}),
			},
			flow:               flowBetween(gw, api, 9090),
			wantVerdict:        replay.VerdictDeny,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
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
			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "gateway/allow-dns-range",
			wantMatchedRuleIdx: 0,
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
			wantVerdict:        replay.VerdictDeny,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
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
			flow:               flowBetween(gw, apiWithNamedPort, 8080),
			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "payment/allow-http",
			wantMatchedRuleIdx: 0,
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
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictUnknown,
			wantConf:           replay.ConfidenceTrusted,
			wantUnknown:        replay.ReasonNamedPortUnresolved,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "egress deny blocks even when ingress allows",
			policies: []networkingv1.NetworkPolicy{
				npEgress("gateway", "deny-egress", nil, nil),
				npIngress("payment", "allow-all", nil, []networkingv1.NetworkPolicyIngressRule{{}}),
			},
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictDeny,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name:     "ccnp presence degrades the verdict",
			policies: nil,
			flow:     flowBetween(gw, api, 8080),
			opts:     []replay.Option{replay.WithForeignPlane(true)},

			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceDegraded,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name:               "cross cluster peer is denied and flagged",
			policies:           []networkingv1.NetworkPolicy{npIngress("payment", "deny", nil, nil)},
			flow:               flowBetween(remotePod("gateway", "gw-1", "172.16.0.9"), api, 8080),
			wantVerdict:        replay.VerdictDeny,
			wantConf:           replay.ConfidenceTrusted,
			wantCrossClus:      true,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name:               "host network destination is out of scope",
			policies:           []networkingv1.NetworkPolicy{npIngress("kube-system", "deny", nil, nil)},
			flow:               flowBetween(api, hostNetworkPod("kube-system", "agent", "192.168.1.7"), 9100),
			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		// 候选放行策略的 PodSelector 无法解析时不能被静默当作"没选中"：
		// 那会让一条本可放行的策略凭空消失，最终吐出一个可信的 DENY——
		// 建立在无法解析的策略之上的可信结论，是本引擎最危险的输出。
		{
			name: "malformed candidate policy selector yields unknown, not a silent deny",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				func() networkingv1.NetworkPolicy {
					p := npIngress("payment", "allow-broken-selector", nil, []networkingv1.NetworkPolicyIngressRule{{}})
					p.Spec.PodSelector = metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "app", Operator: "BogusOperator", Values: []string{"x"}},
						},
					}
					return p
				}(),
			},
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictUnknown,
			wantConf:           replay.ConfidenceTrusted,
			wantUnknown:        replay.ReasonPolicyMalformed,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		// 同一个缺陷类别，触发点是隔离判定本身：唯一一条策略的 PodSelector
		// 就无法解析，连"主体是否被隔离"都判不出来。结论必须仍是
		// UNKNOWN/POLICY_MALFORMED，而不是把这次求值失败误归类成快照缺失。
		// 顺序无关性由 TestEvaluateMalformedIsolatingPolicyIsOrderIndependent 固化。
		{
			name: "malformed isolating policy selector yields unknown",
			policies: []networkingv1.NetworkPolicy{
				func() networkingv1.NetworkPolicy {
					p := npIngress("payment", "broken-selector", nil, nil)
					p.Spec.PodSelector = metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "app", Operator: "BogusOperator", Values: []string{"x"}},
						},
					}
					return p
				}(),
			},
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictUnknown,
			wantConf:           replay.ConfidenceTrusted,
			wantUnknown:        replay.ReasonPolicyMalformed,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		// 同一个缺陷类别，触发点在规则级而非 selectsPod：ipBlock 的 CIDR
		// 写错了。这是三种触发点里在真实数据集中最常见的一种（一次 YAML
		// 笔误），Detail 必须像 podSelector 那条一样指名是哪条策略、哪个
		// 规则下标出的错，否则解释器对着这条 UNKNOWN 只能交白卷。
		{
			name: "malformed rule-level ipblock yields unknown with a named detail",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "bad-cidr", nil, []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "not-a-cidr"},
					}},
				}}),
			},
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictUnknown,
			wantConf:           replay.ConfidenceTrusted,
			wantUnknown:        replay.ReasonPolicyMalformed,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
			wantDetailContains: "payment/bad-cidr",
		},
		// namespaceSelector 求值需要查快照里对端 Pod 所在命名空间的标签。
		// 当那个命名空间不在快照索引里时，静默判 false 会把一条本可能
		// 放行的流量变成一个可信的 DENY——namespaces() 里的所有用例都
		// 恰好覆盖了它们引用的命名空间，这条用例故意用一个不在其中的
		// 命名空间，才能把这条路径暴露出来。
		{
			name: "missing namespace snapshot yields unknown, not a silent deny",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "deny", nil, nil),
				npIngress("payment", "allow-edge", nil, []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
					}},
				}}),
			},
			flow:               flowBetween(pod("unlisted-ns", "src-1", "10.4.0.50", map[string]string{"app": "src"}), api, 8080),
			wantVerdict:        replay.VerdictUnknown,
			wantConf:           replay.ConfidenceTrusted,
			wantUnknown:        replay.ReasonSnapshotMissing,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		// 出向命中了策略、入向只是没被任何策略选中。入向那侧没有任何可
		// 解释的内容，不能反过来把出向真正的放行理由抹掉 —— §6.6 要求
		// ALLOW 说得出命中哪条 policy 的第几条 rule，抹掉后解释器只剩空白。
		{
			name: "egress match survives an unselected destination",
			policies: []networkingv1.NetworkPolicy{
				npEgress("gateway", "deny-egress", nil, nil),
				npEgress("gateway", "egress-allow", nil, []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
					}},
				}}),
			},
			flow:               flowBetween(gw, api, 8080),
			wantVerdict:        replay.VerdictAllow,
			wantConf:           replay.ConfidenceTrusted,
			wantMatchedPolicy:  "gateway/egress-allow",
			wantMatchedRuleIdx: 0,
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
			if got.Reason.MatchedPolicy != tc.wantMatchedPolicy {
				t.Errorf("Reason.MatchedPolicy = %q, want %q", got.Reason.MatchedPolicy, tc.wantMatchedPolicy)
			}
			if got.Reason.MatchedRuleIdx != tc.wantMatchedRuleIdx {
				t.Errorf("Reason.MatchedRuleIdx = %d, want %d", got.Reason.MatchedRuleIdx, tc.wantMatchedRuleIdx)
			}
			if tc.wantDetailContains != "" && !strings.Contains(got.Reason.Detail, tc.wantDetailContains) {
				t.Errorf("Reason.Detail = %q, want it to contain %q", got.Reason.Detail, tc.wantDetailContains)
			}
			assertUnknownReasonInvariant(t, got)
		})
	}
}

// UNKNOWN 与 unknown_reason 必须互为充要条件，跨一组有代表性的判定验证。
//
// 顺带记录一条设计事实：本层能产出的原因只有 SNAPSHOT_MISSING、
// NAMED_PORT_UNRESOLVED、POLICY_MALFORMED 三种。IDENTITY_LOST_MESH 与
// CCNP_PRESENT 按 §6.4 走 confidence=DEGRADED 通道，仍然给出结论，
// 因此永远不会出现在 UnknownReason 上 —— 下面的 mesh 与 CCNP 用例正是
// 对这条不变式的断言：它们必须是带 DEGRADED 的确定结论，而不是 UNKNOWN。
func TestDecisionUnknownReasonInvariant(t *testing.T) {
	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	mesh := func() replay.PodRef {
		p := pod("gateway", "gw-2", "10.4.0.8", map[string]string{"app": "gateway"})
		p.InMesh = true
		return p
	}()

	named := intstr.FromString("http")
	tcp := corev1.ProtocolTCP

	decisions := []struct {
		name     string
		policies []networkingv1.NetworkPolicy
		flow     replay.Flow
		opts     []replay.Option
	}{
		{name: "open allow", flow: flowBetween(gw, api, 8080)},
		{
			name:     "default deny",
			policies: []networkingv1.NetworkPolicy{denyAllIngress("payment")},
			flow:     flowBetween(gw, api, 8080),
		},
		{
			name: "additive allow",
			policies: []networkingv1.NetworkPolicy{
				denyAllIngress("payment"), allowFromGateway("payment", 8080),
			},
			flow: flowBetween(gw, api, 8080),
		},
		{
			name: "named port unresolved",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "allow-named", nil, []networkingv1.NetworkPolicyIngressRule{{
					Ports: []networkingv1.NetworkPolicyPort{{Port: &named, Protocol: &tcp}},
				}}),
			},
			flow: flowBetween(gw, api, 8080),
		},
		{
			name: "malformed ipblock",
			policies: []networkingv1.NetworkPolicy{
				npIngress("payment", "bad-cidr", nil, []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "not-a-cidr"},
					}},
				}}),
			},
			flow: flowBetween(gw, api, 8080),
		},
		{
			name:     "unresolved local endpoint",
			policies: []networkingv1.NetworkPolicy{denyAllIngress("payment")},
			flow: replay.Flow{
				Source:   replay.Endpoint{ClusterID: testCluster, IP: "10.4.0.99"},
				Dest:     ep(api),
				Protocol: replay.ProtocolTCP, Port: 8080,
			},
		},
		{
			name:     "mesh endpoint stays a verdict",
			policies: []networkingv1.NetworkPolicy{denyAllIngress("payment")},
			flow:     flowBetween(mesh, api, 8080),
		},
		{
			name:     "ccnp present stays a verdict",
			policies: []networkingv1.NetworkPolicy{denyAllIngress("payment")},
			flow:     flowBetween(gw, api, 8080),
			opts:     []replay.Option{replay.WithForeignPlane(true)},
		},
		{
			name:     "cross cluster deny",
			policies: []networkingv1.NetworkPolicy{denyAllIngress("payment")},
			flow:     flowBetween(remotePod("gateway", "gw-1", "172.16.0.9"), api, 8080),
		},
		{
			name:     "host network endpoint",
			policies: []networkingv1.NetworkPolicy{npIngress("kube-system", "deny", nil, nil)},
			flow:     flowBetween(api, hostNetworkPod("kube-system", "agent", "192.168.1.7"), 9100),
		},
	}

	for _, tc := range decisions {
		t.Run(tc.name, func(t *testing.T) {
			got := replay.NewEvaluator(testCluster, tc.policies, namespaces(), tc.opts...).Evaluate(tc.flow)
			assertUnknownReasonInvariant(t, got)

			switch got.UnknownReason {
			case replay.ReasonIdentityLostMesh, replay.ReasonCCNPPresent:
				t.Errorf("UnknownReason = %q; mesh and CCNP route through confidence=DEGRADED (§6.4) and must still yield a verdict",
					got.UnknownReason)
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
