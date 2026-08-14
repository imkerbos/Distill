package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/imkerbos/Distill/internal/httpapi"
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
	h := httpapi.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))

	assertSecurityHeaders(t, rec.Header(), "middleware")
}

// 说不清效果的头一律不设：这条把「顺手补一个」挡在评审之前。
// 要新增，先在 secheaders.go 里写明它拦住了什么，再改这里。
func TestSecurityHeadersSetsNothingElse(t *testing.T) {
	h := httpapi.SecurityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
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
