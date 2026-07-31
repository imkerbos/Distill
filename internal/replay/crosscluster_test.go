package replay_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/replay"
)

func remotePod(ns, name, ip string) replay.PodRef {
	return replay.PodRef{
		ClusterID: "prod-eu-1", Namespace: ns, Name: name, IP: ip,
		Labels: map[string]string{"app": name},
	}
}

func TestEvaluateMarksCrossClusterFlows(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces())

	src := remotePod("gateway", "gw-1", "172.16.0.9")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if !got.CrossCluster {
		t.Error("CrossCluster must be set; this is a known enforcement gap that has to stay visible")
	}
}

// 外部地址没有 ClusterID，不能被当成跨集群流量：CrossCluster 是平台
// 对外公布的已知敞口规模，把公网出向算进去会虚增这个数字。
func TestEvaluateExternalEndpointIsNotCrossCluster(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces())

	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	external := replay.Endpoint{IP: "8.8.8.8"}

	got := e.Evaluate(replay.Flow{
		Source: ep(src), Dest: external, Protocol: replay.ProtocolTCP, Port: 443,
	})
	if got.CrossCluster {
		t.Error("an endpoint with no ClusterID must not be marked cross-cluster")
	}
}

func TestEvaluateSameClusterIsNotCrossCluster(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces())

	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	if got := e.Evaluate(flowBetween(src, dst, 8080)); got.CrossCluster {
		t.Error("same-cluster flow must not be marked CrossCluster")
	}
}

// 跨集群对端无 ipBlock 覆盖时结论是 DENY 而非 UNKNOWN：
// 策略确实会拦截它，这是正确结论，不是数据不足。
func TestEvaluateCrossClusterPeerDeniedWithoutIPBlock(t *testing.T) {
	e := replay.NewEvaluator(testCluster,
		[]networkingv1.NetworkPolicy{denyAllIngress("payment")}, namespaces())

	src := remotePod("gateway", "gw-1", "172.16.0.9")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictDeny {
		t.Errorf("Verdict = %q, want DENY", got.Verdict)
	}
	if got.UnknownReason != replay.ReasonNone {
		t.Errorf("UnknownReason = %q, want none; the policy genuinely blocks this", got.UnknownReason)
	}
	if !got.CrossCluster {
		t.Error("CrossCluster must stay set on the denied decision")
	}
}

// 本集群策略不能用 selector 选中其他集群的 Pod，只能靠 ipBlock。
func TestEvaluateCrossClusterPeerAllowedByIPBlock(t *testing.T) {
	p := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "allow-remote-cidr"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					IPBlock: &networkingv1.IPBlock{CIDR: "172.16.0.0/14"},
				}},
			}},
		},
	}

	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{p}, namespaces())

	src := remotePod("gateway", "gw-1", "172.16.0.9")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW via ipBlock", got.Verdict)
	}
}

// 远端 Pod 有标签，但本集群的 namespaceSelector 不该选中它。
func TestEvaluateCrossClusterPeerNotSelectableBySelector(t *testing.T) {
	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{
		denyAllIngress("payment"),
		allowFromGateway("payment", 8080),
	}, namespaces())

	src := remotePod("gateway", "gw-1", "172.16.0.9")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictDeny {
		t.Errorf("Verdict = %q, want DENY; a local selector must not reach another cluster's pod", got.Verdict)
	}
}
