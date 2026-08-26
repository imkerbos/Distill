package predict_test

import (
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/replay"
)

func pod(ns, name, app string) *replay.PodRef {
	return &replay.PodRef{
		ClusterID: "c1", Namespace: ns, Name: name,
		Labels: map[string]string{"app": app},
	}
}

func nss() []replay.NamespaceRef {
	return []replay.NamespaceRef{
		{ClusterID: "c1", Name: "gateway", Labels: map[string]string{"kubernetes.io/metadata.name": "gateway"}},
		{ClusterID: "c1", Name: "payment", Labels: map[string]string{"kubernetes.io/metadata.name": "payment"}},
	}
}

func flow(src, dst *replay.PodRef, port int32) replay.Flow {
	return replay.Flow{
		Source:   replay.Endpoint{ClusterID: "c1", IP: "10.4.0.1", Pod: src},
		Dest:     replay.Endpoint{ClusterID: "c1", IP: "10.4.0.4", Pod: dst},
		Protocol: replay.ProtocolTCP, Port: port,
		Timestamp: time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
	}
}

// denyAllFor 造一条只有 default-deny 的策略，任何流量在它下面都是 DENY。
func denyAllFor(ns string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "candidate-api"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress,
			},
		},
	}
}

// 现在 ALLOW、候选下 DENY —— 这是上线即阻断，页面上唯一真正要人看的数字。
func TestWouldBreakIsDetected(t *testing.T) {
	f := flow(pod("gateway", "gateway-1", "gateway"), pod("payment", "payment-1", "api"), 8080)
	rep := predict.Run(predict.Input{
		ClusterID:  "c1",
		Policies:   []networkingv1.NetworkPolicy{denyAllFor("payment")},
		Namespaces: nss(),
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f,
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})
	if got := rep.Counts[predict.ChangeWouldBreak]; got != 1 {
		t.Fatalf("WOULD_BREAK = %d, want 1", got)
	}
	items := rep.Changes[predict.ChangeWouldBreak]
	if len(items) != 1 || items[0].FlowID != "f1" {
		t.Fatalf("changes = %+v, want the complete list, not just a count", items)
	}
	if items[0].Current != "ALLOW" || items[0].Predicted != "DENY" {
		t.Errorf("change = %+v, want ALLOW -> DENY", items[0])
	}
}

// 现在 DENY、候选下 ALLOW —— 敌方面扩大，必须单列。
func TestWouldOpenIsDetected(t *testing.T) {
	f := flow(pod("gateway", "gateway-1", "gateway"), pod("payment", "payment-1", "api"), 8080)
	// 候选策略放行 gateway → payment:8080。
	allow := denyAllFor("payment")
	allow.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "gateway"},
			},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
		}},
	}}
	rep := predict.Run(predict.Input{
		ClusterID:  "c1",
		Policies:   []networkingv1.NetworkPolicy{allow},
		Namespaces: nss(),
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f,
			Decision: replay.Decision{Verdict: replay.VerdictDeny, Confidence: replay.ConfidenceTrusted},
		}},
	})
	if got := rep.Counts[predict.ChangeWouldOpen]; got != 1 {
		t.Fatalf("WOULD_OPEN = %d, want 1 (report = %+v)", got, rep.Counts)
	}
}

func TestUnchangedIsCounted(t *testing.T) {
	f := flow(pod("gateway", "gateway-1", "gateway"), pod("payment", "payment-1", "api"), 8080)
	rep := predict.Run(predict.Input{
		ClusterID:  "c1",
		Policies:   []networkingv1.NetworkPolicy{denyAllFor("payment")},
		Namespaces: nss(),
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f,
			Decision: replay.Decision{Verdict: replay.VerdictDeny, Confidence: replay.ConfidenceTrusted},
		}},
	})
	if got := rep.Counts[predict.ChangeUnchanged]; got != 1 {
		t.Errorf("UNCHANGED = %d, want 1", got)
	}
}

// current 侧判不出的流量绝不能落进 UNCHANGED。
//
// current=UNKNOWN + predicted=DENY 归入 UNCHANGED，界面会以
// 「判定结论保持一致」呈现，等于宣称我们知道这条连接原本是通的。
// 构成必须记 current 侧的原因：predicted 侧根本没有原因可记。
func TestCurrentUnknownIsNotCountedAsUnchanged(t *testing.T) {
	f := flow(pod("gateway", "gateway-1", "gateway"), pod("payment", "payment-1", "api"), 8080)
	rep := predict.Run(predict.Input{
		ClusterID:  "c1",
		Policies:   []networkingv1.NetworkPolicy{denyAllFor("payment")},
		Namespaces: nss(),
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f,
			Decision: replay.Decision{
				Verdict:       replay.VerdictUnknown,
				UnknownReason: replay.ReasonPolicyMalformed,
				Confidence:    replay.ConfidenceTrusted,
			},
		}},
	})
	if got := rep.Counts[predict.ChangeUnchanged]; got != 0 {
		t.Errorf("UNCHANGED = %d, want 0; an indeterminate current verdict is not agreement", got)
	}
	if got := rep.Counts[predict.ChangeUnknown]; got != 1 {
		t.Fatalf("UNKNOWN = %d, want 1 (counts = %+v)", got, rep.Counts)
	}
	if got := rep.UnknownComposition[string(replay.ReasonPolicyMalformed)]; got != 1 {
		t.Errorf("composition[POLICY_MALFORMED] = %d, want 1; composition = %+v",
			got, rep.UnknownComposition)
	}
	items := rep.Changes[predict.ChangeUnknown]
	if len(items) != 1 || items[0].UnknownReason != string(replay.ReasonPolicyMalformed) {
		t.Errorf("changes = %+v, want the row to carry the current-side reason", items)
	}
}

// 两条 UNKNOWN 入口混在一起时，构成之和仍必须等于 UNKNOWN 总数，
// 且各自记到自己那一侧的原因上 —— 否则缺口追不回源头。
func TestUnknownCompositionSplitsBothEntryPaths(t *testing.T) {
	byCurrent := flow(pod("gateway", "gateway-1", "gateway"), pod("payment", "payment-1", "api"), 8080)
	byPredicted := flow(pod("gateway", "gateway-1", "gateway"), nil, 8080)
	byPredicted.Dest = replay.Endpoint{ClusterID: "c1", IP: "10.4.9.9"} // 本集群内、身份未还原

	rep := predict.Run(predict.Input{
		ClusterID:  "c1",
		Policies:   []networkingv1.NetworkPolicy{denyAllFor("payment")},
		Namespaces: nss(),
		Observations: []policygen.Observation{
			{FlowID: "f1", Flow: byCurrent, Decision: replay.Decision{
				Verdict:       replay.VerdictUnknown,
				UnknownReason: replay.ReasonPolicyMalformed,
				Confidence:    replay.ConfidenceTrusted,
			}},
			{FlowID: "f2", Flow: byPredicted, Decision: replay.Decision{
				Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted,
			}},
		},
	})
	if got := rep.Counts[predict.ChangeUnknown]; got != 2 {
		t.Fatalf("UNKNOWN = %d, want 2 (counts = %+v)", got, rep.Counts)
	}
	if got := rep.UnknownComposition[string(replay.ReasonPolicyMalformed)]; got != 1 {
		t.Errorf("current-side reason counted %d times, want 1", got)
	}
	if got := rep.UnknownComposition[string(replay.ReasonSnapshotMissing)]; got != 1 {
		t.Errorf("predicted-side reason counted %d times, want 1; composition = %+v",
			got, rep.UnknownComposition)
	}
	total := 0
	for _, n := range rep.UnknownComposition {
		total += n
	}
	if total != rep.Counts[predict.ChangeUnknown] {
		t.Errorf("composition sums to %d but UNKNOWN count is %d", total, rep.Counts[predict.ChangeUnknown])
	}
}

// UNKNOWN 必须按原因分组给出绝对数，不能只给一个比例。
func TestUnknownCompositionIsReported(t *testing.T) {
	f := flow(pod("gateway", "gateway-1", "gateway"), nil, 8080)
	f.Dest = replay.Endpoint{ClusterID: "c1", IP: "10.4.9.9"} // 本集群内、身份未还原
	rep := predict.Run(predict.Input{
		ClusterID:  "c1",
		Policies:   []networkingv1.NetworkPolicy{denyAllFor("payment")},
		Namespaces: nss(),
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f,
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})
	if rep.Counts[predict.ChangeUnknown] != 1 {
		t.Fatalf("UNKNOWN = %d, want 1", rep.Counts[predict.ChangeUnknown])
	}
	if len(rep.UnknownComposition) == 0 {
		t.Error("UnknownComposition empty; a bare UNKNOWN count cannot tell anyone what to fix")
	}
	total := 0
	for _, n := range rep.UnknownComposition {
		total += n
	}
	if total != rep.Counts[predict.ChangeUnknown] {
		t.Errorf("composition sums to %d but UNKNOWN count is %d",
			total, rep.Counts[predict.ChangeUnknown])
	}
}

// confidence 分布必须同屏可得：90% DEGRADED 的预测不能和 90% TRUSTED
// 摆在一起看（spec §8.1）。
func TestConfidenceDistributionIsReported(t *testing.T) {
	f := flow(pod("gateway", "gateway-1", "gateway"), pod("payment", "payment-1", "api"), 8080)
	rep := predict.Run(predict.Input{
		ClusterID: "c1", ForeignPlane: true,
		Policies:   []networkingv1.NetworkPolicy{denyAllFor("payment")},
		Namespaces: nss(),
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f,
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceDegraded},
		}},
	})
	if rep.DegradedCount != 1 || rep.TrustedCount != 0 {
		t.Errorf("trusted=%d degraded=%d, want 0/1", rep.TrustedCount, rep.DegradedCount)
	}
	if rep.TotalEvaluated != 1 {
		t.Errorf("TotalEvaluated = %d, want 1", rep.TotalEvaluated)
	}
}

// 可信度三档之和必须等于评估总数。
//
// 从前的 default 分支把任何未识别取值都算成 TRUSTED，和恰好对得上，
// 分布看起来自洽 —— 那正是最难发现的一类错报。
func TestConfidenceTallySumsToTotal(t *testing.T) {
	f := flow(pod("gateway", "gateway-1", "gateway"), pod("payment", "payment-1", "api"), 8080)
	rep := predict.Run(predict.Input{
		ClusterID:  "c1",
		Policies:   []networkingv1.NetworkPolicy{denyAllFor("payment")},
		Namespaces: nss(),
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f,
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})
	if sum := rep.TrustedCount + rep.DegradedCount + rep.UnratedCount; sum != rep.TotalEvaluated {
		t.Errorf("trusted+degraded+unrated = %d, TotalEvaluated = %d", sum, rep.TotalEvaluated)
	}
	if rep.UnratedCount != 0 {
		t.Errorf("UnratedCount = %d, want 0; the evaluator only emits registered confidences",
			rep.UnratedCount)
	}
}

func TestChangeKindEnumIsClosed(t *testing.T) {
	if len(predict.AllChangeKinds()) != 4 {
		t.Fatalf("AllChangeKinds() = %d, want 4", len(predict.AllChangeKinds()))
	}
	if predict.ChangeKind("MAYBE").Valid() {
		t.Error("unregistered change kind reported valid")
	}
}
