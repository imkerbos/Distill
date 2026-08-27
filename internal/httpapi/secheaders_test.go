package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/imkerbos/Distill/internal/httpapi"
	applog "github.com/imkerbos/Distill/internal/log"
)

// wantSecurityHeaders 是这一层承诺的全部内容。
//
// 值写成字面量而不是从被测代码里取：从实现里读期望值的测试，
// 会跟着实现一起被改坏，然后继续绿着。
var wantSecurityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; " +
		"base-uri 'none'; form-action 'none'",
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "no-referrer",
	"X-Frame-Options":        "DENY",
}

func assertSecurityHeaders(t *testing.T, h http.Header, where string) {
	t.Helper()
	for name, want := range wantSecurityHeaders {
		if got := h.Get(name); got != want {
			t.Errorf("%s: %s = %q, want %q", where, name, got, want)
		}
	}
}

// 方向一：守卫本身有效。
func TestSecurityHeadersAreSet(t *testing.T) {
	h := httpapi.SecurityHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))

	assertSecurityHeaders(t, rec.Header(), "middleware")
}

// 说不清效果的头一律不设：这条把「顺手补一个」挡在评审之前。
// 要新增，先在 secheaders.go 里写明它拦住了什么，再改这里。
func TestSecurityHeadersSetsNothingElse(t *testing.T) {
	h := httpapi.SecurityHeaders(false)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for _, unset := range []string{"Strict-Transport-Security", "Permissions-Policy"} {
		if got := rec.Header().Get(unset); got != "" {
			t.Errorf("%s = %q; secheaders.go documents why it is deliberately unset", unset, got)
		}
	}
}

// 方向二：装配点仍然生效，且覆盖不经过 handler 的响应。
//
// 摘掉 router.go 里的 r.Use(SecurityHeaders)，这四个子用例同时变红。
func TestRouterSetsSecurityHeadersOnEveryResponse(t *testing.T) {
	h, _, cookie := newTestRouter(t, nil)

	cases := []struct {
		name string
		req  func() *http.Request
	}{
		{"login", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
			return r
		}},
		{"authenticated", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/current", nil)
			r.AddCookie(cookie)
			return r
		}},
		{"unauthenticated 401", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/api/v1/sessions/current", nil)
		}},
		{"404", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.req())
			assertSecurityHeaders(t, rec.Header(), tc.name)
		})
	}
}

// 带前端的部署要用另一份 CSP。
//
// 这条用例来自第一次真部署：CSP 是按「只产出 JSON」写的 default-src 'none'，
// 而同一个进程开始服务前端之后，它把自己的脚本、样式和图标全挡了 ——
// 界面渲染成一片空白，控制台里全是违规。本机开发形态（前端跑在独立的
// vite server 上）永远碰不到这条路径。
func TestWebUICSPAllowsWhatTheFrontendNeeds(t *testing.T) {
	h := httpapi.SecurityHeaders(true)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	// 少任何一条，界面上就少一类东西 —— 而 style 那条少了最阴：
	// 页面会以无样式的形态渲染出来，看起来是"打开了"。
	for _, need := range []string{
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"font-src 'self'",
	} {
		if !strings.Contains(csp, need) {
			t.Errorf("CSP 缺少 %q\n实际: %s", need, csp)
		}
	}
}

// 放行是逐条的，不是整体放宽。
//
// 退回 default-src 'self' 会连带放开 object-src、frame-src 这些前端根本
// 不用的取值，而它们正是注入之后最好用的几条路径。
func TestWebUICSPStaysDenyByDefault(t *testing.T) {
	h := httpapi.SecurityHeaders(true)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("兜底不再是 default-src 'none'：%s", csp)
	}
	// 框架防护与那两条注入路径在两份策略里都必须在。
	for _, need := range []string{"frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, need) {
			t.Errorf("带前端的 CSP 丢了 %q：%s", need, csp)
		}
	}
}

// 只产出 JSON 的部署不受影响：它配得上最严的那一份。
func TestAPIOnlyCSPIsUnchanged(t *testing.T) {
	h := httpapi.SecurityHeaders(false)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("Content-Security-Policy"); got != wantSecurityHeaders["Content-Security-Policy"] {
		t.Errorf("只产出 JSON 的 CSP 变了：\n got %s\nwant %s",
			got, wantSecurityHeaders["Content-Security-Policy"])
	}
}

// 路由必须按这次部署带不带前端来选 CSP。
//
// 上面几条都直接调 SecurityHeaders(true)，绕过了「路由到底传了什么」——
// 实测把 router.go 里那个参数写死成 false，它们照样全绿，而界面会白屏。
func TestRouterPicksTheCSPByWhetherItServesTheWebUI(t *testing.T) {
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	cspOf := func(d httpapi.Deps) string {
		rec := httptest.NewRecorder()
		httpapi.NewRouter(d).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Header().Get("Content-Security-Policy")
	}

	withUI := cspOf(httpapi.Deps{Logger: logger, WebUI: fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html>")},
	}})
	if !strings.Contains(withUI, "script-src 'self'") {
		t.Errorf("带前端的部署拿到的是只产出 JSON 那份 CSP，界面会白屏：%s", withUI)
	}

	withoutUI := cspOf(httpapi.Deps{Logger: logger})
	if withoutUI != wantSecurityHeaders["Content-Security-Policy"] {
		t.Errorf("不带前端的部署 CSP 变了：\n got %s\nwant %s",
			withoutUI, wantSecurityHeaders["Content-Security-Policy"])
	}
}
