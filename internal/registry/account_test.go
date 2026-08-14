package registry_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

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

// PasswordHash 必须能验出正确的密码，也必须验不过错的。
//
// 先立这一条，下面「拿不出哈希」的几条才不能靠一个空壳类型全部通过 ——
// 一个什么都不装的容器当然什么也漏不出来。
func TestPasswordHashMatchesOnlyTheRightPassword(t *testing.T) {
	const password = "correct-horse-battery"
	raw, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	h := registry.NewPasswordHash(string(raw))

	if !h.Matches(password) {
		t.Error("Matches(正确密码) = false —— 库里的账号就此谁也登不进来")
	}
	if h.Matches("not-the-password") {
		t.Error("Matches(错误密码) = true")
	}
	// 零值必须谁也验不过：一个没被赋过哈希的容器如果放行，那就是一条
	// 登录绕过 —— 而它出现的场合恰好是「读路径忘了填」。
	var zero registry.PasswordHash
	if zero.Matches(password) || zero.Matches("") {
		t.Error("零值 PasswordHash 验过了密码，它必须谁也验不过")
	}
}

// 哈希不得从 PasswordHash 里以任何一条隐式路径漏出来。
//
// 三条路径一起断言，因为它们各自独立地会把不导出的 []byte 印出来：
// json.Marshal（会进响应体，规范 §20、§35）、fmt 的 %v/%s/%+v 与 %#v
// （会进日志，规范 §19、§21）。少钉一条，那条就是将来那次泄漏。
//
// 断言的是**哈希那串字符不出现**，不是"输出等于某个占位符"：后者在有人
// 把占位符改成哈希本身时照样绿。
func TestPasswordHashNeverPrintsItsBytes(t *testing.T) {
	raw, err := bcrypt.GenerateFromPassword([]byte("correct-horse-battery"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	secret := string(raw)
	h := registry.NewPasswordHash(secret)

	// 哈希的字节形态也要查：%v 把 []byte 印成 [36 50 97 ...]，那串数字
	// 不含 secret 的任何一个字符，只查字符串会把它整条放过去。
	rawBytes := []byte(secret)
	bytesForm := fmt.Sprintf("%v", rawBytes)

	// 内嵌进一个响应体形状的结构里：真正的泄漏长这个样子，而不是有人
	// 单独去序列化一个哈希。
	envelope := struct {
		Username string
		Hash     registry.PasswordHash
	}{Username: "alice", Hash: h}

	blob, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	rendered := []string{
		string(blob),
		// %v 与 %s 对实现了 Stringer 的类型走同一条路径，列一个就够；
		// h.String() 单列在下面，因此这里不再重复 %s。
		fmt.Sprintf("%v", h),
		fmt.Sprintf("%+v", h),
		fmt.Sprintf("%#v", h),
		fmt.Sprintf("%v", envelope),
		fmt.Sprintf("%+v", envelope),
		h.String(),
	}
	for _, out := range rendered {
		if strings.Contains(out, secret) || strings.Contains(out, bytesForm) {
			t.Errorf("PasswordHash 渲染成了 %q，其中带着密码哈希", out)
		}
	}

	// 反方向：类型的导出方法集必须恰好是这四个。少了这条，一个将来加上
	// Bytes() 的版本仍然能让上面全绿 —— 那时泄漏走的是那个方法，而不是
	// 格式化，而本类型的全部保证都建立在"没有出口"上。
	//
	// 断言的是集合相等而不是"不含 Bytes"：黑名单漏掉一个名字就是放行
	// 一个出口，而新增任何一个方法都应该是一次有人明确改过这条断言的
	// 决定。
	want := map[string]bool{
		"Matches": true, "String": true, "GoString": true, "MarshalJSON": true,
	}
	rt := reflect.TypeOf(registry.PasswordHash{})
	got := map[string]bool{}
	for i := range rt.NumMethod() {
		got[rt.Method(i).Name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PasswordHash 的导出方法集 = %v, want %v —— "+
			"新增的方法若能把哈希取出来，本类型的全部保证就作废了", got, want)
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
