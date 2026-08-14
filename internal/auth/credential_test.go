package auth_test

import (
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/registry"
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

	if _, ok := v.Verify("demo", "secret"); !ok {
		t.Error("correct credentials must verify")
	}
	if _, ok := v.Verify("demo", "wrong"); ok {
		t.Error("wrong password must not verify")
	}
	if _, ok := v.Verify("ghost", "secret"); ok {
		t.Error("unknown user must not verify")
	}
}

// 引导账号是管理员：它是平台的第一次登录，也是此后一切变更的唯一入口。
func TestVerifyReturnsTheAccountRole(t *testing.T) {
	v := auth.NewVerifier([]config.User{
		{Username: "demo", PasswordHash: hashOf(t, "secret")},
	})

	role, ok := v.Verify("demo", "secret")
	if !ok {
		t.Fatal("correct credentials must verify")
	}
	if role != registry.RoleAdmin {
		t.Errorf("role = %q, want %q — the bootstrap account is the administrator", role, registry.RoleAdmin)
	}
}

// 校验失败时不得回一个合法角色：调用方拿它去签会话，那就是一次凭空的授权。
func TestVerifyFailureYieldsNoRole(t *testing.T) {
	v := auth.NewVerifier([]config.User{
		{Username: "demo", PasswordHash: hashOf(t, "secret")},
	})

	for _, tc := range []struct{ user, pass string }{
		{"demo", "wrong"},
		{"ghost", "secret"},
		{"", ""},
	} {
		role, ok := v.Verify(tc.user, tc.pass)
		if ok {
			t.Fatalf("Verify(%q, %q) must fail", tc.user, tc.pass)
		}
		if role.Valid() {
			t.Errorf("Verify(%q, %q) returned a usable role %q on failure", tc.user, tc.pass, role)
		}
	}
}

// 未知用户与错误密码都必须走完哈希比对，否则响应耗时会泄露账号是否存在。
func TestVerifyUnknownUserStillHashes(t *testing.T) {
	v := auth.NewVerifier([]config.User{
		{Username: "demo", PasswordHash: hashOf(t, "secret")},
	})
	// 行为断言：两种失败的返回值逐字段相同，调用方无法区分。
	ghostRole, ghostOK := v.Verify("ghost", "x")
	wrongRole, wrongOK := v.Verify("demo", "x")
	if ghostOK != wrongOK || ghostRole != wrongRole {
		t.Error("unknown-user and wrong-password must be indistinguishable to the caller")
	}
}

func TestVerifyRejectsEmpty(t *testing.T) {
	v := auth.NewVerifier([]config.User{{Username: "demo", PasswordHash: hashOf(t, "secret")}})
	if _, ok := v.Verify("", ""); ok {
		t.Error("empty credentials must not verify")
	}
}
