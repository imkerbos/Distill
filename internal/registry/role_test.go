package registry_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

// 逐个常量显式调用 Valid()，与 baseline.Kind、policygen 的枚举测试同一惯例：
// 新增取值时按惯例加进 AllRoles，漏加的那个在这里就是一条红。
func TestRoleValid(t *testing.T) {
	for _, r := range registry.AllRoles() {
		if !r.Valid() {
			t.Errorf("%q is declared but Valid() says otherwise", r)
		}
	}
	if !registry.RoleAdmin.Valid() {
		t.Error("RoleAdmin must be valid")
	}
	if !registry.RoleViewer.Valid() {
		t.Error("RoleViewer must be valid")
	}
}

// 空角色必须不合法：一个没被赋过角色的会话不能因为零值而拿到任何权限。
func TestRoleZeroValueAndUnknownAreInvalid(t *testing.T) {
	if registry.Role("").Valid() {
		t.Error("the zero role must not be valid — an unassigned role must grant nothing")
	}
	if registry.Role("SUPERADMIN").Valid() {
		t.Error("an unregistered role must not be valid")
	}
	if registry.Role("admin").Valid() {
		t.Error("role matching must be exact, not case-insensitive")
	}
}

func TestRolePermits(t *testing.T) {
	cases := []struct {
		name     string
		holder   registry.Role
		required registry.Role
		want     bool
	}{
		{"admin does what admins do", registry.RoleAdmin, registry.RoleAdmin, true},
		{"admin also reads", registry.RoleAdmin, registry.RoleViewer, true},
		{"viewer reads", registry.RoleViewer, registry.RoleViewer, true},
		{"viewer does not administer", registry.RoleViewer, registry.RoleAdmin, false},
		{"the zero role does nothing", registry.Role(""), registry.RoleViewer, false},
		{"an unknown holder does nothing", registry.Role("SUPERADMIN"), registry.RoleViewer, false},
		{"an unknown requirement is refused", registry.RoleAdmin, registry.Role("OPERATOR"), false},
		{"an empty requirement is refused", registry.RoleAdmin, registry.Role(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.holder.Permits(tc.required); got != tc.want {
				t.Errorf("Role(%q).Permits(%q) = %v, want %v", tc.holder, tc.required, got, tc.want)
			}
		})
	}
}
