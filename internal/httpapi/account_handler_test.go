package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// 账号端点的用例统一用这两个明文密码。它们都过 registry.ValidatePassword
// 的 12 字符下限 —— 用一个更短的密码会让某些用例在校验那一步就返回，
// 而它们要断言的是后面的事。
const (
	viewerPassword = "viewer-password-1234"
	newPassword    = "brand-new-password-5678"
)

// bcryptPrefixes 是 bcrypt 输出可能的前缀。
//
// 断言"响应里没有哈希"时按前缀找，而不是只找那一条存下来的哈希：只比对
// 已知的那一串，一条把**别的**账号的哈希带出去的响应会安然通过。
var bcryptPrefixes = []string{"$2a$", "$2b$", "$2y$"}

// assertNoHash 断言一段响应体里既没有 bcrypt 哈希、也没有明文密码。
func assertNoHash(t *testing.T, where, body string, plaintexts ...string) {
	t.Helper()
	for _, p := range bcryptPrefixes {
		if strings.Contains(body, p) {
			t.Errorf("%s carried a bcrypt hash: %s", where, body)
		}
	}
	for _, p := range plaintexts {
		if p != "" && strings.Contains(body, p) {
			t.Errorf("%s carried a plaintext password %q: %s", where, p, body)
		}
	}
}

// 账号的任何一条响应都不能带出密码哈希或明文（规范 §19、§20、§35；
// design doc 2026-08-14 §6）。
//
// 覆盖全部四条会回传账号信息的路径，而不是只覆盖列表：漏掉的那一条就是
// 会被挑中的那一条。断言同时比对**库里真实存下的那串哈希**，这样即便
// 将来有人给响应加一个名字看起来无害的字段，只要它承载的是哈希，这里
// 就会红。
func TestAccountResponsesNeverCarryAHash(t *testing.T) {
	reg := fixtureSource()
	h, sessions, bootstrap := newTestRouterWithRegistry(t, fixtureReader(), reg)
	reg.withAccount(t, "alice", registry.RoleViewer, viewerPassword)
	aliceCookie := sessionCookie(t, sessions, reg, "alice", registry.RoleViewer)
	storedHash := reg.accounts["alice"].hash
	if storedHash == "" {
		t.Fatal("the fixture account has no stored hash; the rest of this test would prove nothing")
	}

	list := authedGet(t, h, bootstrap, "/api/v1/accounts")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", list.Code, list.Body.String())
	}
	// 列表确实回了 alice —— 否则下面那条"没有哈希"的断言只是在一个空
	// 列表上成立。
	if !strings.Contains(list.Body.String(), "alice") {
		t.Fatalf("the account list does not contain alice: %s", list.Body.String())
	}
	assertNoHash(t, "the account list", list.Body.String(), viewerPassword, storedHash)

	created := authedPostJSON(t, h, bootstrap, "/api/v1/accounts", map[string]string{
		"username": "bob", "password": newPassword,
	})
	if got := bodyOf(t, created)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("create code = %v, want 0: %s", got, created.Body.String())
	}
	assertNoHash(t, "the create response", created.Body.String(), newPassword, reg.accounts["bob"].hash)

	reset := authedPostJSON(t, h, bootstrap, "/api/v1/accounts/alice/password", map[string]string{
		"password": newPassword,
	})
	assertNoHash(t, "the reset response", reset.Body.String(), newPassword, reg.accounts["alice"].hash)

	own := authedPostJSON(t, h, aliceCookie, "/api/v1/me/password", map[string]string{
		"currentPassword": newPassword, "newPassword": viewerPassword,
	})
	if got := bodyOf(t, own)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("own-password code = %v, want 0: %s", got, own.Body.String())
	}
	assertNoHash(t, "the own-password response", own.Body.String(), newPassword, reg.accounts["alice"].hash)
}

// 改自己的密码必须提供当前密码（规范 §28）：一台没锁屏的机器留下的会话
// 不该足以把账号的控制权永久转移走。
func TestChangingOwnPasswordRequiresTheCurrentOne(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	reg.withAccount(t, "alice", registry.RoleViewer, viewerPassword)
	cookie := sessionCookie(t, sessions, reg, "alice", registry.RoleViewer)
	before := reg.accounts["alice"].hash

	wrong := authedPostJSON(t, h, cookie, "/api/v1/me/password", map[string]string{
		"currentPassword": "not-the-current-password", "newPassword": newPassword,
	})
	if wrong.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a wrong password is a business failure", wrong.Code)
	}
	if got := bodyOf(t, wrong)["code"]; got != float64(response.CodeInvalidCredentials) {
		t.Errorf("code = %v, want %d", got, response.CodeInvalidCredentials)
	}
	// 拒绝必须发生在写入之前：一条只看响应码的断言在"先写库再报错"的
	// 实现下同样通过。
	if reg.accounts["alice"].hash != before {
		t.Fatal("the stored password changed even though the current one was wrong")
	}
	// 旧密码仍然可用 —— 这是操作者真正在意的那一半。
	if !registry.NewPasswordHash(reg.accounts["alice"].hash).Matches(viewerPassword) {
		t.Error("the old password no longer works after a refused change")
	}

	right := authedPostJSON(t, h, cookie, "/api/v1/me/password", map[string]string{
		"currentPassword": viewerPassword, "newPassword": newPassword,
	})
	if got := bodyOf(t, right)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0 — the correct current password must be accepted: %s",
			got, right.Body.String())
	}
	if !registry.NewPasswordHash(reg.accounts["alice"].hash).Matches(newPassword) {
		t.Error("the new password does not work after a successful change")
	}
}

// accountRoutes 是账号管理的全部端点，手写而不是从路由表反推：从被测对象
// 推出期望，那条断言无论声明改成什么都成立。
func accountRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/api/v1/accounts"},
		{http.MethodPost, "/api/v1/accounts"},
		{http.MethodPut, "/api/v1/accounts/alice/role"},
		{http.MethodPost, "/api/v1/accounts/alice/disable"},
		{http.MethodPost, "/api/v1/accounts/alice/enable"},
		{http.MethodDelete, "/api/v1/accounts/alice"},
		{http.MethodPost, "/api/v1/accounts/alice/password"},
	}
}

// 只读账号打管理端点必须是 403，且与"未登录"的 401 可分：前端对这两者的
// 处置完全相反（design doc §6，规范 §7）。
func TestViewerIsRefusedOnAccountEndpoints(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	reg.withAccount(t, "alice", registry.RoleViewer, viewerPassword)
	viewer := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)

	for _, rt := range accountRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			refused := callWith(t, h, rt.method, rt.path, viewer)
			if refused.Code != http.StatusForbidden {
				t.Fatalf("viewer status = %d, want 403 (%s)", refused.Code, refused.Body.String())
			}
			if got := bodyOf(t, refused)["code"]; got != float64(response.CodeForbidden) {
				t.Errorf("viewer code = %v, want %d", got, response.CodeForbidden)
			}

			anon := callWith(t, h, rt.method, rt.path, nil)
			if anon.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous status = %d, want 401", anon.Code)
			}
			if got := bodyOf(t, anon)["code"]; got != float64(response.CodeUnauthenticated) {
				t.Errorf("anonymous code = %v, want %d", got, response.CodeUnauthenticated)
			}
			if anon.Code == refused.Code {
				t.Error("an insufficient role must not answer the same as no session at all")
			}
		})
	}

	// 拒绝发生在 handler 之前：一次被拒的请求不能改动任何东西。
	if reg.accounts["alice"].account.DisabledAt != nil {
		t.Error("a refused disable request still disabled the account")
	}
}

// logLines 把日志缓冲区里的每一行 JSON 解出来。
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
		out = append(out, got)
	}
	return out
}

// completionLine 取出某条路径的完成日志。
func completionLine(t *testing.T, buf *bytes.Buffer, path string) map[string]any {
	t.Helper()
	for _, line := range logLines(t, buf) {
		if line["msg"] == "request completed" && line["path"] == path {
			return line
		}
	}
	t.Fatalf("no completion log line for %s:\n%s", path, buf.String())
	return nil
}

// 请求日志必须答得出"谁"。
//
// 这个字段此前是一条死分支：RequestLogger 装在路由根部，而会话是
// RequireSession 往下游 context 里放的，上游的 r 永远看不到它
// （design doc 2026-08-14 §7）。断言必须读**真实的日志输出**——从会话
// 存储或 handler 反推，等于让测试自己回答自己的问题。
func TestRequestLogCarriesTheAccountName(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _, logs := newTestRouterWithLog(t, reg)
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	rec := callWith(t, h, http.MethodGet, "/api/v1/accounts", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	line := completionLine(t, logs, "/api/v1/accounts")
	if line["user"] != adminUser {
		t.Errorf("log user = %v, want %q — an authenticated request must be attributable:\n%s",
			line["user"], adminUser, logs.String())
	}

	// 未认证的请求不写这个字段，而不是写一个空串：日志聚合会把 "" 当成
	// 一个真实存在的账号名。
	anon := callWith(t, h, http.MethodGet, "/api/v1/flows", nil)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anon.Code)
	}
	if _, present := completionLine(t, logs, "/api/v1/flows")["user"]; present {
		t.Error("an anonymous request produced a log line carrying a user field")
	}
}

// 授权拒绝同样要能对上账号：没有这一条，「最后一个管理员」那条保护即使
// 触发，事后也说不清是谁触发的（design doc 2026-08-14 §7）。
func TestAuthorizationRefusalIsAttributableToAnAccount(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _, logs := newTestRouterWithLog(t, reg)
	cookie := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)

	rec := callWith(t, h, http.MethodGet, "/api/v1/accounts", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	line := completionLine(t, logs, "/api/v1/accounts")
	if line["status"] != float64(http.StatusForbidden) {
		t.Fatalf("logged status = %v, want 403 — this is not the refusal's log line:\n%s",
			line["status"], logs.String())
	}
	if line["code"] != float64(response.CodeForbidden) {
		t.Errorf("logged code = %v, want %d", line["code"], response.CodeForbidden)
	}
	if line["user"] != "readonly" {
		t.Errorf("log user = %v, want readonly — a refusal nobody can be traced to is not an audit record:\n%s",
			line["user"], logs.String())
	}
	// 日志里不能出现凭据。
	assertNoHash(t, "the request log", logs.String(), seededPassword, reg.accounts["readonly"].hash)
}

// 与配置里的引导账号撞名的账号必须被拒绝。
//
// 撞名不是不好看：Verify 与 RoleOf 都先看引导分支，于是这个账号既永远
// 登不进来，又会在引导闸门开着时被解析成管理员——一个角色写着 VIEWER
// 的行拿到 ADMIN（见 auth.Verifier.ValidateNewUsername）。
func TestCreateAccountRefusesTheBootstrapName(t *testing.T) {
	reg := fixtureSource()
	h, _, bootstrap := newTestRouterWithRegistry(t, fixtureReader(), reg)

	for _, name := range []string{"demo", "DEMO", "Demo"} {
		t.Run(name, func(t *testing.T) {
			rec := authedPostJSON(t, h, bootstrap, "/api/v1/accounts", map[string]string{
				"username": name, "password": newPassword,
			})
			if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
				t.Errorf("code = %v, want %d — the bootstrap name must be refused: %s",
					got, response.CodeInvalidParam, rec.Body.String())
			}
			if _, created := reg.accounts[name]; created {
				t.Errorf("account %q was created despite colliding with the bootstrap account", name)
			}
		})
	}
}

// 新建账号一律是只读：请求体里根本没有 role 这个字段，提权只能走改角色
// 那一个端点，因此永远是一次单独的、有自己审计动作的操作
// （design doc 2026-08-14 §6）。
func TestCreatingAnAccountCannotMintAnAdministrator(t *testing.T) {
	reg := fixtureSource()
	h, _, bootstrap := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, bootstrap, "/api/v1/accounts", map[string]any{
		"username": "carol", "password": newPassword,
		// 调用方自称要的角色。请求形状里没有这个字段，它不该有任何效果。
		"role": string(registry.RoleAdmin),
	})
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}
	if got := reg.accounts["carol"].account.Role; got != registry.RoleViewer {
		t.Fatalf("role = %q, want %q — a role claimed in the request body must not be adopted",
			got, registry.RoleViewer)
	}

	// 改角色那一个端点确实能把它提上去 —— 否则上面那条断言只说明这个
	// 平台建不出管理员。
	up := authedPutJSON(t, h, bootstrap, "/api/v1/accounts/carol/role",
		map[string]string{"role": string(registry.RoleAdmin)})
	if got := bodyOf(t, up)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("update-role code = %v, want 0: %s", got, up.Body.String())
	}
	if got := reg.accounts["carol"].account.Role; got != registry.RoleAdmin {
		t.Errorf("role = %q, want %q", got, registry.RoleAdmin)
	}
}

// 密码强度在服务端把关，且拒绝是一句调用方能据以行动的话
// （registry.ValidatePassword，规范 §3、§34）。
func TestWeakPasswordsAreRefusedByTheServer(t *testing.T) {
	reg := fixtureSource()
	h, _, bootstrap := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, bootstrap, "/api/v1/accounts", map[string]string{
		"username": "dave", "password": "short",
	})
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d: %s", got, response.CodeInvalidParam, rec.Body.String())
	}
	if _, created := reg.accounts["dave"]; created {
		t.Error("an account was created with a password below the minimum length")
	}
}

// 不得把自己锁在门外：拒绝必须是一个调用方能据以行动的业务失败，不是 500
// （design doc 2026-08-14 §5）。
//
// 三条路径各测一次：停用、软删除、降级。它们在存储层是三个方法，边界层
// 也就有三处要把 ErrLastAdmin 认出来。
func TestRemovingTheLastAdminIsABusinessFailureNotA500(t *testing.T) {
	cases := []struct {
		name string
		call func(t *testing.T, h http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder
	}{
		{"disable", func(t *testing.T, h http.Handler, c *http.Cookie) *httptest.ResponseRecorder {
			return authedPostJSON(t, h, c, "/api/v1/accounts/"+adminUser+"/disable", map[string]string{})
		}},
		{"delete", func(t *testing.T, h http.Handler, c *http.Cookie) *httptest.ResponseRecorder {
			return authedDelete(t, h, c, "/api/v1/accounts/"+adminUser)
		}},
		{"demote", func(t *testing.T, h http.Handler, c *http.Cookie) *httptest.ResponseRecorder {
			return authedPutJSON(t, h, c, "/api/v1/accounts/"+adminUser+"/role",
				map[string]string{"role": string(registry.RoleViewer)})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := fixtureSource()
			h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
			cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

			rec := tc.call(t, h, cookie)
			if rec.Code != http.StatusInternalServerError {
				// 只断言"不是 500"还不够：那在一次 404 上同样成立。
				if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
					t.Errorf("code = %v, want %d — the refusal must say what to do about it: %s",
						got, response.CodeInvalidParam, rec.Body.String())
				}
			} else {
				t.Fatalf("status = 500 — the last-admin guard must not look like a service failure: %s",
					rec.Body.String())
			}

			// 而且它真的没被改掉。
			a := reg.accounts[adminUser].account
			if a.DisabledAt != nil || a.Role != registry.RoleAdmin || reg.accounts[adminUser].deletedAt != nil {
				t.Errorf("the last admin was modified anyway: %+v", a)
			}
		})
	}
}

// 引导账号登录必须留痕：它能登进来，意味着平台此刻一个启用中的管理员
// 都不剩（design doc 2026-08-14 §2，规范 §43）。
func TestBootstrapLoginIsAudited(t *testing.T) {
	reg := fixtureSource()
	// 装配阶段本身就是一次引导登录。
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

	if len(reg.bootstrapLogins) != 1 || reg.bootstrapLogins[0] != "demo" {
		t.Fatalf("bootstrap login audit rows = %v, want exactly one for demo", reg.bootstrapLogins)
	}

	// 库内账号登录不写这条：它不是从逃生口进来的。
	reg.withAccount(t, "alice", registry.RoleViewer, viewerPassword)
	rec := postJSON(t, h, "/api/v1/sessions", map[string]string{
		"username": "alice", "password": viewerPassword,
	})
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("database account sign-in code = %v, want 0: %s", got, rec.Body.String())
	}
	if len(reg.bootstrapLogins) != 1 {
		t.Errorf("a database account's sign-in wrote a BOOTSTRAP_LOGIN row: %v", reg.bootstrapLogins)
	}
}

// 当前会话端点要回角色：界面据此决定渲染不渲染管理员入口（design doc §6）。
// 那只是体验，服务端已经拒绝过一次（规范 §34）。
func TestCurrentSessionReturnsTheRole(t *testing.T) {
	reg := fixtureSource()
	h, sessions, bootstrap := newTestRouterWithRegistry(t, fixtureReader(), reg)

	admin := authedGet(t, h, bootstrap, "/api/v1/sessions/current")
	data, _ := bodyOf(t, admin)["data"].(map[string]any)
	if data["username"] != "demo" || data["role"] != string(registry.RoleAdmin) {
		t.Errorf("data = %v, want demo/ADMIN", bodyOf(t, admin)["data"])
	}

	viewer := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)
	got := authedGet(t, h, viewer, "/api/v1/sessions/current")
	data, _ = bodyOf(t, got)["data"].(map[string]any)
	if data["username"] != "readonly" || data["role"] != string(registry.RoleViewer) {
		t.Errorf("data = %v, want readonly/VIEWER", bodyOf(t, got)["data"])
	}
}

// 引导账号在账号表里没有对应的行，改自己的密码要说得清为什么不行，
// 而不是回一句"资源不存在"。
func TestBootstrapAccountCannotChangeItsOwnPassword(t *testing.T) {
	reg := fixtureSource()
	h, _, bootstrap := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, bootstrap, "/api/v1/me/password", map[string]string{
		"currentPassword": testPassword, "newPassword": newPassword,
	})
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Fatalf("code = %v, want %d: %s", got, response.CodeInvalidParam, rec.Body.String())
	}
	if msg, _ := bodyOf(t, rec)["msg"].(string); !strings.Contains(msg, "引导账号") {
		t.Errorf("msg = %q, want an explanation naming the bootstrap account", msg)
	}
}

// 账号表读不出来时，账号端点走真实的 500，且不把数据库错误文本回传
// （规范 §22）。
func TestAccountEndpointsDoNotLeakRegistryErrorText(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)
	// 建好会话之后再让账号表坏掉：坏在前面的话，连授权都过不去，这条
	// 断言就落在一个没被执行到的分支上。
	reg.failAccountsWith = errors.New("mysql: dial tcp 10.0.0.5:3306: connection refused")

	rec := callWith(t, h, http.MethodGet, "/api/v1/accounts", cookie)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{"mysql", "10.0.0.5", "3306"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the response leaked %q: %s", secret, rec.Body.String())
		}
	}
}
