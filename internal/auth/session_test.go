package auth_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/registry"
)

func TestSessionCreateAndGet(t *testing.T) {
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	s := auth.NewSessionStore(time.Hour, func() time.Time { return now })

	sess, err := s.Create("demo")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("session ID is empty")
	}
	if sess.Username != "demo" {
		t.Errorf("Username = %q, want demo", sess.Username)
	}

	got, ok := s.Get(sess.ID)
	if !ok {
		t.Fatal("freshly created session must be retrievable")
	}
	if got.Username != "demo" {
		t.Errorf("Username = %q, want demo", got.Username)
	}
}

// 会话不得携带角色。
//
// 这条断言直接对着结构体本身，因为「角色被抄进会话」的症状不在任何一次
// 单独的调用里 —— 每次登录看起来都正常，差别只在一个账号被降权或停用
// **之后**，那张已经签发的会话还剩多久（design doc 2026-08-14 §4）。
// 字段一旦加回来，授权层就会去读它，而这里是能在编译产物上看见它的地方。
func TestSessionCarriesNoRole(t *testing.T) {
	st := reflect.TypeOf(auth.Session{})
	roleType := reflect.TypeOf(registry.Role(""))
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.Type == roleType || f.Name == "Role" {
			t.Errorf("auth.Session has field %s %s — the session carries identity only; "+
				"the role must be read from the account record at authorization time",
				f.Name, f.Type)
		}
	}
}

// 会话 ID 必须不可预测：可猜的 ID 等于把所有人的会话交出去。
func TestSessionIDsAreUnique(t *testing.T) {
	s := auth.NewSessionStore(time.Hour, nil)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		sess, err := s.Create("demo")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if seen[sess.ID] {
			t.Fatalf("duplicate session ID %q", sess.ID)
		}
		if len(sess.ID) < 32 {
			t.Fatalf("session ID %q is too short to resist guessing", sess.ID)
		}
		seen[sess.ID] = true
	}
}

func TestSessionExpires(t *testing.T) {
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := auth.NewSessionStore(time.Hour, clock)

	sess, err := s.Create("demo")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	now = now.Add(59 * time.Minute)
	if _, ok := s.Get(sess.ID); !ok {
		t.Error("session must still be valid before its TTL elapses")
	}

	now = now.Add(2 * time.Minute)
	if _, ok := s.Get(sess.ID); ok {
		t.Error("session must be invalid after its TTL elapses")
	}
}

func TestSessionDelete(t *testing.T) {
	s := auth.NewSessionStore(time.Hour, nil)
	sess, err := s.Create("demo")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.Delete(sess.ID)
	if _, ok := s.Get(sess.ID); ok {
		t.Error("deleted session must not be retrievable")
	}
}

func TestSessionGetUnknownID(t *testing.T) {
	s := auth.NewSessionStore(time.Hour, nil)
	if _, ok := s.Get("no-such-session"); ok {
		t.Error("unknown session ID must not resolve")
	}
}

// 会话存储会被多个请求并发访问。
func TestSessionStoreIsConcurrencySafe(t *testing.T) {
	s := auth.NewSessionStore(time.Hour, nil)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			sess, err := s.Create("demo")
			if err != nil {
				return
			}
			s.Get(sess.ID)
			s.Delete(sess.ID)
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}
