package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// stubRoles 是一份写死的账号名到角色的映射，充当 roleResolver。
//
// 不在这里接真的 *auth.Verifier：这些用例验证的是「声明表怎么被执行」，
// 而角色从哪来在 internal/auth 与账号端点的用例里各有自己的覆盖。
type stubRoles struct {
	roles map[string]registry.Role
	err   error
}

func (s stubRoles) RoleOf(_ context.Context, username string) (registry.Role, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	r, ok := s.roles[username]
	return r, ok, nil
}

// testAuthorizer 构造一个带写死角色表与丢弃式日志器的授权器。
func testAuthorizer(t *testing.T, roles roleResolver) *authorizer {
	t.Helper()
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return newAuthorizer(apiPrefix, roles, logger)
}

// 未声明的路由必须被拒绝，而不是放行。
//
// 这是这一层现在就值得建的理由：今天只有一个管理员账号，角色判定挡不住
// 任何人；而「新增端点忘了声明」是从第一天起就会发生的事，它必须以一次
// 谁都调不通的失败暴露出来，而不是变成一个谁都能调的管理接口。
func TestUndeclaredRouteIsRefused(t *testing.T) {
	sessions := auth.NewSessionStore(time.Hour, nil)
	admin := mustSession(t, sessions, "demo")

	var declaredRan, undeclaredRan bool
	az := testAuthorizer(t, stubRoles{roles: map[string]registry.Role{"demo": registry.RoleAdmin}})
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
	admin := mustSession(t, sessions, "demo")

	az := testAuthorizer(t, stubRoles{roles: map[string]registry.Role{"demo": registry.RoleAdmin}})
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
	az := testAuthorizer(t, stubRoles{})
	h := az.enforce(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler must not run without a session in the context")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, apiPrefix+"/clusters", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// 角色解析出来的两种"不成立"必须走两条不同的出口，且都不放行。
//
// 停用与软删除让会话立即失效（401，回登录页）；账号表读不出来是一次依赖
// 故障（500），不能被答成"你权限不足"——那会让操作者去找管理员要一个他
// 本来就有的权限。两者都不是 403，而 403 是这个中间件最常见的答复，因此
// 必须有东西钉住它们不会退化成 403，也不会退化成放行（规范 §49）。
func TestEnforceRefusesWhenTheRoleDoesNotResolve(t *testing.T) {
	cases := []struct {
		name       string
		roles      stubRoles
		wantStatus int
		wantCode   response.Code
	}{
		{
			name:       "disabled or deleted account",
			roles:      stubRoles{roles: map[string]registry.Role{}},
			wantStatus: http.StatusUnauthorized,
			wantCode:   response.CodeSessionExpired,
		},
		{
			name:       "the account table cannot be read",
			roles:      stubRoles{err: errors.New("database is down")},
			wantStatus: http.StatusInternalServerError,
			wantCode:   response.CodeInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := auth.NewSessionStore(time.Hour, nil)
			cookie := mustSession(t, sessions, "ghost")

			az := testAuthorizer(t, tc.roles)
			r := chi.NewRouter()
			r.Route(apiPrefix, func(api chi.Router) {
				api.Group(func(protected chi.Router) {
					protected.Use(RequireSession(sessions))
					protected.Use(az.enforce)
					az.route(protected, http.MethodGet, "/thing", accessViewer,
						func(w http.ResponseWriter, _ *http.Request) {
							t.Error("the handler must not run when the role does not resolve")
							response.WriteOK(w, nil)
						})
				})
			})

			rec := callAs(r, http.MethodGet, apiPrefix+"/thing", cookie)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if got["code"] != float64(tc.wantCode) {
				t.Errorf("code = %v, want %d", got["code"], tc.wantCode)
			}
			// 依赖故障的原因不回给调用方（规范 §22）。
			if strings.Contains(rec.Body.String(), "database is down") {
				t.Errorf("the response leaked the dependency failure: %s", rec.Body.String())
			}
		})
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
//
// 不带角色：会话只携带身份，角色在每次判定时现读（design doc 2026-08-14 §4）。
func mustSession(t *testing.T, sessions *auth.SessionStore, user string) *http.Cookie {
	t.Helper()
	sess, err := sessions.Create(user)
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
