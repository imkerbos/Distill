package replay_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/replay"
)

// brokenSelectorPolicy 是一条 PodSelector 无法解析的策略。
func brokenSelectorPolicy(ns, name string) networkingv1.NetworkPolicy {
	p := npIngress(ns, name, nil, nil)
	p.Spec.PodSelector = metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: "BogusOperator", Values: []string{"x"}},
		},
	}
	return p
}

// evaluateBothOrders 用同一份策略集的正序与逆序各求值一次。
func evaluateBothOrders(
	t *testing.T,
	policies []networkingv1.NetworkPolicy,
	f replay.Flow,
) (replay.Decision, replay.Decision) {
	t.Helper()

	reversed := make([]networkingv1.NetworkPolicy, 0, len(policies))
	for i := len(policies) - 1; i >= 0; i-- {
		reversed = append(reversed, policies[i])
	}

	forward := replay.NewEvaluator(testCluster, policies, namespaces()).Evaluate(f)
	backward := replay.NewEvaluator(testCluster, reversed, namespaces()).Evaluate(f)
	return forward, backward
}

// Kubernetes List 不保证返回顺序，所以策略切片的排列不得影响任何结论：
// 否则同一集群、同一 commit、同一条流量会回放出两个不同答案，而这些
// 答案会变成应用到生产集群的策略推荐。
//
// 这里的正确答案是 ALLOW：NetworkPolicy 是纯 additive-allow，一条无法
// 解析的策略最多只能"多放行"，绝不可能撤销另一条策略已经给出的放行。
func TestEvaluateVerdictIsIndependentOfPolicyOrder(t *testing.T) {
	good := npIngress("payment", "good", nil, []networkingv1.NetworkPolicyIngressRule{{}})
	bad := brokenSelectorPolicy("payment", "bad")

	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	forward, backward := evaluateBothOrders(t,
		[]networkingv1.NetworkPolicy{good, bad}, flowBetween(gw, api, 8080))

	if forward != backward {
		t.Errorf("policy slice order changed the decision:\n [good,bad] = %+v\n [bad,good] = %+v",
			forward, backward)
	}
	if forward.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW; an unparseable policy can only ever add permission",
			forward.Verdict)
	}
	if forward.UnknownReason != replay.ReasonNone {
		t.Errorf("UnknownReason = %q, want none; a genuine match settles the question", forward.UnknownReason)
	}
	if forward.Reason.MatchedPolicy != "payment/good" {
		t.Errorf("MatchedPolicy = %q, want payment/good", forward.Reason.MatchedPolicy)
	}
}

// 与上一条同源，但触发点是隔离判定：写坏的策略与一条 default-deny 并存。
// 结论仍必须是 UNKNOWN/POLICY_MALFORMED，且 Reason 在两种顺序下一致——
// 旧实现在逆序下会丢掉 Reason.Isolated，让解释器说不出主体已被隔离。
func TestEvaluateMalformedIsolatingPolicyIsOrderIndependent(t *testing.T) {
	deny := denyAllIngress("payment")
	bad := brokenSelectorPolicy("payment", "broken-selector")

	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	forward, backward := evaluateBothOrders(t,
		[]networkingv1.NetworkPolicy{deny, bad}, flowBetween(gw, api, 8080))

	if forward != backward {
		t.Errorf("policy slice order changed the decision:\n [deny,bad] = %+v\n [bad,deny] = %+v",
			forward, backward)
	}
	if forward.Verdict != replay.VerdictUnknown {
		t.Errorf("Verdict = %q, want UNKNOWN", forward.Verdict)
	}
	if forward.UnknownReason != replay.ReasonPolicyMalformed {
		t.Errorf("UnknownReason = %q, want %q", forward.UnknownReason, replay.ReasonPolicyMalformed)
	}
	if !forward.Reason.Isolated {
		t.Error("Reason.Isolated must survive: the default-deny does isolate the subject")
	}
}

// 同一条规则集里两个不同子系统的问题同时出现时，归类必须由固定优先级
// 决定。旧实现是后写覆盖：把 ipBlock 那条规则排在前面，结论就退化成
// SNAPSHOT_MISSING，会把人指向快照管线去查一个 YAML 里的 CIDR 笔误。
func TestEvaluateCoOccurringUnknownReasonsPickHighestPrecedence(t *testing.T) {
	missingNS := networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
		}},
	}
	badCIDR := networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: "not-a-cidr"},
		}},
	}

	src := pod("unlisted-ns", "src-1", "10.4.0.50", map[string]string{"app": "src"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	flow := flowBetween(src, api, 8080)

	orders := map[string][]networkingv1.NetworkPolicyIngressRule{
		"snapshot rule first":  {missingNS, badCIDR},
		"malformed rule first": {badCIDR, missingNS},
	}

	for name, rules := range orders {
		t.Run(name, func(t *testing.T) {
			e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{
				npIngress("payment", "mixed", nil, rules),
			}, namespaces())

			got := e.Evaluate(flow)
			if got.Verdict != replay.VerdictUnknown {
				t.Fatalf("Verdict = %q, want UNKNOWN", got.Verdict)
			}
			if got.UnknownReason != replay.ReasonPolicyMalformed {
				t.Errorf("UnknownReason = %q, want %q; a malformed policy outranks a missing snapshot regardless of rule order",
					got.UnknownReason, replay.ReasonPolicyMalformed)
			}
		})
	}
}

// 决定结论的那一侧是非 ALLOW 时，它的 Reason 就是"结论从哪来"的唯一记录。
// 出向已经命中过一条 allow 规则、入向因策略无法解析判 UNKNOWN 时，若用出向
// 那份信息量更大的 Reason 覆盖入向，解释器就会在一条 UNKNOWN 上报告另一个
// 方向的成功命中，同时丢掉 Detail 里"卡在哪"——§6.6 要的正是这一项。
func TestEvaluateDecidingSideReasonSurvivesPopulatedBase(t *testing.T) {
	policies := []networkingv1.NetworkPolicy{
		npEgress("gateway", "egress-allow", nil, []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			}},
		}}),
		brokenSelectorPolicy("payment", "broken-selector"),
	}

	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := replay.NewEvaluator(testCluster, policies, namespaces()).Evaluate(flowBetween(gw, api, 8080))

	if got.Verdict != replay.VerdictUnknown {
		t.Fatalf("Verdict = %q, want UNKNOWN", got.Verdict)
	}
	if got.UnknownReason != replay.ReasonPolicyMalformed {
		t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, replay.ReasonPolicyMalformed)
	}
	if got.Reason.Direction != replay.DirectionIngress {
		t.Errorf("Reason.Direction = %q, want INGRESS; the deciding side is ingress", got.Reason.Direction)
	}
	if got.Reason.MatchedPolicy != "" {
		t.Errorf("Reason.MatchedPolicy = %q, want empty; an UNKNOWN never matched an allow rule",
			got.Reason.MatchedPolicy)
	}
	if got.Reason.MatchedRuleIdx != -1 {
		t.Errorf("Reason.MatchedRuleIdx = %d, want -1", got.Reason.MatchedRuleIdx)
	}
	if got.Reason.Detail == "" {
		t.Error("Reason.Detail must keep the parse error: it is the only record of where the evaluation got stuck")
	}
}

// 策略无法解析与规则级未决原因同时出现时，分类必须由固定优先级决定，
// 不能由"先测哪个分支"决定。POLICY_MALFORMED 输给排名更低的原因，就等于
// 把一个 YAML 笔误指向快照或服务发现管线去查，而真正该改的是那份 YAML。
func TestEvaluateMalformedPolicyOutranksCoOccurringReasons(t *testing.T) {
	namedPort := intstr.FromString("http")
	// api 没有声明 http 这个命名端口，所以这条规则只能给出 NAMED_PORT_UNRESOLVED。
	unresolvableNamedPort := npIngress("payment", "named-port", nil,
		[]networkingv1.NetworkPolicyIngressRule{{
			Ports: []networkingv1.NetworkPolicyPort{{Port: &namedPort}},
		}})
	// role=edge 只能在命名空间快照里判定，而源 Pod 所在的命名空间不在快照中。
	missingSnapshot := npIngress("payment", "missing-ns", nil,
		[]networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
			}},
		}})
	bad := brokenSelectorPolicy("payment", "broken-selector")

	src := pod("unlisted-ns", "src-1", "10.4.0.50", map[string]string{"app": "src"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	flow := flowBetween(src, api, 8080)

	tests := []struct {
		name       string
		coOccurent networkingv1.NetworkPolicy
	}{
		{name: "with an unresolvable named port", coOccurent: unresolvableNamedPort},
		{name: "with a missing namespace snapshot", coOccurent: missingSnapshot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forward, backward := evaluateBothOrders(t,
				[]networkingv1.NetworkPolicy{tt.coOccurent, bad}, flow)

			if forward != backward {
				t.Errorf("policy slice order changed the decision:\n forward  = %+v\n backward = %+v",
					forward, backward)
			}
			for _, got := range []replay.Decision{forward, backward} {
				if got.Verdict != replay.VerdictUnknown {
					t.Fatalf("Verdict = %q, want UNKNOWN", got.Verdict)
				}
				if got.UnknownReason != replay.ReasonPolicyMalformed {
					t.Errorf("UnknownReason = %q, want %q; an unparseable policy outranks every other reason",
						got.UnknownReason, replay.ReasonPolicyMalformed)
				}
				if got.Reason.Detail == "" {
					t.Error("Reason.Detail must name the policy that failed to parse")
				}
			}
		})
	}
}

// 端点声称属于本集群却没还原出身份时，跳过该方向等于一次隐式 ALLOW——
// 一个标着 TRUSTED、Reason 全空、结构上无法解释的 ALLOW。必须判 UNKNOWN。
func TestEvaluateUnresolvedLocalEndpointIsUnknown(t *testing.T) {
	policies := []networkingv1.NetworkPolicy{
		denyAllIngress("payment"),
		npEgress("gateway", "deny-egress", nil, nil),
	}
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	unresolved := replay.Endpoint{ClusterID: testCluster, IP: "10.4.0.99"}

	tests := []struct {
		name string
		flow replay.Flow
	}{
		{
			name: "source identity not resolved",
			flow: replay.Flow{Source: unresolved, Dest: ep(api), Protocol: replay.ProtocolTCP, Port: 8080},
		},
		{
			name: "destination identity not resolved",
			flow: replay.Flow{Source: ep(api), Dest: unresolved, Protocol: replay.ProtocolTCP, Port: 8080},
		},
		{
			name: "neither identity resolved",
			flow: replay.Flow{Source: unresolved, Dest: unresolved, Protocol: replay.ProtocolTCP, Port: 8080},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replay.NewEvaluator(testCluster, policies, namespaces()).Evaluate(tt.flow)

			if got.Verdict != replay.VerdictUnknown {
				t.Errorf("Verdict = %q, want UNKNOWN; we do not know who this endpoint is", got.Verdict)
			}
			if got.UnknownReason != replay.ReasonSnapshotMissing {
				t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, replay.ReasonSnapshotMissing)
			}
			if got.Reason.Detail == "" {
				t.Error("Reason.Detail must say which endpoint could not be resolved")
			}
		})
	}
}

// 拦截未还原身份的端点时，连接级别的标记必须照常算完：CrossCluster 是
// 平台对外公布的敞口规模，Confidence 决定这条结论能不能用于策略推荐，
// 两者都不能因为提前返回而丢失。
func TestEvaluateUnresolvedLocalEndpointKeepsConnectionFlags(t *testing.T) {
	unresolved := replay.Endpoint{ClusterID: testCluster, IP: "10.4.0.99"}
	remote := ep(remotePod("gateway", "gw-1", "172.16.0.9"))

	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{denyAllIngress("payment")},
		namespaces(), replay.WithForeignPlane(true))

	got := e.Evaluate(replay.Flow{
		Source: remote, Dest: unresolved, Protocol: replay.ProtocolTCP, Port: 8080,
	})

	if got.Verdict != replay.VerdictUnknown {
		t.Fatalf("Verdict = %q, want UNKNOWN", got.Verdict)
	}
	if !got.CrossCluster {
		t.Error("CrossCluster must survive the early return")
	}
	if got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("Confidence = %q, want DEGRADED; CCNP presence must survive the early return", got.Confidence)
	}
}
