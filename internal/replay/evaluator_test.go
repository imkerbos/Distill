package replay_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/replay"
)

const testCluster = "prod-asia-1"

func pod(ns, name, ip string, labels map[string]string) replay.PodRef {
	return replay.PodRef{
		ClusterID: testCluster, Namespace: ns, Name: name, IP: ip, Labels: labels,
	}
}

func ep(p replay.PodRef) replay.Endpoint {
	return replay.Endpoint{ClusterID: p.ClusterID, IP: p.IP, Pod: &p}
}

func flowBetween(src, dst replay.PodRef, port int32) replay.Flow {
	return replay.Flow{Source: ep(src), Dest: ep(dst), Protocol: replay.ProtocolTCP, Port: port}
}

func namespaces() []replay.NamespaceRef {
	return []replay.NamespaceRef{
		{ClusterID: testCluster, Name: "payment", Labels: map[string]string{"env": "prod"}},
		{ClusterID: testCluster, Name: "gateway", Labels: map[string]string{"env": "prod", "role": "edge"}},
	}
}

func denyAllIngress(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default-deny-ingress"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
}

func allowFromGateway(ns string, port int32) networkingv1.NetworkPolicy {
	v := intstr.FromInt32(port)
	tcp := corev1.ProtocolTCP
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "allow-gateway"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: &v, Protocol: &tcp}},
			}},
		},
	}
}

// 没有任何策略时，流量默认放行。
func TestEvaluateNoPoliciesAllows(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces())
	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW", got.Verdict)
	}
	if got.Confidence != replay.ConfidenceTrusted {
		t.Errorf("Confidence = %q, want TRUSTED", got.Confidence)
	}
}

func TestEvaluateDefaultDenyBlocks(t *testing.T) {
	e := replay.NewEvaluator(testCluster,
		[]networkingv1.NetworkPolicy{denyAllIngress("payment")}, namespaces())
	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictDeny {
		t.Errorf("Verdict = %q, want DENY", got.Verdict)
	}
	if !got.Reason.Isolated {
		t.Error("Reason.Isolated must be true so the explainer can say why")
	}
	if got.Reason.Direction != replay.DirectionIngress {
		t.Errorf("Reason.Direction = %q, want INGRESS", got.Reason.Direction)
	}
}

// 多条策略是 additive 的：一条 default-deny 加一条放行，结果是放行。
func TestEvaluateAdditivePolicies(t *testing.T) {
	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{
		denyAllIngress("payment"),
		allowFromGateway("payment", 8080),
	}, namespaces())

	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW; policies are additive", got.Verdict)
	}
	if got.Reason.MatchedPolicy != "payment/allow-gateway" {
		t.Errorf("MatchedPolicy = %q, want payment/allow-gateway", got.Reason.MatchedPolicy)
	}
}

func TestEvaluateWrongPortStillDenied(t *testing.T) {
	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{
		denyAllIngress("payment"),
		allowFromGateway("payment", 8080),
	}, namespaces())

	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	if got := e.Evaluate(flowBetween(src, dst, 9090)); got.Verdict != replay.VerdictDeny {
		t.Errorf("Verdict = %q, want DENY for a port outside the rule", got.Verdict)
	}
}

// 一条连接需要源侧 egress 与目的侧 ingress 同时放行。
func TestEvaluateRequiresBothDirections(t *testing.T) {
	egressDeny := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gateway", Name: "default-deny-egress"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}

	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{egressDeny}, namespaces())
	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictDeny {
		t.Errorf("Verdict = %q, want DENY; egress side blocks it", got.Verdict)
	}
	if got.Reason.Direction != replay.DirectionEgress {
		t.Errorf("Reason.Direction = %q, want EGRESS", got.Reason.Direction)
	}
}

// 命名端口无法解析时降级为 UNKNOWN，不能判 DENY。
func TestEvaluateUnresolvedNamedPortIsUnknown(t *testing.T) {
	name := intstr.FromString("http")
	tcp := corev1.ProtocolTCP
	p := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "allow-named"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Port: &name, Protocol: &tcp}},
			}},
		},
	}

	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{p}, namespaces())
	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"}) // 无 NamedPorts

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictUnknown {
		t.Errorf("Verdict = %q, want UNKNOWN", got.Verdict)
	}
	if got.UnknownReason != replay.ReasonNamedPortUnresolved {
		t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, replay.ReasonNamedPortUnresolved)
	}
}

// 策略格式非法时必须判 UNKNOWN 而非 DENY —— 静默跳过一条无法求值的
// 规则，会生成一条建立在错误前提上的策略推荐。
func TestEvaluateMalformedPolicyIsUnknown(t *testing.T) {
	p := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "bad-cidr"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					IPBlock: &networkingv1.IPBlock{CIDR: "not-a-cidr"},
				}},
			}},
		},
	}

	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{p}, namespaces())
	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictUnknown {
		t.Errorf("Verdict = %q, want UNKNOWN", got.Verdict)
	}
	if got.UnknownReason != replay.ReasonPolicyMalformed {
		t.Errorf("UnknownReason = %q, want %q", got.UnknownReason, replay.ReasonPolicyMalformed)
	}
}
