package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/httpapi"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// 授权层今天挡不住任何真实调用方：配置文件里只有引导账号，而它是管理员。
// 这些用例因此都自己签一个只读会话 —— 它们验证的是「第二个身份出现时
// 会被挡住」，以及「漏了声明的端点是拒绝而不是放行」。

// adminUser 是这些用例使用的库内管理员账号名。
//
// 不能取配置里的引导账号名（"demo"）：那个名字走的是引导分支，与账号表里
// 的行无关，而引导分支在库里出现启用中的管理员之后就自己关上了
// （见 auth.Verifier.ValidateNewUsername 与 design doc 2026-08-14 §2）。
const adminUser = "ops-admin"

// seededPassword 是测试账号的明文密码。只为让建号路径拿到一个合法的哈希，
// 这些用例都不走登录。
const seededPassword = "seeded-password-1234"

// sessionCookie 在账号表里建一个账号，为它签一个会话，并取得那个 Cookie。
//
// 不走登录接口：登录要先有明文密码，而这些用例关心的是"这个角色够不够得着
// 这条路由"，不是"密码对不对"。这不是绕过认证 —— 会话仍然由服务端签发。
//
// **账号必须真的落进 reg**：角色在每次请求上从账号记录现读
// （design doc 2026-08-14 §4）。只签会话不建账号的话，enforce 解析不出
// 角色，每条断言都会退化成 401 而不再检查授权。
func sessionCookie(
	t *testing.T, sessions *auth.SessionStore, reg *memRegistry, user string, role registry.Role,
) *http.Cookie {
	t.Helper()
	// 已经建过就不再建一次：同一个身份可能要签好几张会话，而账号表拒绝
	// 重复的用户名（用户名不复用，见 memRegistry.CreateAccount）。
	if _, exists, err := reg.Account(context.Background(), user); err != nil {
		t.Fatalf("read account %s: %v", user, err)
	} else if !exists {
		reg.withAccount(t, user, role, seededPassword)
	}
	sess, err := sessions.Create(user)
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
		// 设置的读与写都在这里：读回传凭据后端与 Secret Manager 项目 /
		// 前缀，属于平台自己的部署形态，不是只读账号该看的东西
		// （分类存疑时取更严的那一侧，见本次报告）。
		{http.MethodGet, "/api/v1/settings"},
		{http.MethodPut, "/api/v1/settings"},
		// 仓库同理：列表回传仓库地址、分支与凭据引用。
		{http.MethodGet, "/api/v1/git-repos"},
		{http.MethodPost, "/api/v1/git-repos"},
		{http.MethodPut, "/api/v1/git-repos/policies"},
		{http.MethodDelete, "/api/v1/git-repos/policies"},
		{http.MethodPost, "/api/v1/git-repos/policies/verify"},
		{http.MethodPost, "/api/v1/clusters"},
		{http.MethodPut, "/api/v1/clusters/prod-asia-1"},
		{http.MethodDelete, "/api/v1/clusters/prod-asia-1"},
		{http.MethodPut, "/api/v1/clusters/prod-asia-1/git-binding"},
		{http.MethodDelete, "/api/v1/clusters/prod-asia-1/git-binding"},
		{http.MethodPost, "/api/v1/clusters/prod-asia-1/git-binding/verify"},
		// 采集摘要：回传一个真实集群的资产盘点，以及平台在这个集群上哪几类
		// 资源没被授权读 —— 后者是一份「平台在哪里是瞎的」的清单。
		// 分类存疑时取更严的那一侧（见 router.go 上那段注释）。
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/collection"},
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/policy-imports"},
		{http.MethodPost, "/api/v1/clusters/prod-asia-1/policy-imports"},
		{http.MethodDelete, "/api/v1/clusters/prod-asia-1/policy-imports/1"},
		// 导出比它渲染的那份预览更严：交出去的是一份完整的网络策略文档，
		// 等同于集群网络结构的说明书（design doc 2026-08-14 §5）。
		{http.MethodGet, "/api/v1/clusters/prod-asia-1/policy-export"},
		// 写回两条都是管理员（design doc 2026-08-14 §9）。出计划也在内：
		// 它回传的是将要写进策略仓库的完整文件内容与目标分支，比导出多带
		// 一份"平台打算往哪里写"。
		{http.MethodPost, "/api/v1/clusters/prod-asia-1/policy-writeback/plan"},
		{http.MethodPost, "/api/v1/clusters/prod-asia-1/policy-writeback/push"},
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
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	cookie := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)

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
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	cookie := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)

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
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("router is %T, which cannot be walked — this test can no longer see the route table", h)
	}

	// 账号端点的路径参数指向一个专门的目标账号，而不是调用者自己：被走到
	// 的路由里有 DELETE 与 disable，拿调用者当目标会让它在遍历中途把自己
	// 删掉，之后每一条断言都变成 401 —— 而 401 不等于 403，断言会安静地
	// 不再检查任何东西。
	reg.withAccount(t, "target-user", registry.RoleViewer, seededPassword)

	// 路径参数的取值本身无关紧要：这里断言的是授权，不是 handler 的结论。
	params := map[string]string{
		"{clusterID}": "prod-asia-1",
		"{importID}":  "1",
		"{flowID}":    "no-such-flow",
		"{repoID}":    "policies",
		"{username}":  "target-user",
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
		cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)
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
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

	anon := callWith(t, h, http.MethodPost, "/api/v1/clusters", nil)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anon.Code)
	}
	if got := bodyOf(t, anon)["code"]; got != float64(response.CodeUnauthenticated) {
		t.Errorf("anonymous code = %v, want %d", got, response.CodeUnauthenticated)
	}

	viewer := callWith(t, h, http.MethodPost, "/api/v1/clusters",
		sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer))
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
		sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer))
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
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	cookie := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)

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

// 会话本身**不带角色**：它只携带身份，角色在每次判定时现读
// （design doc 2026-08-14 §4）。这条用例守的是那个方向 —— 判定结果必须
// 来自服务端的账号记录，而登录时抄下的一份内存值不再存在。
func TestSessionCarriesIdentityAndTheRoleIsResolvedPerRequest(t *testing.T) {
	reg := fixtureSource()
	h, sessions, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	sess, ok := sessions.Get(cookie.Value)
	if !ok {
		t.Fatal("the login cookie does not resolve to a session")
	}
	if sess.Username != "demo" {
		t.Fatalf("session username = %q, want demo", sess.Username)
	}

	// 引导账号在库里没有启用中的管理员时解析成管理员，因此够得着管理员路由。
	rec := callWith(t, h, http.MethodGet, "/api/v1/clusters/prod-asia-1/policy-imports", cookie)
	if rec.Code == http.StatusForbidden {
		t.Errorf("the bootstrap account was refused an administrator route: %s", rec.Body.String())
	}

	// 库里一出现启用中的管理员，引导闸门就关上，同一张会话在下一次请求
	// 立即失去权限 —— 不必有谁记得去撤销它（design doc §2、§4）。
	reg.withAccount(t, adminUser, registry.RoleAdmin, seededPassword)
	after := callWith(t, h, http.MethodGet, "/api/v1/clusters/prod-asia-1/policy-imports", cookie)
	if after.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — the bootstrap session must stop working once a real admin exists (%s)",
			after.Code, after.Body.String())
	}
}

// 请求体、请求头与查询串里的 role 一律不参与判定（规范 §9、§34）。
func TestRoleClaimsInTheRequestAreIgnored(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	cookie := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)

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
