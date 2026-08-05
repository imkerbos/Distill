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
func newTestRouter(t *testing.T, reader store.Reader) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	sessions := auth.NewSessionStore(time.Hour, nil)
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	// reader 允许为 nil（见上），因此默认窗口只在拿到 fixture 实现时取。
	// 走类型断言而非扩展 store.Reader：数据窗口是 fixture 特有的概念，
	// 真实存储没有"全部数据的时间范围"这回事。
	var window store.TimeWindow
	if fr, ok := reader.(*store.FixtureReader); ok {
		window = fr.DataWindow()
	}

	h := httpapi.NewRouter(httpapi.Deps{
		Sessions: sessions,
		Verifier: auth.NewVerifier([]config.User{{Username: "demo", PasswordHash: string(hash)}}),
		Logger:   logger,
		Reader:   reader,
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

// fixtureReader 是走真实合成数据的 Reader，供需要真实响应内容的测试使用。
func fixtureReader() store.Reader {
	return store.NewFixtureReader(fixture.Load())
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
