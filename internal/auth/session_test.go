package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/registry"
)

func TestSessionCreateAndGet(t *testing.T) {
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	s := auth.NewSessionStore(time.Hour, func() time.Time { return now })

	sess, err := s.Create("demo", registry.RoleAdmin)
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
	// 角色必须原样存活到读出来那一刻：授权层读的正是这个字段，
	// 一个在存取之间丢掉的角色会让每次判定都落到零值上。
	if got.Role != registry.RoleAdmin {
		t.Errorf("Role = %q, want %q", got.Role, registry.RoleAdmin)
	}
}

// 会话的角色由签发方给出，取什么值就存什么值 —— 不会被悄悄提权。
func TestSessionCreateKeepsTheGivenRole(t *testing.T) {
	s := auth.NewSessionStore(time.Hour, nil)
	sess, err := s.Create("readonly", registry.RoleViewer)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Role != registry.RoleViewer {
		t.Fatalf("Role = %q, want %q", sess.Role, registry.RoleViewer)
	}
	got, ok := s.Get(sess.ID)
	if !ok {
		t.Fatal("session must be retrievable")
	}
	if got.Role != registry.RoleViewer {
		t.Errorf("stored Role = %q, want %q — a viewer must not read back as anything else",
			got.Role, registry.RoleViewer)
	}
}

// 签不出没有角色的会话：那样的会话一旦存在，授权层就要为它的零值兜底。
func TestSessionCreateRejectsAnUnregisteredRole(t *testing.T) {
	s := auth.NewSessionStore(time.Hour, nil)
	for _, role := range []registry.Role{"", "SUPERADMIN", "admin"} {
		sess, err := s.Create("demo", role)
		if !errors.Is(err, auth.ErrInvalidRole) {
			t.Errorf("Create with role %q: err = %v, want ErrInvalidRole", role, err)
		}
		if sess.ID != "" {
			t.Errorf("Create with role %q returned a session anyway: %q", role, sess.ID)
		}
	}
}

// 会话 ID 必须不可预测：可猜的 ID 等于把所有人的会话交出去。
func TestSessionIDsAreUnique(t *testing.T) {
	s := auth.NewSessionStore(time.Hour, nil)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		sess, err := s.Create("demo", registry.RoleAdmin)
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

	sess, err := s.Create("demo", registry.RoleAdmin)
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
	sess, err := s.Create("demo", registry.RoleAdmin)
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
			sess, err := s.Create("demo", registry.RoleAdmin)
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
