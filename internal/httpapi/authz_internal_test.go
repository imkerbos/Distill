package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// 未声明的路由必须被拒绝，而不是放行。
//
// 这是这一层现在就值得建的理由：今天只有一个管理员账号，角色判定挡不住
// 任何人；而「新增端点忘了声明」是从第一天起就会发生的事，它必须以一次
// 谁都调不通的失败暴露出来，而不是变成一个谁都能调的管理接口。
func TestUndeclaredRouteIsRefused(t *testing.T) {
	sessions := auth.NewSessionStore(time.Hour, nil)
	admin := mustSession(t, sessions, "demo", registry.RoleAdmin)

	var declaredRan, undeclaredRan bool
	az := newAuthorizer(apiPrefix)
	r := chi.NewRouter()
	r.Route(apiPrefix, func(api chi.Router) {
		api.Group(func(protected chi.Router) {
			protected.Use(RequireSession(sessions))
			protected.Use(az.enforce)
			// 对照组：同一套装配下，声明过的路由是通的 —— 否则下面那条
			// 403 只说明这个测试装配坏了，不说明默认拒绝生效。
			az.route(protected, http.MethodGet, "/declared", accessViewer,
				func(w http.ResponseWriter, _ *http.Request) {
					declaredRan = true
					response.WriteOK(w, nil)
				})
			// 绕过 az.route 直接挂上去 —— 正是「加了端点忘了声明」的写法。
			protected.Get("/undeclared", func(w http.ResponseWriter, _ *http.Request) {
				undeclaredRan = true
				response.WriteOK(w, nil)
			})
		})
	})

	if rec := callAs(r, http.MethodGet, apiPrefix+"/declared", admin); rec.Code != http.StatusOK {
		t.Fatalf("declared route status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !declaredRan {
		t.Fatal("the declared route's handler never ran; the rest of this test would prove nothing")
	}

	rec := callAs(r, http.MethodGet, apiPrefix+"/undeclared", admin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared route status = %d, want 403 — a route with no declaration must fail closed",
			rec.Code)
	}
	if undeclaredRan {
		t.Error("the undeclared route's handler ran; refusal must happen before the handler")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got["code"] != float64(response.CodeForbidden) {
		t.Errorf("code = %v, want %d", got["code"], response.CodeForbidden)
	}
}

// 声明是按方法记的：同一个地址上 GET 与 DELETE 是两件不同性质的事，
// 给一个方法开的口子不能顺带把另一个方法也放开。
func TestDeclarationIsPerMethod(t *testing.T) {
	sessions := auth.NewSessionStore(time.Hour, nil)
	admin := mustSession(t, sessions, "demo", registry.RoleAdmin)

	az := newAuthorizer(apiPrefix)
	r := chi.NewRouter()
	r.Route(apiPrefix, func(api chi.Router) {
		api.Group(func(protected chi.Router) {
			protected.Use(RequireSession(sessions))
			protected.Use(az.enforce)
			az.route(protected, http.MethodGet, "/thing", accessViewer,
				func(w http.ResponseWriter, _ *http.Request) { response.WriteOK(w, nil) })
			protected.Delete("/thing", func(w http.ResponseWriter, _ *http.Request) {
				t.Error("the undeclared DELETE must not run")
			})
		})
	})

	if rec := callAs(r, http.MethodGet, apiPrefix+"/thing", admin); rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	if rec := callAs(r, http.MethodDelete, apiPrefix+"/thing", admin); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE status = %d, want 403 — the declaration on GET must not cover DELETE", rec.Code)
	}
}

// 没有会话就走不到授权：enforce 装错位置时必须报未认证，而不是拿一个
// 零值角色去判定。
func TestEnforceWithoutASessionIsUnauthenticated(t *testing.T) {
	az := newAuthorizer(apiPrefix)
	h := az.enforce(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler must not run without a session in the context")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, apiPrefix+"/clusters", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAccessPermits(t *testing.T) {
	cases := []struct {
		name string
		acc  access
		role registry.Role
		want bool
	}{
		{"session accepts an admin", accessSession, registry.RoleAdmin, true},
		{"session accepts a viewer", accessSession, registry.RoleViewer, true},
		{"session refuses a roleless session", accessSession, registry.Role(""), false},
		{"viewer accepts a viewer", accessViewer, registry.RoleViewer, true},
		{"viewer accepts an admin", accessViewer, registry.RoleAdmin, true},
		{"admin refuses a viewer", accessAdmin, registry.RoleViewer, false},
		{"admin accepts an admin", accessAdmin, registry.RoleAdmin, true},
		{"an unregistered requirement refuses everyone", access(0), registry.RoleAdmin, false},
		{"an unregistered requirement refuses viewers too", access(99), registry.RoleViewer, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.acc.permits(tc.role); got != tc.want {
				t.Errorf("access(%d).permits(%q) = %v, want %v", tc.acc, tc.role, got, tc.want)
			}
		})
	}
}

// mustSession 签发一个会话并返回它的 Cookie。
func mustSession(t *testing.T, sessions *auth.SessionStore, user string, role registry.Role) *http.Cookie {
	t.Helper()
	sess, err := sessions.Create(user, role)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 请求 Cookie，不是响应头：G124 的那几个属性在这里没有意义。
	return &http.Cookie{Name: SessionCookieName, Value: sess.ID} //nolint:gosec // G124: request cookie
}

// callAs 发一次带会话的请求。
func callAs(h http.Handler, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
