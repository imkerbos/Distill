package secrets_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/secrets"
)

func TestValidateRefRejectsWhatCannotBeASecretName(t *testing.T) {
	ok := []string{"a", "prod-asia-1", "x9", strings.Repeat("a", 64)}
	for _, ref := range ok {
		if err := secrets.ValidateRef(ref); err != nil {
			t.Errorf("ValidateRef(%q) = %v, want nil", ref, err)
		}
	}
	bad := []string{
		"", "-lead", "trail-", "Upper", "under_score", "dot.dot",
		"has/slash", "..", "../etc/passwd", strings.Repeat("a", 65),
	}
	for _, ref := range bad {
		if err := secrets.ValidateRef(ref); err == nil {
			t.Errorf("ValidateRef(%q) = nil, want error", ref)
		}
	}
}
