package replay_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/replay"
)

// unknown_reason 必须是封闭枚举：指标要统计 UNKNOWN 的构成，
// 自由文本既无法聚合也无法驱动改进。
func TestUnknownReasonIsClosedEnum(t *testing.T) {
	want := []replay.UnknownReason{
		replay.ReasonSnapshotMissing,
		replay.ReasonIPAmbiguous,
		replay.ReasonClusterAmbiguous,
		replay.ReasonIdentityLostMesh,
		replay.ReasonCCNPPresent,
		replay.ReasonNATTranslated,
		replay.ReasonExternalNoIdentity,
		replay.ReasonNamedPortUnresolved,
		replay.ReasonLogSampledOut,
		replay.ReasonPolicyMalformed,
		replay.ReasonAdminPolicyUnsupported,
		replay.ReasonAdminPriorityAmbiguous,
	}

	got := replay.AllUnknownReasons()
	if len(got) != len(want) {
		t.Fatalf("AllUnknownReasons() has %d entries, want %d; a new reason must be registered", len(got), len(want))
	}

	registered := make(map[replay.UnknownReason]bool, len(got))
	for _, r := range got {
		registered[r] = true
	}
	for _, r := range want {
		if !registered[r] {
			t.Errorf("reason %q is not registered in AllUnknownReasons()", r)
		}
	}
}

func TestUnknownReasonValid(t *testing.T) {
	if !replay.ReasonIPAmbiguous.Valid() {
		t.Error("registered reason must be Valid")
	}
	if !replay.ReasonNone.Valid() {
		t.Error("ReasonNone must be Valid; it means no unknown reason applies")
	}
	if replay.UnknownReason("BECAUSE_IT_LOOKED_WEIRD").Valid() {
		t.Error("free-text reason must be rejected")
	}
}

// verdict 与 confidence 必须独立：mesh 与 CCNP 场景下能判定但不可信，
// 合并成单字段会让这类结论被当作可信结果使用。
func TestDecisionKeepsVerdictAndConfidenceIndependent(t *testing.T) {
	d := replay.Decision{
		Verdict:    replay.VerdictAllow,
		Confidence: replay.ConfidenceDegraded,
	}
	if d.Verdict != replay.VerdictAllow {
		t.Errorf("Verdict = %q, want ALLOW", d.Verdict)
	}
	if d.Confidence != replay.ConfidenceDegraded {
		t.Errorf("Confidence = %q, want DEGRADED", d.Confidence)
	}
}

func TestReasonZeroValueHasNoMatchedRule(t *testing.T) {
	r := replay.NewReason(replay.DirectionIngress)
	if r.MatchedRuleIdx != -1 {
		t.Errorf("MatchedRuleIdx = %d, want -1 for no match", r.MatchedRuleIdx)
	}
}
