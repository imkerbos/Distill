package replay_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	npav1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"

	"github.com/imkerbos/Distill/internal/replay"
)

/*
管理面策略（ANP / BANP）求值的固化用例。

与 golden_test.go 同一条纪律，理由更重：这一族**带 Deny 且排在标准
NetworkPolicy 之前**，判错的方向是把一条其实被拦住的连接判成放行 ——
而那正是会被写进一条放行建议、下发之后才断的方向。

求值次序（依据 CRD 里 action 字段的原文）：

	ANP（priority 升序，同策略内按规则序）
	  ├ Allow → 终局放行，压过 NetworkPolicy
	  ├ Deny  → 终局阻断
	  └ Pass  → 跳过剩余 ANP 规则，交给下一段
	NetworkPolicy（照旧）
	BANP —— **只在主体没被任何 NetworkPolicy 选中时**才轮到
*/

// --- 夹具 --------------------------------------------------------------

func nsSel(labels map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: labels}
}

// anp 造一条 AdminNetworkPolicy。subject 按命名空间标签选。
func anp(name string, priority int32, subjectNS map[string]string,
	ingress []npav1.AdminNetworkPolicyIngressRule,
	egress []npav1.AdminNetworkPolicyEgressRule,
) npav1.AdminNetworkPolicy {
	return npav1.AdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: npav1.AdminNetworkPolicySpec{
			Priority: priority,
			Subject:  npav1.AdminNetworkPolicySubject{Namespaces: nsSel(subjectNS)},
			Ingress:  ingress,
			Egress:   egress,
		},
	}
}

// anpIngress 造一条入向规则：从带 fromNS 标签的命名空间来。
func anpIngress(name string, action npav1.AdminNetworkPolicyRuleAction,
	fromNS map[string]string, ports *[]npav1.AdminNetworkPolicyPort,
) npav1.AdminNetworkPolicyIngressRule {
	return npav1.AdminNetworkPolicyIngressRule{
		Name: name, Action: action, Ports: ports,
		From: []npav1.AdminNetworkPolicyIngressPeer{{Namespaces: nsSel(fromNS)}},
	}
}

func anpEgressTo(name string, action npav1.AdminNetworkPolicyRuleAction,
	peer npav1.AdminNetworkPolicyEgressPeer,
) npav1.AdminNetworkPolicyEgressRule {
	return npav1.AdminNetworkPolicyEgressRule{
		Name: name, Action: action, To: []npav1.AdminNetworkPolicyEgressPeer{peer},
	}
}

func anpPorts(p ...npav1.AdminNetworkPolicyPort) *[]npav1.AdminNetworkPolicyPort {
	out := append([]npav1.AdminNetworkPolicyPort(nil), p...)
	return &out
}

func anpTCP(port int32) npav1.AdminNetworkPolicyPort {
	return npav1.AdminNetworkPolicyPort{
		PortNumber: &npav1.Port{Protocol: corev1.ProtocolTCP, Port: port},
	}
}

// banp 造集群里那唯一一条 BaselineAdminNetworkPolicy。
func banp(subjectNS map[string]string,
	ingress []npav1.BaselineAdminNetworkPolicyIngressRule,
) *npav1.BaselineAdminNetworkPolicy {
	return &npav1.BaselineAdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: npav1.BaselineAdminNetworkPolicySpec{
			Subject: npav1.AdminNetworkPolicySubject{Namespaces: nsSel(subjectNS)},
			Ingress: ingress,
		},
	}
}

func banpIngress(name string, action npav1.BaselineAdminNetworkPolicyRuleAction,
	fromNS map[string]string,
) npav1.BaselineAdminNetworkPolicyIngressRule {
	return npav1.BaselineAdminNetworkPolicyIngressRule{
		Name: name, Action: action,
		From: []npav1.AdminNetworkPolicyIngressPeer{{Namespaces: nsSel(fromNS)}},
	}
}

// --- 用例 --------------------------------------------------------------

type adminGoldenCase struct {
	name               string
	policies           []networkingv1.NetworkPolicy
	anps               []npav1.AdminNetworkPolicy
	banp               *npav1.BaselineAdminNetworkPolicy
	wantVerdict        replay.Verdict
	wantUnknown        replay.UnknownReason
	wantMatchedPolicy  string
	wantMatchedRuleIdx int
	wantDetailContains string
}

func TestGoldenAdminPolicySemantics(t *testing.T) {
	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	prod := map[string]string{"env": "prod"}
	edge := map[string]string{"role": "edge"}

	denyAll := npIngress("payment", "deny-all", nil, nil)
	allowGw := npIngress("payment", "allow-gw", nil, []networkingv1.NetworkPolicyIngressRule{{
		From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: nsSel(edge)}},
		Ports: tcpPort(8080),
	}})

	cases := []adminGoldenCase{
		{
			name: "ANP Allow 压过 NetworkPolicy 的 default-deny",
			// 这是 ANP 存在的理由：它能放行一条被命名空间自己的策略拦住的连接。
			// 判错方向是把它读成 DENY —— 平台会去建议一条其实多余的放行规则。
			policies:           []networkingv1.NetworkPolicy{denyAll},
			anps:               []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("allow-edge", npav1.AdminNetworkPolicyRuleActionAllow, edge, nil)}, nil)},
			wantVerdict:        replay.VerdictAllow,
			wantMatchedPolicy:  "anp/a",
			wantMatchedRuleIdx: 0,
			wantDetailContains: "action Allow",
		},
		{
			name: "ANP Deny 压过 NetworkPolicy 的放行",
			// 反方向，也是最危险的那一个：只看 NetworkPolicy 会得到一个可信的
			// ALLOW，而集群其实拦着。
			policies:           []networkingv1.NetworkPolicy{allowGw},
			anps:               []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("deny-edge", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil)}, nil)},
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "anp/a",
			wantMatchedRuleIdx: 0,
			wantDetailContains: "action Deny",
		},
		{
			name: "priority 小的先生效",
			// 同时命中两条 ANP：20 那条 Deny，10 那条 Allow，结论应当是 Allow。
			anps: []npav1.AdminNetworkPolicy{
				anp("later", 20, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("deny", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil)}, nil),
				anp("earlier", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("allow", npav1.AdminNetworkPolicyRuleActionAllow, edge, nil)}, nil),
			},
			wantVerdict:        replay.VerdictAllow,
			wantMatchedPolicy:  "anp/earlier",
			wantMatchedRuleIdx: 0,
		},
		{
			name: "同一策略内按规则顺序，第一条命中的定局",
			anps: []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{
				anpIngress("deny-first", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil),
				anpIngress("allow-second", npav1.AdminNetworkPolicyRuleActionAllow, edge, nil),
			}, nil)},
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "anp/a",
			wantMatchedRuleIdx: 0,
		},
		{
			name: "Pass 交给 NetworkPolicy，NetworkPolicy 说了算",
			// Pass 之后 default-deny 生效，结论是 DENY —— 而 Reason 必须落在
			// 那条 NetworkPolicy 上，不是落在 ANP 上：结论是它做的。
			policies:           []networkingv1.NetworkPolicy{denyAll},
			anps:               []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("pass", npav1.AdminNetworkPolicyRuleActionPass, edge, nil)}, nil)},
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "Pass 之后跳过剩余 ANP 规则",
			// 第二条 Deny 与第一条 Pass 命中同一条流量。Pass 让它不再被看，
			// 于是结论由 NetworkPolicy 段给出（这里没有策略 → 放行）。
			// 漏掉"跳过剩余规则"会让结论变成 DENY。
			anps: []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{
				anpIngress("pass", npav1.AdminNetworkPolicyRuleActionPass, edge, nil),
				anpIngress("deny", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil),
			}, nil)},
			wantVerdict:        replay.VerdictAllow,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "Pass 之后主体没被 NetworkPolicy 选中，仍然轮到 BANP",
			// **这一条固化的是先前想错的那个点**：曾以为 Pass 会连 BANP 一起
			// 跳过。按那个错版本，这里会是 ALLOW；按 API 原文，BANP 的 Deny
			// 生效，是 DENY。方向正是"把被拦住的连接判成放行"。
			anps:               []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("pass", npav1.AdminNetworkPolicyRuleActionPass, edge, nil)}, nil)},
			banp:               banp(prod, []npav1.BaselineAdminNetworkPolicyIngressRule{banpIngress("deny", npav1.BaselineAdminNetworkPolicyRuleActionDeny, edge)}),
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "banp/default",
			wantMatchedRuleIdx: 0,
			wantDetailContains: "BaselineAdminNetworkPolicy",
		},
		{
			name:               "一条 ANP 都没命中时同样轮到 BANP",
			banp:               banp(prod, []npav1.BaselineAdminNetworkPolicyIngressRule{banpIngress("deny", npav1.BaselineAdminNetworkPolicyRuleActionDeny, edge)}),
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "banp/default",
			wantMatchedRuleIdx: 0,
		},
		{
			name: "主体被 NetworkPolicy 选中时，BANP 不再有机会改判",
			// denyAll 选中了主体、没有规则放行 → DENY。BANP 的 Allow 不该
			// 把它翻成 ALLOW，否则一份 default-deny 会被兜底策略架空。
			policies:           []networkingv1.NetworkPolicy{denyAll},
			banp:               banp(prod, []npav1.BaselineAdminNetworkPolicyIngressRule{banpIngress("allow", npav1.BaselineAdminNetworkPolicyRuleActionAllow, edge)}),
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "BANP 没命中则默认放行",
			banp: banp(prod, []npav1.BaselineAdminNetworkPolicyIngressRule{
				banpIngress("deny-other", npav1.BaselineAdminNetworkPolicyRuleActionDeny, map[string]string{"role": "batch"}),
			}),
			wantVerdict:        replay.VerdictAllow,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "端口不匹配的 ANP 规则不命中",
			anps: []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{
				anpIngress("deny-9090", npav1.AdminNetworkPolicyRuleActionDeny, edge, anpPorts(anpTCP(9090))),
			}, nil)},
			wantVerdict:        replay.VerdictAllow,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "端口范围命中",
			anps: []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{
				{
					Name: "deny-range", Action: npav1.AdminNetworkPolicyRuleActionDeny,
					From:  []npav1.AdminNetworkPolicyIngressPeer{{Namespaces: nsSel(edge)}},
					Ports: anpPorts(npav1.AdminNetworkPolicyPort{PortRange: &npav1.PortRange{Protocol: corev1.ProtocolTCP, Start: 8000, End: 8100}}),
				},
			}, nil)},
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "anp/a",
			wantMatchedRuleIdx: 0,
		},
		{
			name: "subject 选不中的 ANP 完全不参与",
			anps: []npav1.AdminNetworkPolicy{anp("a", 10, map[string]string{"env": "staging"}, []npav1.AdminNetworkPolicyIngressRule{
				anpIngress("deny", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil),
			}, nil)},
			wantVerdict:        replay.VerdictAllow,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "两条同优先级的 ANP 同时选中主体 → UNKNOWN",
			// API 原文说这时行为未定义。平台不替它挑一个：挑中的那一半时间
			// 会给出一个自信的错答案。
			anps: []npav1.AdminNetworkPolicy{
				anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("allow", npav1.AdminNetworkPolicyRuleActionAllow, edge, nil)}, nil),
				anp("b", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("deny", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil)}, nil),
			},
			wantVerdict:        replay.VerdictUnknown,
			wantUnknown:        replay.ReasonAdminPriorityAmbiguous,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
		},
		{
			name: "同优先级但只有一条选中主体 → 照常求值",
			// 不因为集群里存在同优先级的策略就整片降级。
			anps: []npav1.AdminNetworkPolicy{
				anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{anpIngress("deny", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil)}, nil),
				anp("b", 10, map[string]string{"env": "staging"}, []npav1.AdminNetworkPolicyIngressRule{anpIngress("allow", npav1.AdminNetworkPolicyRuleActionAllow, edge, nil)}, nil),
			},
			wantVerdict:        replay.VerdictDeny,
			wantMatchedPolicy:  "anp/a",
			wantMatchedRuleIdx: 0,
		},
		{
			name: "认不出的 action 不能被当成不命中",
			// 当成不命中会把一条本该定局的规则读没了，后面任何一条 Allow
			// 都会变成终局结论。
			anps: []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{
				anpIngress("weird", npav1.AdminNetworkPolicyRuleAction("Reject"), edge, nil),
			}, nil)},
			wantVerdict:        replay.VerdictUnknown,
			wantUnknown:        replay.ReasonPolicyMalformed,
			wantMatchedPolicy:  "anp/a",
			wantMatchedRuleIdx: 0,
			wantDetailContains: "unknown action",
		},
		{
			name: "subject 两个字段都给 → 报错而不是猜",
			anps: []npav1.AdminNetworkPolicy{{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
				Spec: npav1.AdminNetworkPolicySpec{
					Priority: 10,
					Subject: npav1.AdminNetworkPolicySubject{
						Namespaces: nsSel(prod),
						Pods:       &npav1.NamespacedPod{},
					},
					Ingress: []npav1.AdminNetworkPolicyIngressRule{anpIngress("deny", npav1.AdminNetworkPolicyRuleActionDeny, edge, nil)},
				},
			}},
			wantVerdict:        replay.VerdictUnknown,
			wantUnknown:        replay.ReasonPolicyMalformed,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
			wantDetailContains: "exactly one",
		},
		{
			name: "空 peer 列表是非法的，不是「任意对端」",
			// NetworkPolicy 里空 peer 表示任意对端。照那个读法，这条规则会
			// 匹配一切 —— 而它的动作是 Deny。
			anps: []npav1.AdminNetworkPolicy{anp("a", 10, prod, []npav1.AdminNetworkPolicyIngressRule{
				{Name: "no-peers", Action: npav1.AdminNetworkPolicyRuleActionDeny},
			}, nil)},
			wantVerdict:        replay.VerdictUnknown,
			wantUnknown:        replay.ReasonPolicyMalformed,
			wantMatchedPolicy:  "",
			wantMatchedRuleIdx: -1,
			wantDetailContains: "at least one",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []replay.Option{replay.WithAdminPolicies(tc.anps, tc.banp)}
			got := replay.NewEvaluator(testCluster, tc.policies, namespaces(), opts...).
				Evaluate(flowBetween(gw, api, 8080))

			assertUnknownReasonInvariant(t, got)
			if got.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %q, want %q (reason %+v)", got.Verdict, tc.wantVerdict, got.Reason)
			}
			if got.UnknownReason != tc.wantUnknown {
				t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, tc.wantUnknown)
			}
			if got.Reason.MatchedPolicy != tc.wantMatchedPolicy {
				t.Errorf("MatchedPolicy = %q, want %q", got.Reason.MatchedPolicy, tc.wantMatchedPolicy)
			}
			if got.Reason.MatchedRuleIdx != tc.wantMatchedRuleIdx {
				t.Errorf("MatchedRuleIdx = %d, want %d", got.Reason.MatchedRuleIdx, tc.wantMatchedRuleIdx)
			}
			if tc.wantDetailContains != "" && !strings.Contains(got.Reason.Detail, tc.wantDetailContains) {
				t.Errorf("Detail = %q, want it to contain %q", got.Reason.Detail, tc.wantDetailContains)
			}
		})
	}
}

// egress 的 networks 与 nodes 两种 peer 只有出向才有，单独一组。
func TestGoldenAdminPolicyEgressPeers(t *testing.T) {
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	external := replay.Endpoint{IP: "169.254.169.254"}
	prod := map[string]string{"env": "prod"}
	flow := replay.Flow{
		Source: replay.Endpoint{ClusterID: testCluster, IP: api.IP, Pod: &api},
		Dest:   external, Protocol: replay.ProtocolTCP, Port: 80,
	}

	t.Run("networks 命中则 Deny", func(t *testing.T) {
		// 拦元数据服务是这个 peer 最常见的用途，也是一条真的会救命的规则。
		p := anp("a", 10, prod, nil, []npav1.AdminNetworkPolicyEgressRule{
			anpEgressTo("deny-metadata", npav1.AdminNetworkPolicyRuleActionDeny,
				npav1.AdminNetworkPolicyEgressPeer{Networks: []npav1.CIDR{"169.254.169.254/32"}}),
		})
		got := replay.NewEvaluator(testCluster, nil, namespaces(),
			replay.WithAdminPolicies([]npav1.AdminNetworkPolicy{p}, nil)).Evaluate(flow)
		if got.Verdict != replay.VerdictDeny {
			t.Errorf("Verdict = %q, want DENY (reason %+v)", got.Verdict, got.Reason)
		}
	})

	t.Run("networks 不含该地址则不命中", func(t *testing.T) {
		p := anp("a", 10, prod, nil, []npav1.AdminNetworkPolicyEgressRule{
			anpEgressTo("deny-other", npav1.AdminNetworkPolicyRuleActionDeny,
				npav1.AdminNetworkPolicyEgressPeer{Networks: []npav1.CIDR{"10.0.0.0/8"}}),
		})
		got := replay.NewEvaluator(testCluster, nil, namespaces(),
			replay.WithAdminPolicies([]npav1.AdminNetworkPolicy{p}, nil)).Evaluate(flow)
		if got.Verdict != replay.VerdictAllow {
			t.Errorf("Verdict = %q, want ALLOW", got.Verdict)
		}
	})

	t.Run("nodes peer 平台还解释不了 → UNKNOWN 而不是不命中", func(t *testing.T) {
		// 当成"不命中"会让一条拦到节点流量的 Deny 静默消失。
		p := anp("a", 10, prod, nil, []npav1.AdminNetworkPolicyEgressRule{
			anpEgressTo("deny-nodes", npav1.AdminNetworkPolicyRuleActionDeny,
				npav1.AdminNetworkPolicyEgressPeer{Nodes: nsSel(map[string]string{"role": "worker"})}),
		})
		got := replay.NewEvaluator(testCluster, nil, namespaces(),
			replay.WithAdminPolicies([]npav1.AdminNetworkPolicy{p}, nil)).Evaluate(flow)
		if got.Verdict != replay.VerdictUnknown {
			t.Fatalf("Verdict = %q, want UNKNOWN", got.Verdict)
		}
		if got.UnknownReason != replay.ReasonAdminPolicyUnsupported {
			t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, replay.ReasonAdminPolicyUnsupported)
		}
	})
}

// 没有登记管理面策略时，求值必须与从前逐字节一致。
//
// 这是接入这一族时唯一的硬性要求：绝大多数集群没有 ANP，它们的判定不该
// 因为平台学会了读 ANP 而变化一个字节。
func TestAdminPolicyAbsentChangesNothing(t *testing.T) {
	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	policies := []networkingv1.NetworkPolicy{
		npIngress("payment", "allow-gw", nil, []networkingv1.NetworkPolicyIngressRule{{
			From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: nsSel(map[string]string{"role": "edge"})}},
			Ports: tcpPort(8080),
		}}),
	}
	flow := flowBetween(gw, api, 8080)

	without := replay.NewEvaluator(testCluster, policies, namespaces()).Evaluate(flow)
	// 三种"没有管理面策略"的写法都要等价：不传选项、传空切片、传 nil BANP。
	for _, opts := range [][]replay.Option{
		{replay.WithAdminPolicies(nil, nil)},
		{replay.WithAdminPolicies([]npav1.AdminNetworkPolicy{}, nil)},
	} {
		got := replay.NewEvaluator(testCluster, policies, namespaces(), opts...).Evaluate(flow)
		if got != without {
			t.Errorf("registering no admin policy changed the decision:\n got %+v\nwant %+v", got, without)
		}
	}
}
