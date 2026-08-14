package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/httpapi"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

const testPassword = "distill-demo"

// newTestRouter 装配一个使用给定 Reader 的路由，返回路由、会话存储，
// 以及一个已登录的 Cookie。
//
// 全包只此一个装配入口：认证测试要会话存储、数据测试要 Cookie、错误路径
// 测试要一个全失败的 Reader，但登录与依赖装配这段逻辑三处都一样，
// 分成几个签名不同的构造器只会让它们慢慢长歪。reader 可以为 nil，
// 用于不触达数据层的测试。
//
// Registry 固定传一个空的 memRegistry：绝大多数用例只关心 Fleet/Flow
// 数据层，不需要注册任何集群。需要注册数据的测试改用
// newTestRouterWithRegistry。
func newTestRouter(t *testing.T, reader store.Reader) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()
	return newTestRouterWithRegistry(t, reader, newMemRegistry())
}

// newTestRouterWithRegistry 是 newTestRouter 的变体，额外接受一个
// registry.Store 实现，供注册与导入相关的 handler 测试使用。
func newTestRouterWithRegistry(
	t *testing.T, reader store.Reader, reg registry.Store,
) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()

	return newTestRouterWithWindow(t, reader, reg, fixtureWindow(reader))
}

// fixtureWindow 取 fixture 数据的全量时间窗。
//
// reader 允许为 nil（见上），因此默认窗口只在拿到 fixture 实现时取。
// 走类型断言而非扩展 store.Reader：数据窗口是 fixture 特有的概念，
// 真实存储没有"全部数据的时间范围"这回事。
func fixtureWindow(reader store.Reader) store.TimeWindow {
	if fr, ok := reader.(*store.FixtureReader); ok {
		return fr.DataWindow()
	}
	return store.TimeWindow{}
}

// newTestRouterWithGitVerifier 是 newTestRouterWithRegistry 的变体，
// 额外注入一个 Git 绑定校验器。
//
// 单独开一个入口而不是给既有构造器加参数：绝大多数用例装配的是"未配置
// secrets"的部署形态（校验器为 nil），而那正是必须持续被覆盖的默认形态 ——
// 让它保持零参数，就不会有人为了改签名顺手给它塞一个非 nil 的替身。
func newTestRouterWithGitVerifier(
	t *testing.T, reader store.Reader, reg registry.Store, gv httpapi.GitVerifier,
) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()
	return buildTestRouter(t, reader, reg, fixtureWindow(reader), gv)
}

// newTestRouterWithWindow 是 newTestRouterWithRegistry 的底层版本，
// 额外接受一个显式的 DefaultWindow，而不是自动取 fixture 的全量窗口。
//
// 绝大多数用例要的正是"全量窗口兜底"，交给 newTestRouterWithRegistry
// 自动推导即可。只有验证"默认窗口比调用方显式传入的窗口更窄"这类场景
// （override_handler_test.go 里对 rule-overrides 请求体 from/to 的穿透
// 测试）才需要手动指定一个比全量更窄的默认窗口——fixture 数据集里，
// 任何自定义窗口的候选集在数学上都是全量窗口候选集的子集（BASELINE
// 规则与窗口无关，LEARNED 规则的存在性只会随窗口扩大而增多、不会减少），
// 所以默认窗口取全量时，永远构造不出"只在自定义窗口里存在、默认窗口里
// 没有"的指纹，那条测试就验证不了任何东西。
func newTestRouterWithWindow(
	t *testing.T, reader store.Reader, reg registry.Store, window store.TimeWindow,
) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()
	// 校验器为 nil：未配置 secrets 是绝大多数用例要的形态，也是部署上
	// 真实存在的一种 —— 结论一律 NOT_VERIFIED，而不是"校验都通过"。
	return buildTestRouter(t, reader, reg, window, nil)
}

// newTestRouterWithLog 是 newTestRouterWithRegistry 的变体，额外把请求日志
// 引到一个缓冲区。
//
// 只有验证「日志里有没有账号名」的用例需要它（design doc 2026-08-14 §7）：
// 那条断言必须读**真实的日志输出**，从 handler 或会话存储反推等于让测试
// 自己回答自己的问题。其余用例仍把日志丢进 io.Discard。
func newTestRouterWithLog(
	t *testing.T, reg registry.Store,
) (http.Handler, *auth.SessionStore, *http.Cookie, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h, sessions, cookie := buildTestRouterWithLog(t, nil, reg, store.TimeWindow{}, nil, "INFO", &buf)
	return h, sessions, cookie, &buf
}

// buildTestRouter 是全部装配入口的底层实现。
func buildTestRouter(
	t *testing.T, reader store.Reader, reg registry.Store,
	window store.TimeWindow, gv httpapi.GitVerifier,
) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()
	return buildTestRouterWithLog(t, reader, reg, window, gv, "ERROR", io.Discard)
}

// buildTestRouterWithLog 是 buildTestRouter 多带一个日志去处的版本。
func buildTestRouterWithLog(
	t *testing.T, reader store.Reader, reg registry.Store,
	window store.TimeWindow, gv httpapi.GitVerifier, level string, logOut io.Writer,
) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	sessions := auth.NewSessionStore(time.Hour, nil)
	logger, err := applog.New(level, logOut)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	h := httpapi.NewRouter(httpapi.Deps{
		Sessions: sessions,
		// 账号表跟着传进去：角色在每次请求上从账号记录现读，而引导账号
		// 只在库里没有启用中的管理员时可用（design doc 2026-08-14 §2、§4）。
		// 绝大多数用例的账号表是空的，于是 "demo" 仍然是那个能登进来的
		// 管理员 —— 与这些用例此前的形态一致。
		Verifier:    auth.NewVerifier(config.User{Username: "demo", PasswordHash: string(hash)}, reg),
		Logger:      logger,
		Reader:      reader,
		Registry:    reg,
		GitVerifier: gv,
		// 流量查询的时间窗是必填的；测试装配方与 cmd 一样注入覆盖
		// 全量数据的窗口，使不关心时间维的用例无须逐个传 from/to。
		DefaultWindow: window,
	})

	login := postJSON(t, h, "/api/v1/sessions", map[string]string{
		"username": "demo", "password": testPassword,
	})
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login returned no cookie")
	}
	return h, sessions, cookies[0]
}

// fixtureClusters 镜像 internal/fixture/asset.go 里两个集群的注册信息。
// 值必须与那里的 Registry/APIServers 逐字一致，否则测试读到的网段会
// 与 fixture 兜底值不一致。
func fixtureClusters() []registry.Cluster {
	return []registry.Cluster{
		{
			ID: "prod-asia-1", DisplayName: "Asia Prod",
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
			State:              registry.StateReady,
			APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
		{
			ID: "prod-eu-1", DisplayName: "EU Prod",
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.132.0.0/20",
			State:              registry.StateReady,
			APIServers:         []registry.APIServer{{Host: "10.13.0.2", CIDR: "10.13.0.0/28", Port: 443}},
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
	}
}

// fixtureSource 是装了两个 fixture 集群的内存注册表。
//
// memRegistry 同时满足 registry.Store 与 store.ClusterSource：注册状态
// 现在每个请求现查（spec §4.5），reader 与 handler 要用同一份注册信息，
// 否则一个空注册表会让所有按集群解析的端点都报「未找到」。
func fixtureSource() *memRegistry {
	reg := newMemRegistry()
	for _, c := range fixtureClusters() {
		reg.clusters[c.ID] = c
	}
	return reg
}

// fixtureReader 是走真实合成数据的 Reader，供需要真实响应内容的测试使用。
func fixtureReader() store.Reader {
	return store.NewFixtureReader(fixture.Load(), fixtureSource())
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bodyOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	return got
}

func TestLoginSuccessSetsCookie(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	rec := postJSON(t, h, "/api/v1/sessions", map[string]string{
		"username": "demo", "password": testPassword,
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Errorf("code = %v, want 0", got)
	}

	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == httpapi.SessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !sessionCookie.HttpOnly {
		t.Error("session cookie must be HttpOnly so scripts cannot read it")
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", sessionCookie.SameSite)
	}
	if sessionCookie.Path != "/" {
		t.Errorf("Path = %q, want /", sessionCookie.Path)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	rec := postJSON(t, h, "/api/v1/sessions", map[string]string{
		"username": "demo", "password": "wrong",
	})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — bad credentials are a business failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(10001) {
		t.Errorf("code = %v, want 10001", got)
	}
}

// 不存在的用户与错误密码必须返回完全相同的响应，否则等于提供账号枚举接口。
func TestLoginUnknownUserIsIndistinguishable(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	wrongPass := postJSON(t, h, "/api/v1/sessions", map[string]string{"username": "demo", "password": "wrong"})
	unknown := postJSON(t, h, "/api/v1/sessions", map[string]string{"username": "ghost", "password": "wrong"})

	if wrongPass.Body.String() != unknown.Body.String() {
		t.Errorf("responses differ:\n wrong password: %s\n unknown user:   %s",
			wrongPass.Body.String(), unknown.Body.String())
	}
}

func TestLoginRejectsMalformedJSON(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

func TestCurrentSessionReturnsUsername(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	login := postJSON(t, h, "/api/v1/sessions", map[string]string{"username": "demo", "password": testPassword})
	cookie := login.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/current", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	data, ok := bodyOf(t, rec)["data"].(map[string]any)
	if !ok || data["username"] != "demo" {
		t.Errorf("data = %v, want username demo", bodyOf(t, rec)["data"])
	}
}

// 登出必须销毁服务端会话，而不是仅清 Cookie：
// 只清 Cookie 的话，已泄露的会话 ID 仍然有效直到过期。
func TestLogoutInvalidatesSessionServerSide(t *testing.T) {
	h, sessions, _ := newTestRouter(t, nil)

	login := postJSON(t, h, "/api/v1/sessions", map[string]string{"username": "demo", "password": testPassword})
	cookie := login.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/current", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", rec.Code)
	}
	if _, ok := sessions.Get(cookie.Value); ok {
		t.Error("session still valid server-side after logout")
	}

	// 用同一个 Cookie 再访问受保护端点，必须 401。
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/current", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("status after logout = %d, want 401", rec2.Code)
	}
}

func TestProtectedEndpointRequiresSession(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/current", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}
