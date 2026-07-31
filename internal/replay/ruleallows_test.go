package replay

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ruleAllows 在某个 peer 报错时，必须把该规则已经累积的原因一起带回去。
// 丢掉它意味着 "SNAPSHOT_MISSING 与 POLICY_MALFORMED 同时发生" 这件事
// 只剩下后者，UNKNOWN 构成的统计就少算了一次快照缺失。
func TestRuleAllowsKeepsAccumulatedReasonOnError(t *testing.T) {
	e := &Evaluator{clusterID: "c1", namespaces: nsIndex()}

	r := rule{peers: []networkingv1.NetworkPolicyPeer{
		{NamespaceSelector: &metav1.LabelSelector{}},         // 命名空间不在快照里
		{IPBlock: &networkingv1.IPBlock{CIDR: "not-a-cidr"}}, // 策略写错
	}}
	ep := endpointOf(PodRef{ClusterID: "c1", Namespace: "absent-ns", Name: "x", IP: "10.4.0.1"})

	matched, reason, err := e.ruleAllows(r, "payment", ep, Flow{Protocol: ProtocolTCP, Port: 8080})
	if err == nil {
		t.Fatal("a malformed CIDR must still surface as an error")
	}
	if matched {
		t.Error("matched must be false on the error path")
	}
	if reason != ReasonSnapshotMissing {
		t.Errorf("reason = %q, want %q; the reason accumulated before the error must survive it",
			reason, ReasonSnapshotMissing)
	}
}
