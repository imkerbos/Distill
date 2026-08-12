package gitverify_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/imkerbos/Distill/internal/gitverify"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
)

func TestClassifyDistinguishesWhoNeedsToFixIt(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want registry.VerifyResult
	}{
		{"credential missing", secrets.ErrNotFound, registry.VerifyCredentialUnresolved},
		{"auth rejected", transport.ErrAuthenticationRequired, registry.VerifyAuthFailed},
		{"auth invalid", transport.ErrAuthorizationFailed, registry.VerifyAuthFailed},
		{"repo absent", transport.ErrRepositoryNotFound, registry.VerifyRepoUnreachable},
		{"timeout", context.DeadlineExceeded, registry.VerifyRepoUnreachable},
		{"branch absent", plumbing.ErrReferenceNotFound, registry.VerifyBranchMissing},
		// clone 阶段分支不存在给的不是 ErrReferenceNotFound，见 verify.go。
		{"branch absent from clone", git.NoMatchingRefSpecError{}, registry.VerifyBranchMissing},
		{"empty remote", transport.ErrEmptyRemoteRepository, registry.VerifyBranchMissing},
		{"unknown", errors.New("boom"), registry.VerifyRepoUnreachable},
		{"no error", nil, registry.VerifyOK},
	}
	for _, c := range cases {
		if got := gitverify.Classify(c.err); got != c.want {
			t.Errorf("%s: Classify() = %q, want %q", c.name, got, c.want)
		}
	}
}

// 底层错误文本不得出现在结论里：结论是封闭枚举，
// 自由文本会把仓库地址、用户名甚至凭据片段带到 API 边界。
func TestClassifyReturnsOnlyEnumValues(t *testing.T) {
	got := gitverify.Classify(errors.New("ssh: handshake failed for git@internal.example.com"))
	if !got.Valid() {
		t.Fatalf("Classify() = %q, which is not a valid enum value", got)
	}
	if strings.Contains(string(got), "example.com") {
		t.Fatalf("Classify() leaked the underlying error text: %q", got)
	}
}

// 包装过的错误也要认得出来：go-git 与 secrets 都会用 %w 往上裹。
func TestClassifyLooksThroughWrapping(t *testing.T) {
	wrapped := errors.Join(errors.New("clone failed"), transport.ErrAuthorizationFailed)
	if got := gitverify.Classify(wrapped); got != registry.VerifyAuthFailed {
		t.Fatalf("Classify(wrapped) = %q, want %q", got, registry.VerifyAuthFailed)
	}
}
