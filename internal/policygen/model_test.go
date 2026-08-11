package policygen_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/policygen"
)

func TestEvidenceClassEnumIsClosed(t *testing.T) {
	if len(policygen.AllEvidenceClasses()) != 4 {
		t.Fatalf("AllEvidenceClasses() = %d entries, want 4", len(policygen.AllEvidenceClasses()))
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
	if len(policygen.AllUngeneratableReasons()) != 4 {
		t.Fatalf("AllUngeneratableReasons() = %d entries, want 4",
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
