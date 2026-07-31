package httpapi_test

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
	return newRouterWithReader(t, store.NewFixtureReader(fixture.Load()))
}

// newRouterWithReader 装配一个使用给定 Reader 的路由，并返回一个已登录的 Cookie。
//
// 独立于 newFullRouter：错误路径测试需要一个所有查询都失败的 Reader，
// 与真实 fixture 数据无关，但登录、Cookie 装配这部分逻辑不该重复一份。
func newRouterWithReader(t *testing.T, reader store.Reader) (http.Handler, *http.Cookie) {
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
		Reader:   reader,
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

// brokenReader 让每个查询都失败，用来锁住内部错误的处理方式。
type brokenReader struct{}

func (brokenReader) Clusters(context.Context) ([]store.ClusterSummary, error) {
	return nil, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Topology(context.Context, string) (store.Topology, error) {
	return store.Topology{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Flows(context.Context, store.FlowFilter) ([]store.FlowRecord, error) {
	return nil, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Flow(context.Context, string) (store.Decision, bool, error) {
	return store.Decision{}, false, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Quality(context.Context, string) (store.Quality, error) {
	return store.Quality{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
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
	if bodyOf(t, rec)["data"] != nil {
		t.Error("data must be null on a business-level failure")
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

// 内部故障必须计入服务错误率，所以走真实的 500 —— 但错误细节只进日志。
// 把 "connection refused at 10.0.0.5:9050" 这类信息回给调用方，
// 等于顺着 API 把内部拓扑交出去。
func TestReaderFailureIsInternalErrorAndLeaksNothing(t *testing.T) {
	h, cookie := newRouterWithReader(t, brokenReader{})

	for _, path := range []string{
		"/api/v1/clusters",
		"/api/v1/clusters/prod-asia-1/topology",
		"/api/v1/clusters/prod-asia-1/quality",
	} {
		rec := authedGet(t, h, cookie, path)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s status = %d, want 500", path, rec.Code)
		}
		body := bodyOf(t, rec)
		if body["code"] != float64(50001) {
			t.Errorf("%s code = %v, want 50001", path, body["code"])
		}
		if body["data"] != nil {
			t.Errorf("%s data = %v, want null", path, body["data"])
		}
		for _, secret := range []string{"bigquery", "connection refused", "10.0.0.5", "9050"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("%s response leaked %q: %s", path, secret, rec.Body.String())
			}
		}
		for _, values := range rec.Header() {
			for _, v := range values {
				for _, secret := range []string{"bigquery", "connection refused", "10.0.0.5", "9050"} {
					if strings.Contains(v, secret) {
						t.Errorf("%s response header leaked %q", path, secret)
					}
				}
			}
		}
	}
}
