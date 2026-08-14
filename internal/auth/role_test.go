package auth_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/auth"
)

// 逐个常量显式调用 Valid()，与 baseline.Kind、policygen 的枚举测试同一惯例：
// 新增取值时按惯例加进 AllRoles，漏加的那个在这里就是一条红。
func TestRoleValid(t *testing.T) {
	for _, r := range auth.AllRoles() {
		if !r.Valid() {
			t.Errorf("%q is declared but Valid() says otherwise", r)
		}
	}
	if !auth.RoleAdmin.Valid() {
		t.Error("RoleAdmin must be valid")
	}
	if !auth.RoleViewer.Valid() {
		t.Error("RoleViewer must be valid")
	}
}

// 空角色必须不合法：一个没被赋过角色的会话不能因为零值而拿到任何权限。
func TestRoleZeroValueAndUnknownAreInvalid(t *testing.T) {
	if auth.Role("").Valid() {
		t.Error("the zero role must not be valid — an unassigned role must grant nothing")
	}
	if auth.Role("SUPERADMIN").Valid() {
		t.Error("an unregistered role must not be valid")
	}
	if auth.Role("admin").Valid() {
		t.Error("role matching must be exact, not case-insensitive")
	}
}

func TestRolePermits(t *testing.T) {
	cases := []struct {
		name     string
		holder   auth.Role
		required auth.Role
		want     bool
	}{
		{"admin does what admins do", auth.RoleAdmin, auth.RoleAdmin, true},
		{"admin also reads", auth.RoleAdmin, auth.RoleViewer, true},
		{"viewer reads", auth.RoleViewer, auth.RoleViewer, true},
		{"viewer does not administer", auth.RoleViewer, auth.RoleAdmin, false},
		{"the zero role does nothing", auth.Role(""), auth.RoleViewer, false},
		{"an unknown holder does nothing", auth.Role("SUPERADMIN"), auth.RoleViewer, false},
		{"an unknown requirement is refused", auth.RoleAdmin, auth.Role("OPERATOR"), false},
		{"an empty requirement is refused", auth.RoleAdmin, auth.Role(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.holder.Permits(tc.required); got != tc.want {
				t.Errorf("Role(%q).Permits(%q) = %v, want %v", tc.holder, tc.required, got, tc.want)
			}
		})
	}
}
