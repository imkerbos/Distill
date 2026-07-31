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
//
//nolint:gosec // G101: 这不是凭据，是固定的 bcrypt 哈希，只用于拉平耗时。
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

// Verifier 校验本地账号。
type Verifier struct {
	hashes map[string][]byte
}

// NewVerifier 用配置中的账号列表构造校验器。
func NewVerifier(users []config.User) *Verifier {
	m := make(map[string][]byte, len(users))
	for _, u := range users {
		m[u.Username] = []byte(u.PasswordHash)
	}
	return &Verifier{hashes: m}
}

// Verify 校验用户名与密码是否匹配。
//
// 无论用户是否存在都执行一次哈希比对，调用方无法从耗时或返回值
// 区分"用户不存在"与"密码错误"。
func (v *Verifier) Verify(username, password string) bool {
	hash, ok := v.hashes[username]
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}
