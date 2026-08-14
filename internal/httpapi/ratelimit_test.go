package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/httpapi"
)

// 方向一：守卫本身有效 —— 到了上限就拒绝，不是拒绝一切。
func TestRateLimiterAllowsUpToLimitThenDenies(t *testing.T) {
	l := httpapi.NewRateLimiter(3, time.Minute, 16, nil)

	for i := range 3 {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attempt %d was denied below the limit", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Error("the 4th attempt was allowed past a limit of 3")
	}
	// 另一个来源不受牵连。
	if !l.Allow("5.6.7.8") {
		t.Error("a different source was denied; the counter is not per-key")
	}
}

func TestRateLimiterResetsAfterWindow(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	l := httpapi.NewRateLimiter(2, time.Minute, 16, func() time.Time { return now })

	l.Allow("1.2.3.4")
	l.Allow("1.2.3.4")
	if l.Allow("1.2.3.4") {
		t.Fatal("the limit did not hold inside the window")
	}

	now = now.Add(time.Minute + time.Second)
	if !l.Allow("1.2.3.4") {
		t.Error("the window never reset; a limited caller would stay locked out forever")
	}
}

// 内存上界：表撑满之后新键被**拒绝**，不是被放行。
//
// 放行等于把「表满」这个攻击者能主动制造的条件变成关掉限流的开关。
func TestRateLimiterFailsClosedWhenKeyTableIsFull(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	l := httpapi.NewRateLimiter(5, time.Minute, 4, func() time.Time { return now })

	for i := range 4 {
		if !l.Allow(fmt.Sprintf("10.0.0.%d", i)) {
			t.Fatalf("key %d was denied while the table still had room", i)
		}
	}
	if l.Allow("10.0.0.99") {
		t.Error("a new key was admitted past maxKeys — the table can grow without bound")
	}
	// 已登记的键仍然照常计数：满表不该把所有人一起冻住。
	if !l.Allow("10.0.0.0") {
		t.Error("an already-tracked key was denied merely because the table is full")
	}
}

// 上界会自愈：窗口过去之后空位被回收，新来源重新被接纳。
func TestRateLimiterReclaimsExpiredKeys(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	l := httpapi.NewRateLimiter(5, time.Minute, 4, func() time.Time { return now })

	for i := range 4 {
		l.Allow(fmt.Sprintf("10.0.0.%d", i))
	}
	if l.Allow("10.0.0.99") {
		t.Fatal("precondition failed: the table was not full")
	}

	now = now.Add(time.Minute + time.Second)
	if !l.Allow("10.0.0.99") {
		t.Error("expired keys were never reclaimed; the limiter stays wedged shut forever")
	}
}

// 方向二：装配点仍然生效 —— 登录端点确实挂着限流。
//
// 摘掉 router.go 里的 .With(loginLimiter.Middleware)，这条立刻变红：
// 全部 32 次尝试都会返回 200 + 10001。
func TestLoginIsRateLimited(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	sawOK, denied := false, false
	for range 32 {
		rec := postJSON(t, h, "/api/v1/sessions", map[string]string{
			"username": "demo", "password": "wrong",
		})
		switch rec.Code {
		case http.StatusOK:
			sawOK = true
		case http.StatusTooManyRequests:
			denied = true
			if got := bodyOf(t, rec)["code"]; got != float64(20003) {
				t.Errorf("code = %v, want 20003", got)
			}
		default:
			t.Fatalf("unexpected status %d", rec.Code)
		}
		if denied {
			break
		}
	}

	if !sawOK {
		t.Error("no attempt was ever let through; the limiter rejects from the first request")
	}
	if !denied {
		t.Fatal("32 consecutive failed logins were all accepted — login is not rate limited")
	}
}

// 计数键不认 X-Forwarded-For。
//
// 认它等于让攻击者每次请求换一个键：限流当场归零。
func TestLoginRateLimitIgnoresForwardedForSpoofing(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	denied := false
	for i := range 32 {
		body, err := json.Marshal(map[string]string{"username": "demo", "password": "wrong"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		// 每次换一个伪造来源。
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			denied = true
			break
		}
	}

	if !denied {
		t.Fatal("a client-supplied X-Forwarded-For reset the counter; the limit is bypassable")
	}
}

// 限流的回答不得区分「这个用户名存在」与「不存在」。
//
// 计数键只有来源地址，请求体压根没被限流器读过 —— 拒绝发生在
// handleCreateSession 运行之前，因此两种用户名拿到逐字节相同的响应。
func TestRateLimitResponseDoesNotRevealUsernameExistence(t *testing.T) {
	drain := func(username string) *httptest.ResponseRecorder {
		h, _, _ := newTestRouter(t, nil)
		for range 32 {
			rec := postJSON(t, h, "/api/v1/sessions", map[string]string{
				"username": username, "password": "wrong",
			})
			if rec.Code == http.StatusTooManyRequests {
				return rec
			}
		}
		t.Fatalf("never hit the limit for %q", username)
		return nil
	}

	known := drain("demo")   // 配置里存在
	ghost := drain("nobody") // 配置里不存在

	if known.Code != ghost.Code {
		t.Errorf("status differs: known=%d unknown=%d", known.Code, ghost.Code)
	}
	if known.Body.String() != ghost.Body.String() {
		t.Errorf("body differs — the limiter leaks whether a username exists:\n known=%s\nghost=%s",
			known.Body.String(), ghost.Body.String())
	}
}
