package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/httpapi"
	"github.com/imkerbos/Distill/internal/response"
)

// 授权层今天挡不住任何真实调用方：配置文件里只有引导账号，而它是管理员。
// 这些用例因此都自己签一个只读会话 —— 它们验证的是「第二个身份出现时
// 会被挡住」，以及「漏了声明的端点是拒绝而不是放行」。

// sessionCookie 直接在会话存储里签发一个会话并取得它的 Cookie。
//
// 不走登录接口：今天没有任何一条登录路径能产出只读会话（见上）。这不是
// 绕过认证 —— 会话仍然由服务端签发，角色仍然只存在于服务端状态里。
func sessionCookie(t *testing.T, sessions *auth.SessionStore, user string, role auth.Role) *http.Cookie {
	t.Helper()
	sess, err := sessions.Create(user, role)
	if err != nil {
		t.Fatalf("Create session for %s: %v", role, err)
	}
	// 这是测试构造的**请求** Cookie，HttpOnly/Secure/SameSite 是响应头的
	// 概念，与它无关；gosec 的 G124 不区分请求与响应。
	return &http.Cookie{Name: httpapi.SessionCookieName, Value: sess.ID} //nolint:gosec // G124: request cookie
}

// callWith 发一次带 Cookie 的请求；cookie 为 nil 时不带会话。
func callWith(
	t *testing.T, h http.Handler, method, path string, cookie *http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	switch method {
	case http.MethodPost, http.MethodPut:
		body = strings.NewReader("{}")
	default:
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// adminRoutes 是必须要求管理员的路由。
//
// 与 router.go 里的声明分开手写，而不是从声明表反推：从被测对象推出期望，
// 那条断言无论声明改成什么都成立。
func adminRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodPost, "/api/v1/clusters"},
		{http.MethodPut, "/api/v1/clusters/prod-asia-1"},
		{http.MethodDelete, "/api/v1/clusters/prod-asia-1"},
		{http.MethodPut, "/api/v1/clusters/prod-asia-1/git-binding"},
		{http.MethodDelete, "/api/v1/clusters/prod-asia-1/git-binding"},
		{http.MethodPost, "/api/v1/clusters/prod-asia-1/git-binding/verify"},
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/policy-imports"},
		{http.MethodPost, "/api/v1/clusters/prod-asia-1/policy-imports"},
		{http.MethodDelete, "/api/v1/clusters/prod-asia-1/policy-imports/1"},
		{http.MethodPost, "/api/v1/clusters/prod-asia-1/rule-overrides"},
		{http.MethodDelete, "/api/v1/clusters/prod-asia-1/rule-overrides"},
	}
}

// viewerRoutes 是只读账号必须够得着的路由。
func viewerRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/api/v1/clusters"},
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/topology"},
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/quality"},
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/security"},
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/policy-preview"},
		{http.MethodGet, "/api/v1/flows"},
		{http.MethodGet, "/api/v1/flows/no-such-flow/decision"},
	}
}

func TestViewerIsRefusedOnAdminRoutes(t *testing.T) {
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	cookie := sessionCookie(t, sessions, "readonly", auth.RoleViewer)

	for _, rt := range adminRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := callWith(t, h, rt.method, rt.path, cookie)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — a viewer reached an administrator route", rec.Code)
			}
			if got := bodyOf(t, rec)["code"]; got != float64(response.CodeForbidden) {
				t.Errorf("code = %v, want %d", got, response.CodeForbidden)
			}
		})
	}
}

func TestViewerReachesReadRoutes(t *testing.T) {
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	cookie := sessionCookie(t, sessions, "readonly", auth.RoleViewer)

	for _, rt := range viewerRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := callWith(t, h, rt.method, rt.path, cookie)
			if rec.Code == http.StatusForbidden {
				t.Fatalf("a viewer was refused a read route: %s", rec.Body.String())
			}
		})
	}
}

// 管理员满足每一条声明，因此对任何**已声明**的路由都不会拿到 403。
// 反过来说，走到这里的 403 只可能来自「这条路由没有声明」——
// 这条用例因此同时是「新增端点忘了声明」的哨兵。
func TestAdminReachesEveryRegisteredRoute(t *testing.T) {
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())

	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("router is %T, which cannot be walked — this test can no longer see the route table", h)
	}

	// 路径参数的取值本身无关紧要：这里断言的是授权，不是 handler 的结论。
	params := map[string]string{
		"{clusterID}": "prod-asia-1",
		"{importID}":  "1",
		"{flowID}":    "no-such-flow",
	}

	walked := 0
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := route
		for name, value := range params {
			path = strings.ReplaceAll(path, name, value)
		}
		if strings.Contains(path, "{") {
			t.Errorf("route %s has a path parameter this test does not know how to fill", route)
			return nil
		}
		if method == http.MethodPost && path == "/api/v1/sessions" {
			// 登录是唯一无需会话的端点，不经过授权层。
			return nil
		}
		walked++

		// 每条路由用一个新签的会话：被走到的路由里有一条会销毁自己的会话，
		// 之后的请求就会变成 401 —— 而 401 不等于 403，这条断言会安静地
		// 不再检查任何东西。
		cookie := sessionCookie(t, sessions, "demo", auth.RoleAdmin)
		rec := callWith(t, h, method, path, cookie)
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s %s refused an administrator (%s) — the route is most likely missing its declaration",
				method, route, rec.Body.String())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// 走空的遍历会让上面每一条断言都不执行，而测试照样是绿的。
	if walked < len(adminRoutes())+len(viewerRoutes()) {
		t.Fatalf("walked %d protected routes, fewer than the %d this test names by hand",
			walked, len(adminRoutes())+len(viewerRoutes()))
	}
}

// 前端要靠这一点区分「重新登录」与「你不能做这件事」：两者的处置相反。
func TestRefusalIsDistinguishableFromUnauthenticated(t *testing.T) {
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())

	anon := callWith(t, h, http.MethodPost, "/api/v1/clusters", nil)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anon.Code)
	}
	if got := bodyOf(t, anon)["code"]; got != float64(response.CodeUnauthenticated) {
		t.Errorf("anonymous code = %v, want %d", got, response.CodeUnauthenticated)
	}

	viewer := callWith(t, h, http.MethodPost, "/api/v1/clusters",
		sessionCookie(t, sessions, "readonly", auth.RoleViewer))
	if viewer.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", viewer.Code)
	}
	if got := bodyOf(t, viewer)["code"]; got != float64(response.CodeForbidden) {
		t.Errorf("viewer code = %v, want %d", got, response.CodeForbidden)
	}

	if anon.Code == viewer.Code {
		t.Error("an insufficient role must not answer the same as no session at all")
	}
}

// 拒绝必须发生在 handler 之前：一次被拒的写请求不能留下任何痕迹。
func TestRefusedWriteNeverReachesTheHandler(t *testing.T) {
	reg := fixtureSource()
	before := len(reg.clusters)
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := callWith(t, h, http.MethodPost, "/api/v1/clusters",
		sessionCookie(t, sessions, "readonly", auth.RoleViewer))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(reg.clusters) != before {
		t.Errorf("the registry changed on a refused request: %d clusters, want %d", len(reg.clusters), before)
	}
}

// 会话自身的读取与销毁只要求一个有效会话：只读账号也要能看到自己是谁、
// 也要能登出。
func TestSessionEndpointsOnlyNeedAValidSession(t *testing.T) {
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	cookie := sessionCookie(t, sessions, "readonly", auth.RoleViewer)

	read := callWith(t, h, http.MethodGet, "/api/v1/sessions/current", cookie)
	if read.Code != http.StatusOK {
		t.Fatalf("GET /sessions/current status = %d, want 200: %s", read.Code, read.Body.String())
	}
	if got := bodyOf(t, read)["code"]; got != float64(response.CodeOK) {
		t.Errorf("code = %v, want 0", got)
	}

	out := callWith(t, h, http.MethodDelete, "/api/v1/sessions/current", cookie)
	if out.Code != http.StatusOK {
		t.Fatalf("DELETE /sessions/current status = %d, want 200: %s", out.Code, out.Body.String())
	}
	if _, alive := sessions.Get(cookie.Value); alive {
		t.Error("a viewer must be able to end its own session")
	}
}

// 登录签发的会话必须带着账号记录上的角色，而不是一个零值。
// 这条断言接的是「角色从哪里来」那一段：它只能来自服务端的账号记录。
func TestLoginIssuesASessionCarryingTheAccountRole(t *testing.T) {
	h, sessions, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())

	sess, ok := sessions.Get(cookie.Value)
	if !ok {
		t.Fatal("the login cookie does not resolve to a session")
	}
	if sess.Role != auth.RoleAdmin {
		t.Fatalf("session role = %q, want %q — the bootstrap account is the administrator",
			sess.Role, auth.RoleAdmin)
	}

	// 而且它确实够得着管理员路由。
	rec := callWith(t, h, http.MethodGet, "/api/v1/clusters/prod-asia-1/policy-imports", cookie)
	if rec.Code == http.StatusForbidden {
		t.Errorf("the bootstrap account was refused an administrator route: %s", rec.Body.String())
	}
}

// 请求体、请求头与查询串里的 role 一律不参与判定（规范 §9、§34）。
func TestRoleClaimsInTheRequestAreIgnored(t *testing.T) {
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	cookie := sessionCookie(t, sessions, "readonly", auth.RoleViewer)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters?role=ADMIN&is_admin=true",
		strings.NewReader(`{"role":"ADMIN","isAdmin":true,"id":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Role", "ADMIN")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a role claimed by the caller must not grant anything", rec.Code)
	}
}
