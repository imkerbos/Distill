package buildinfo_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/buildinfo"
)

func TestVersionIsNotEmpty(t *testing.T) {
	if got := buildinfo.Version(); got == "" {
		t.Fatalf("Version() = %q, want non-empty", got)
	}
}
