package policygen_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/policygen"
)

func TestEvidenceClassEnumIsClosed(t *testing.T) {
	// 5 类：2026-08-18 加入 INCOMPLETE_WINDOW —— 身份可信、但窗口证明不了
	// 自己没漏的那一类（design doc 2026-08-18-learn-from-incomplete-evidence）。
	if len(policygen.AllEvidenceClasses()) != 5 {
		t.Fatalf("AllEvidenceClasses() = %d entries, want 5", len(policygen.AllEvidenceClasses()))
	}
	if policygen.EvidenceClass("SOMETHING").Valid() {
		t.Error("unregistered evidence class reported valid")
	}
	// 空值表示 Baseline 规则，必须合法，否则校验会拒绝每一条 Baseline。
	if !policygen.EvidenceClass("").Valid() {
		t.Error("empty evidence class reported invalid")
	}
}

func TestUngeneratableReasonEnumIsClosed(t *testing.T) {
	if len(policygen.AllUngeneratableReasons()) != 5 {
		t.Fatalf("AllUngeneratableReasons() = %d entries, want 5",
			len(policygen.AllUngeneratableReasons()))
	}
	if policygen.UngeneratableReason("FREE TEXT").Valid() {
		t.Error("free-text reason reported valid; the enum must stay closed")
	}
	for _, r := range policygen.AllUngeneratableReasons() {
		if !r.Valid() {
			t.Errorf("registered reason %q reported invalid", r)
		}
	}
}

// 长度对比抓不住"漏登记"：新增常量若既没写进 allEvidenceClasses，
// 也没写进这里的显式列表，长度依然相等，测试照样通过。这里对每一个
// 已声明的常量显式调用 Valid()——新增常量按惯例加进下面的列表，
// 若同时忘了登记进 allEvidenceClasses，这里就会失败。
func TestEveryDeclaredEvidenceClassIsValid(t *testing.T) {
	for _, e := range []policygen.EvidenceClass{
		policygen.EvidenceTrustedAllow,
		policygen.EvidenceTrustedDeny,
		policygen.EvidenceInternetEgress,
		policygen.EvidenceCrossCluster,
	} {
		if !e.Valid() {
			t.Errorf("declared evidence class %q is not registered in allEvidenceClasses", e)
		}
	}
	// 空值是合法的第五种取值，代表 Baseline 规则不带证据，单独断言。
	if !policygen.EvidenceClass("").Valid() {
		t.Error("empty evidence class must be valid; it marks a Baseline rule")
	}
}

// 同上，针对 UngeneratableReason：新增常量按惯例加进下面的列表，
// 若同时忘了登记进 allUngeneratableReasons，这里就会失败。
func TestEveryDeclaredUngeneratableReasonIsValid(t *testing.T) {
	for _, r := range []policygen.UngeneratableReason{
		policygen.ReasonNoWorkloadLabel,
		policygen.ReasonIdentityUnknown,
		policygen.ReasonDegradedEvidence,
		policygen.ReasonUnmanagedEndpoint,
		policygen.ReasonLabelKeyConflict,
	} {
		if !r.Valid() {
			t.Errorf("declared reason %q is not registered in allUngeneratableReasons", r)
		}
	}
}

func TestWorkloadExclusionReasonEnumIsClosed(t *testing.T) {
	if len(policygen.AllWorkloadExclusionReasons()) != 3 {
		t.Fatalf("AllWorkloadExclusionReasons() = %d entries, want 3",
			len(policygen.AllWorkloadExclusionReasons()))
	}
	if policygen.WorkloadExclusionReason("FREE TEXT").Valid() {
		t.Error("free-text reason reported valid; the enum must stay closed")
	}
	for _, r := range policygen.AllWorkloadExclusionReasons() {
		if !r.Valid() {
			t.Errorf("registered reason %q reported invalid", r)
		}
	}
}

// 同上，针对 WorkloadExclusionReason：新增常量按惯例加进下面的列表，
// 若同时忘了登记进 allWorkloadExclusionReasons，这里就会失败。
func TestEveryDeclaredWorkloadExclusionReasonIsValid(t *testing.T) {
	for _, r := range []policygen.WorkloadExclusionReason{
		policygen.ExclusionHostNetwork,
		policygen.ExclusionNoWorkloadLabel,
		policygen.ExclusionLabelKeyConflict,
	} {
		if !r.Valid() {
			t.Errorf("declared reason %q is not registered in allWorkloadExclusionReasons", r)
		}
	}
}
