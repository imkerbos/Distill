package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/httpapi"
	applog "github.com/imkerbos/Distill/internal/log"
)

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	var seen string
	h := httpapi.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpapi.RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("handler saw an empty request id")
	}
	if got := rec.Header().Get("X-Request-Id"); got != seen {
		t.Errorf("X-Request-Id = %q, want %q — users report this id when something breaks", got, seen)
	}
}

func TestRequestLoggerEmitsJSONWithRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger, err := applog.New("INFO", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h := httpapi.RequestID(httpapi.RequestLogger(logger)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	for _, field := range []string{"request_id", "method", "path", "status", "duration_ms"} {
		if _, ok := got[field]; !ok {
			t.Errorf("log line is missing %q: %v", field, got)
		}
	}
	if got["path"] != "/api/v1/flows" {
		t.Errorf("path = %v", got["path"])
	}
}

// 密码与会话 token 绝不能出现在日志里。
func TestRequestLoggerNeverLogsSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := applog.New("INFO", &buf)

	h := httpapi.RequestID(httpapi.RequestLogger(logger)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
	))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions?password=hunter2", nil)
	// This is a request cookie the test fabricates to stand in for a leaked
	// session id; HttpOnly/Secure/SameSite are response-header concerns and
	// don't apply here — gosec's G124 doesn't distinguish request vs response.
	req.AddCookie(&http.Cookie{Name: "distill_session", Value: "super-secret-token"}) //nolint:gosec // G124: request cookie, see comment above
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	for _, secret := range []string{"hunter2", "super-secret-token"} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaked %q: %s", secret, out)
		}
	}
}

func TestRequireSessionRejectsAnonymous(t *testing.T) {
	sessions := auth.NewSessionStore(time.Hour, nil)
	h := httpapi.RequireSession(sessions)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler must not run without a session")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — the frontend redirects to login on this", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got["code"] != float64(10003) {
		t.Errorf("code = %v, want 10003", got["code"])
	}
}

func TestRequireSessionAcceptsValidCookie(t *testing.T) {
	sessions := auth.NewSessionStore(time.Hour, nil)
	sess, err := sessions.Create("demo")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var sawUser string
	h := httpapi.RequireSession(sessions)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := httpapi.SessionFrom(r.Context())
		if !ok {
			t.Error("session must be in the request context")
			return
		}
		sawUser = s.Username
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil)
	req.AddCookie(&http.Cookie{Name: "distill_session", Value: sess.ID}) //nolint:gosec // G124: request cookie, not a response header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if sawUser != "demo" {
		t.Errorf("session user = %q, want demo", sawUser)
	}
}

// 过期会话与未登录一样返回 401，但用 10002 让前端能提示"会话已过期"。
func TestRequireSessionRejectsExpired(t *testing.T) {
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	sessions := auth.NewSessionStore(time.Hour, clock)
	sess, _ := sessions.Create("demo")

	now = now.Add(2 * time.Hour)

	h := httpapi.RequireSession(sessions)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler must not run with an expired session")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil)
	req.AddCookie(&http.Cookie{Name: "distill_session", Value: sess.ID}) //nolint:gosec // G124: request cookie, not a response header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["code"] != float64(10002) {
		t.Errorf("code = %v, want 10002 so the UI can say the session expired", got["code"])
	}
}
