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
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
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
	// code 与 status 同样必要：业务失败一律 200，日志里的 code 是运维
	// 统计业务失败率的唯一信号（spec §4.3）。
	for _, field := range []string{"request_id", "method", "path", "status", "code", "duration_ms"} {
		if _, ok := got[field]; !ok {
			t.Errorf("log line is missing %q: %v", field, got)
		}
	}
	if got["path"] != "/api/v1/flows" {
		t.Errorf("path = %v", got["path"])
	}
}

// 业务失败的 code 必须进日志，否则一次 200 + 20002 在两侧都看不见：
// 状态码统计看不到它，日志里也没有可告警的字段。
func TestRequestLoggerRecordsBusinessCode(t *testing.T) {
	var buf bytes.Buffer
	logger, err := applog.New("INFO", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h := httpapi.RequestID(httpapi.RequestLogger(logger)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			response.WriteBusiness(w, response.CodeNotFound)
		}),
	))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	if got["status"] != float64(200) {
		t.Errorf("status = %v, want 200", got["status"])
	}
	if got["code"] != float64(20002) {
		t.Errorf("code = %v, want 20002 — business failures are invisible to status-code alerts", got["code"])
	}
}

// panic 必须被拦成一个正常的 500 包络，并且仍然留下带 request_id 的日志：
// 让它冲出中间件栈，用户报障时手上的 request_id 会查不到任何东西。
func TestRecovererTurnsPanicIntoInternalError(t *testing.T) {
	var buf bytes.Buffer
	logger, err := applog.New("INFO", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h := httpapi.RequestID(httpapi.RequestLogger(logger)(httpapi.Recoverer(logger)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom at 10.0.0.5:9050")
		}),
	)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["code"] != float64(50001) {
		t.Errorf("code = %v, want 50001", body["code"])
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("the panic value reached the response: %s", rec.Body.String())
	}

	requestID := rec.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("no X-Request-Id on the response")
	}
	if !strings.Contains(buf.String(), requestID) {
		t.Errorf("no log line carries request_id %q; a user reporting it would find nothing:\n%s",
			requestID, buf.String())
	}
	if !strings.Contains(buf.String(), "request completed") {
		t.Errorf("a panicking request produced no completion log line:\n%s", buf.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("log line is not JSON: %q", line)
		}
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
	sess, err := sessions.Create("demo", registry.RoleAdmin)
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
	sess, _ := sessions.Create("demo", registry.RoleAdmin)

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
