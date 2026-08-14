// Package auth 负责账号校验与会话生命周期。
package auth

import (
	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/config"
)

// dummyHash 是一个真实的 bcrypt 哈希，用于在用户不存在时仍走一次比对。
//
// 不这样做的话，未知用户会立即返回，而存在的用户要等一次 bcrypt 计算，
// 响应耗时的差异足以让攻击者枚举出哪些账号真实存在。
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

// account 是一条本地账号记录：密码哈希，以及它的角色。
//
// 角色跟着账号记录走，而不是跟着请求走 —— 会话在签发时从这里取它
// （见 Session.Role）。
type account struct {
	hash []byte
	role Role
}

// Verifier 校验本地账号。
type Verifier struct {
	accounts map[string]account
}

// NewVerifier 用配置中的账号列表构造校验器。
//
// 文件里的账号一律是管理员。这不是「所有人都是管理员」这条策略，而是
// 对当前形态的如实描述：config.AuthConfig 只有 BootstrapUser 一个字段，
// 文件里因此只可能有一个账号，而它按定义就是管理员 —— 平台的第一次登录
// 只能是它，此后的一切也只能由它来做。
//
// **第二个账号出现时，角色必须随账号一起来，不能继续在这里赋值。** 那时
// 账号在库里，每条记录自带角色；如果有人先给配置文件加出第二个账号，
// 这一行会静默地把它也变成管理员 —— 这正是把这段话写在这里的原因。
func NewVerifier(users []config.User) *Verifier {
	m := make(map[string]account, len(users))
	for _, u := range users {
		m[u.Username] = account{hash: []byte(u.PasswordHash), role: RoleAdmin}
	}
	return &Verifier{accounts: m}
}

// Verify 校验用户名与密码是否匹配，并返回该账号的角色。
//
// 无论用户是否存在都执行一次哈希比对，调用方无法从耗时或返回值
// 区分"用户不存在"与"密码错误"。
//
// 角色与校验结果一并返回，而不是让调用方通过后再查一次：会话必须带着
// 角色签发（见 SessionStore.Create），两次查找之间的缝隙正是「认证过了、
// 角色却是零值」的来源。校验失败时返回空角色，它不是任何一个合法取值。
func (v *Verifier) Verify(username, password string) (Role, bool) {
	acct, ok := v.accounts[username]
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", false
	}
	if bcrypt.CompareHashAndPassword(acct.hash, []byte(password)) != nil {
		return "", false
	}
	return acct.role, true
}
