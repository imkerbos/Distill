package replay_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/replay"
)

func meshPod(ns, name, ip string) replay.PodRef {
	p := pod(ns, name, ip, map[string]string{"app": name})
	p.InMesh = true
	return p
}

// sidecar 吃掉 L4 源身份，结论仍然给出但不可信 —— 不标 DEGRADED
// 就会有人拿它去生成策略推荐。
func TestEvaluateMeshEndpointIsDegraded(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces())

	src := meshPod("gateway", "gw-1", "10.4.0.9")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("Confidence = %q, want DEGRADED", got.Confidence)
	}
	if got.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW; degraded still yields a verdict", got.Verdict)
	}
}

func TestEvaluateMeshDestinationIsDegraded(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces())

	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := meshPod("payment", "api-1", "10.4.0.1")

	if got := e.Evaluate(flowBetween(src, dst, 8080)); got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("Confidence = %q, want DEGRADED", got.Confidence)
	}
}

// CCNP 有 deny 语义，与标准 NetworkPolicy 的 additive-allow 不同：
// 存在 CCNP 时，仅基于标准策略的结论不可靠。
func TestEvaluateCCNPPresentDegradesWholeCluster(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces(), replay.WithForeignPlane(true))

	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	if got := e.Evaluate(flowBetween(src, dst, 8080)); got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("Confidence = %q, want DEGRADED when CCNP is present", got.Confidence)
	}
}

// 降级不改变结论本身：DENY 仍然是 DENY，只是不可信。
func TestEvaluateDegradedPreservesDenyVerdict(t *testing.T) {
	e := replay.NewEvaluator(testCluster,
		[]networkingv1.NetworkPolicy{denyAllIngress("payment")}, namespaces())

	src := meshPod("gateway", "gw-1", "10.4.0.9")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictDeny {
		t.Errorf("Verdict = %q, want DENY", got.Verdict)
	}
	if got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("Confidence = %q, want DEGRADED", got.Confidence)
	}
}

func TestEvaluateCleanFlowStaysTrusted(t *testing.T) {
	e := replay.NewEvaluator(testCluster, nil, namespaces())

	src := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	if got := e.Evaluate(flowBetween(src, dst, 8080)); got.Confidence != replay.ConfidenceTrusted {
		t.Errorf("Confidence = %q, want TRUSTED", got.Confidence)
	}
}
