package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// sessionIDBytes 是会话 ID 的随机字节数。
// 128 位熵，十六进制编码后 32 个字符，不可枚举。
const sessionIDBytes = 16

// ErrInvalidRole 表示试图用一个未登记的角色签发会话。
var ErrInvalidRole = errors.New("session requires a registered role")

// Session 是一次已认证的会话。
type Session struct {
	// ID 是会话标识，随机生成，作为 Cookie 值下发。
	ID string
	// Username 是会话归属的账号。
	Username string
	// Role 是该账号的角色。
	//
	// **权限只从这里来。** 它在签发会话时由服务端从账号记录取得，此后
	// 只在服务端内存里流动 —— 请求体、请求头、Cookie 与查询串里出现的
	// 任何 role/is_admin 字段都与它无关，也永远不会被读（规范 §9、§34）。
	// 会话本身就是那份可信状态。
	Role registry.Role
	// ExpiresAt 是过期时刻。
	ExpiresAt time.Time
}

// SessionStore 在内存中保存活跃会话。
//
// 内存存储对 demo 足够：进程重启后所有人重新登录，这是可接受的，
// 而引入外部存储会给演示环境增加一个必须先跑起来的依赖。
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
	ttl      time.Duration
	now      func() time.Time
}

// NewSessionStore 构造会话存储。now 可注入以保证测试确定性，传 nil 用 time.Now。
func NewSessionStore(ttl time.Duration, now func() time.Time) *SessionStore {
	if now == nil {
		now = time.Now
	}
	return &SessionStore{
		sessions: make(map[string]Session),
		ttl:      ttl,
		now:      now,
	}
}

// Create 为指定账号签发一个新会话。
//
// role 是必填参数而不是一个可以事后补上的字段：签不出一个没有角色的会话，
// 「忘了设角色」这条路径就不存在。未登记的角色（含零值）一律拒绝签发 ——
// 退一步给它一个默认角色，等于让一次装配失误变成一次静默的授权。
func (s *SessionStore) Create(username string, role registry.Role) (Session, error) {
	if !role.Valid() {
		return Session{}, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}

	raw := make([]byte, sessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, fmt.Errorf("generate session id: %w", err)
	}

	sess := Session{
		ID:        hex.EncodeToString(raw),
		Username:  username,
		Role:      role,
		ExpiresAt: s.now().Add(s.ttl),
	}

	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

	return sess, nil
}

// Get 返回未过期的会话。过期或不存在时第二个返回值为 false。
//
// 过期判断在读取时完成而非后台清理：demo 规模下会话数量很小，
// 一个后台 goroutine 带来的复杂度不值得。
func (s *SessionStore) Get(id string) (Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()

	if !ok {
		return Session{}, false
	}
	if !s.now().Before(sess.ExpiresAt) {
		s.Delete(id)
		return Session{}, false
	}
	return sess, true
}

// Delete 销毁一个会话。
//
// 登出必须销毁服务端会话，而不是仅清除 Cookie：只清 Cookie 的话，
// 已经泄露出去的会话 ID 仍然有效直到过期。
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}
