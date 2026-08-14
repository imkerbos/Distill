package registry_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

func validAccount() registry.Account {
	return registry.Account{Username: "alice", Role: registry.RoleAdmin}
}

// 全部字段合法时必须放行——防止下面几条「必须拒绝」的用例
// 靠一份恒为非法的实现就能全部通过。
func TestValidateAccountAcceptsAWellFormedAccount(t *testing.T) {
	if err := registry.ValidateAccount(validAccount()); err != nil {
		t.Errorf("ValidateAccount() = %v, want nil", err)
	}
}

func TestValidateAccountRejectsEmptyUsername(t *testing.T) {
	a := validAccount()
	a.Username = ""
	if err := registry.ValidateAccount(a); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("ValidateAccount() = %v, want ErrInvalid", err)
	}
}

// 未登记的角色必须被拒绝——账号的角色只能是 internal/auth 里已经登记的
// 取值，不是任意字符串（design doc 2026-08-14 §1「角色只来自服务端账号
// 记录」的前提是这个字段本身封闭）。
func TestAccountRejectsUnregisteredRole(t *testing.T) {
	a := validAccount()
	a.Role = "SUPERADMIN"
	err := registry.ValidateAccount(a)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("ValidateAccount() = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "角色") {
		t.Errorf("err = %q, want it to name the offending field", err)
	}
}

// bcrypt 只取前 72 字节，更长的部分被静默丢弃。若不显式拒绝，
// 一个 100 字符的密码实际只有前 72 个生效，而没有任何人会知道。
func TestPasswordLongerThanBcryptAcceptsIsRejected(t *testing.T) {
	if err := registry.ValidatePassword(strings.Repeat("a", 73)); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("ValidatePassword(73 chars) = %v, want ErrInvalid", err)
	}
	if err := registry.ValidatePassword(strings.Repeat("a", 72)); err != nil {
		t.Fatalf("ValidatePassword(72 chars) = %v, want nil", err)
	}
}

// 上限按字节而不是字符计数：中文字符在 UTF-8 下占 2~4 字节，一个远不到
// 72 个字符的密码可能已经越过 bcrypt 实际截断的那条字节边界。这条用例
// 用 24 个中文字符（72 字节）确认「字符数」与「字节数」在这里不是
// 同一件事——如果实现误按字符计数，这条会放行本该被拒绝的输入。
func TestPasswordLongerThanBcryptAcceptsIsRejectedForMultiByteCharacters(t *testing.T) {
	// 24 个汉字，每个 3 字节 = 72 字节，刚好在边界上，必须放行。
	if err := registry.ValidatePassword(strings.Repeat("密", 24)); err != nil {
		t.Fatalf("ValidatePassword(24 汉字 = 72 字节) = %v, want nil", err)
	}
	// 25 个汉字 = 75 字节，越过边界，必须拒绝——即便字符数只有 25，
	// 远低于「72 个字符」这个误读会给出的上限。
	if err := registry.ValidatePassword(strings.Repeat("密", 25)); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("ValidatePassword(25 汉字 = 75 字节) = %v, want ErrInvalid", err)
	}
}

// 密码最短 12 字符（design doc 2026-08-14 §6）。
func TestPasswordShorterThanTheMinimumIsRejected(t *testing.T) {
	if err := registry.ValidatePassword(strings.Repeat("a", 11)); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("ValidatePassword(11 chars) = %v, want ErrInvalid", err)
	}
	if err := registry.ValidatePassword(strings.Repeat("a", 12)); err != nil {
		t.Fatalf("ValidatePassword(12 chars) = %v, want nil", err)
	}
}
