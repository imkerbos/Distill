package replay_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/replay"
)

func hostNetworkPod(ns, name, ip string) replay.PodRef {
	p := pod(ns, name, ip, map[string]string{"app": name})
	p.HostNetwork = true
	return p
}

func TestIsUnmanaged(t *testing.T) {
	if !replay.IsUnmanaged(hostNetworkPod("kube-system", "agent", "192.168.1.7")) {
		t.Error("hostNetwork pod must be reported as unmanaged by NetworkPolicy")
	}
	if replay.IsUnmanaged(pod("payment", "api-1", "10.4.0.1", nil)) {
		t.Error("regular pod must not be reported as unmanaged")
	}
}

// hostNetwork Pod 使用 Node IP、不在 Pod 网络内，NetworkPolicy 对其
// 基本不生效。把它判成 DENY 会产出一条永远无法验证的错误预测。
func TestEvaluateHostNetworkDestinationIsNotIsolated(t *testing.T) {
	e := replay.NewEvaluator(testCluster,
		[]networkingv1.NetworkPolicy{denyAllIngress("kube-system")}, namespaces())

	src := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	dst := hostNetworkPod("kube-system", "agent", "192.168.1.7")

	got := e.Evaluate(flowBetween(src, dst, 9100))
	if got.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW; hostNetwork pods are outside NetworkPolicy scope", got.Verdict)
	}
	if !got.Reason.Unmanaged {
		t.Error("Reason.Unmanaged must be set so coverage stats can exclude this pod")
	}
}

func TestEvaluateHostNetworkSourceIsNotEgressIsolated(t *testing.T) {
	egressDeny := networkingv1.NetworkPolicy{}
	egressDeny.Namespace = "kube-system"
	egressDeny.Name = "default-deny-egress"
	egressDeny.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}

	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{egressDeny}, namespaces())

	src := hostNetworkPod("kube-system", "agent", "192.168.1.7")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW for a hostNetwork source", got.Verdict)
	}
	if !got.Reason.Unmanaged {
		t.Error("Reason.Unmanaged must be set when the source is a hostNetwork pod")
	}
}

// Unmanaged 必须在 DENY 早退路径上也传出去：覆盖率统计依赖它，
// 把 hostNetwork Pod 算成"已保护"会让覆盖率数字失真。
func TestEvaluateUnmanagedFlagSurvivesDenyPath(t *testing.T) {
	e := replay.NewEvaluator(testCluster,
		[]networkingv1.NetworkPolicy{denyAllIngress("payment")}, namespaces())

	src := hostNetworkPod("kube-system", "agent", "192.168.1.7")
	dst := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := e.Evaluate(flowBetween(src, dst, 8080))
	if got.Verdict != replay.VerdictDeny {
		t.Fatalf("Verdict = %q, want DENY; the destination is ingress-isolated", got.Verdict)
	}
	if !got.Reason.Unmanaged {
		t.Error("Reason.Unmanaged must survive the deny early-return path")
	}
}

// 对称覆盖：出向早退路径上的 Unmanaged 同样不能丢。
// 源是普通 Pod（参与 egress 判定并被拒），目的是 hostNetwork Pod
// （使该连接带上 unmanaged 标记）。
func TestEvaluateUnmanagedFlagSurvivesEgressDenyPath(t *testing.T) {
	egressDeny := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "default-deny-egress"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}

	e := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{egressDeny}, namespaces())

	src := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})
	dst := hostNetworkPod("kube-system", "agent", "192.168.1.7")

	got := e.Evaluate(flowBetween(src, dst, 9100))
	if got.Verdict != replay.VerdictDeny {
		t.Fatalf("Verdict = %q, want DENY; the source is egress-isolated with no matching rule", got.Verdict)
	}
	if !got.Reason.Unmanaged {
		t.Error("Reason.Unmanaged must survive the egress deny early-return path")
	}
	if got.Reason.Direction != replay.DirectionEgress {
		t.Errorf("Reason.Direction = %q, want EGRESS", got.Reason.Direction)
	}
}
