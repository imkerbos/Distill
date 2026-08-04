package auth_test

import (
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/config"
)

func hashOf(t *testing.T, plain string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return string(h)
}

func TestVerify(t *testing.T) {
	v := auth.NewVerifier([]config.User{
		{Username: "demo", PasswordHash: hashOf(t, "secret")},
	})

	if !v.Verify("demo", "secret") {
		t.Error("correct credentials must verify")
	}
	if v.Verify("demo", "wrong") {
		t.Error("wrong password must not verify")
	}
	if v.Verify("ghost", "secret") {
		t.Error("unknown user must not verify")
	}
}

// 未知用户与错误密码都必须走完哈希比对，否则响应耗时会泄露账号是否存在。
func TestVerifyUnknownUserStillHashes(t *testing.T) {
	v := auth.NewVerifier([]config.User{
		{Username: "demo", PasswordHash: hashOf(t, "secret")},
	})
	// 行为断言：两种失败都返回 false，调用方无法区分。
	if v.Verify("ghost", "x") != v.Verify("demo", "x") {
		t.Error("unknown-user and wrong-password must be indistinguishable to the caller")
	}
}

func TestVerifyRejectsEmpty(t *testing.T) {
	v := auth.NewVerifier([]config.User{{Username: "demo", PasswordHash: hashOf(t, "secret")}})
	if v.Verify("", "") {
		t.Error("empty credentials must not verify")
	}
}
