package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// sessionIDBytes 是会话 ID 的随机字节数。
// 128 位熵，十六进制编码后 32 个字符，不可枚举。
const sessionIDBytes = 16

// Session 是一次已认证的会话。
//
// **它只携带身份，不携带角色。** 权限从账号记录现读（见 Verifier.RoleOf）：
// 会话在签发时抄一份角色的话，一个刚被降权或停用的账号会继续拿着原权限
// 直到会话过期，默认 8 小时（design doc 2026-08-14 §4）。
//
// 身份本身仍然只从这里来：请求体、请求头、Cookie 与查询串里出现的任何
// username/role/is_admin 字段都与授权无关，也永远不会被读（规范 §9、§34）。
type Session struct {
	// ID 是会话标识，随机生成，作为 Cookie 值下发。
	ID string
	// Username 是会话归属的账号，也是现读角色时的查找键。
	Username string
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
// 不再接收角色：签发时不知道、也不需要知道这个账号能做什么。授权在每次
// 请求上现读账号记录（见 Verifier.RoleOf），这里只记下"是谁"。
func (s *SessionStore) Create(username string) (Session, error) {
	raw := make([]byte, sessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, fmt.Errorf("generate session id: %w", err)
	}

	sess := Session{
		ID:        hex.EncodeToString(raw),
		Username:  username,
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
