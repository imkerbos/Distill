package httpapi_test

import (
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

// newFullRouter 装配一个带真实数据源的路由，并返回一个已登录的 Cookie。
func newFullRouter(t *testing.T) (http.Handler, *http.Cookie) {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	h := httpapi.NewRouter(httpapi.Deps{
		Sessions: auth.NewSessionStore(time.Hour, nil),
		Verifier: auth.NewVerifier([]config.User{{Username: "demo", PasswordHash: string(hash)}}),
		Logger:   logger,
		Reader:   store.NewFixtureReader(fixture.Load()),
	})

	login := postJSON(t, h, "/api/v1/sessions", map[string]string{
		"username": "demo", "password": testPassword,
	})
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login returned no cookie")
	}
	return h, cookies[0]
}

func authedGet(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestClustersEndpoint(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/clusters")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int                    `json:"code"`
		Data []store.ClusterSummary `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != 0 {
		t.Errorf("code = %d, want 0", env.Code)
	}
	if len(env.Data) != 2 {
		t.Errorf("got %d clusters, want 2", len(env.Data))
	}
}

func TestClustersRequiresAuth(t *testing.T) {
	h, _ := newFullRouter(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestTopologyEndpoint(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/topology")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int            `json:"code"`
		Data store.Topology `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Nodes) == 0 || len(env.Data.Edges) == 0 {
		t.Errorf("topology is empty: %d nodes, %d edges", len(env.Data.Nodes), len(env.Data.Edges))
	}
}

// 集群不存在是业务级失败：HTTP 200 + 20002，不计入服务错误率。
func TestTopologyUnknownClusterIsBusinessError(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/clusters/no-such/topology")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a missing resource is not a server fault", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}

func TestQualityEndpoint(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/quality")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int           `json:"code"`
		Data store.Quality `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.TotalFlows == 0 {
		t.Error("quality reports zero flows")
	}
	if len(env.Data.UnknownComposition) == 0 {
		t.Error("UnknownComposition is empty; the UI needs the breakdown, not just a rate")
	}
}
