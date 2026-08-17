package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// brokenReader 让每个查询都失败，用来锁住内部错误的处理方式。
type brokenReader struct{}

func (brokenReader) DefaultWindow(context.Context, string) (store.TimeWindow, error) {
	return store.TimeWindow{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Topology(context.Context, string, store.TopologyLevel) (store.Topology, error) {
	return store.Topology{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Flows(context.Context, store.FlowFilter) (store.FlowPage, error) {
	return store.FlowPage{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Flow(context.Context, string) (store.Decision, bool, error) {
	return store.Decision{}, false, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Quality(context.Context, string) (store.Quality, error) {
	return store.Quality{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) Security(context.Context, string, store.TimeWindow) (store.SecurityReport, error) {
	return store.SecurityReport{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) PolicyPreview(context.Context, string, string, store.TimeWindow) (store.PolicyPreview, error) {
	return store.PolicyPreview{}, errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

func (brokenReader) EnsureRuleExists(
	context.Context, string, string, string, string, policygen.OverrideDecision, store.TimeWindow,
) error {
	return errors.New("bigquery: connection refused at 10.0.0.5:9050")
}

// panicReader 在查询时 panic，用来验证路由本身确实装了 Recoverer——
// 单独测中间件只能证明它管用，证明不了它被挂上去了。
//
// panic 挂在 Topology 而不是 Clusters：GET /api/v1/clusters 这个 task
// 起改成从 Registry 读，不再经过 Reader，Topology 仍然是纯 Reader 端点。
type panicReader struct{ brokenReader }

func (panicReader) Topology(context.Context, string, store.TopologyLevel) (store.Topology, error) {
	panic("reader exploded at 10.0.0.5:9050")
}

func TestRouterRecoversPanics(t *testing.T) {
	h, _, cookie := newTestRouter(t, panicReader{})
	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/topology")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(50001) {
		t.Errorf("code = %v, want 50001", body["code"])
	}
	for _, secret := range []string{"exploded", "10.0.0.5", "9050"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("panic response leaked %q: %s", secret, rec.Body.String())
		}
	}
}

// noCollectionReader 让每个查询都以「这个集群还没有可用的采集」结束。
//
// 包一层 fmt.Errorf 而不是直接返回哨兵：真实的 collectstore 也是这么返回的
// （它要在文案里点名集群与缺的是哪一步），一个只认裸哨兵的映射在真实调用
// 路径上会失效，而失效之后的症状恰好是它本来要消除的那个 500。
type noCollectionReader struct{ brokenReader }

func (noCollectionReader) DefaultWindow(context.Context, string) (store.TimeWindow, error) {
	return store.TimeWindow{}, noCollection()
}

func (noCollectionReader) Topology(
	context.Context, string, store.TopologyLevel,
) (store.Topology, error) {
	return store.Topology{}, noCollection()
}

func (noCollectionReader) Flows(context.Context, store.FlowFilter) (store.FlowPage, error) {
	return store.FlowPage{}, noCollection()
}

func (noCollectionReader) Flow(context.Context, string) (store.Decision, bool, error) {
	return store.Decision{}, false, noCollection()
}

func (noCollectionReader) Quality(context.Context, string) (store.Quality, error) {
	return store.Quality{}, noCollection()
}

func (noCollectionReader) Security(
	context.Context, string, store.TimeWindow,
) (store.SecurityReport, error) {
	return store.SecurityReport{}, noCollection()
}

func (noCollectionReader) PolicyPreview(
	context.Context, string, string, store.TimeWindow,
) (store.PolicyPreview, error) {
	return store.PolicyPreview{}, noCollection()
}

func noCollection() error {
	return fmt.Errorf("%w: cluster prod-asia-1 has no flow ingest", collectstore.ErrNoCollection)
}

// 「还没有可用的采集」不是服务故障，必须说得出原因。
//
// 落回 50001 时操作者读到的是「服务内部错误」，于是他去查一个完全健康的
// 服务；而事实是这个集群还没被采过，该做的是去跑采集器与摄入。两条处置
// 完全不同，而 500 那句话把人指向了错的那一条（design doc §6）。
//
// 六个读端点一条不落：writeReaderError 是共用的，但共用的不是路由 ——
// 只测其中一条，另外五条改走别的错误处理时全套仍然全绿。
func TestNoUsableCollectionSaysSoInsteadOf500(t *testing.T) {
	h, _, cookie := newTestRouter(t, noCollectionReader{})

	for _, path := range []string{
		"/api/v1/clusters/prod-asia-1/topology",
		"/api/v1/clusters/prod-asia-1/quality",
		"/api/v1/clusters/prod-asia-1/security",
		"/api/v1/clusters/prod-asia-1/policy-preview",
		"/api/v1/flows",
		"/api/v1/flows/flow-0001/decision",
	} {
		rec := authedGet(t, h, cookie, path)

		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 —— 一个还没采过的集群不是服务故障，"+
				"不该计进服务错误率", path, rec.Code)
		}
		body := bodyOf(t, rec)
		if body["code"] != float64(20005) {
			t.Errorf("%s code = %v, want 20005（该集群还没有可用的采集数据）；"+
				"50001 会把操作者支去查一个健康的服务", path, body["code"])
		}
		if msg, _ := body["msg"].(string); msg == response.CodeInternal.Message() {
			t.Errorf("%s msg = %q，与内部错误同一句话，说明这条路径塌回了 500 的文案", path, msg)
		}
		// 采集器没跑（20004）与摄入没跑（20005）是两件事，处置不同。
		if body["code"] == float64(response.CodeNoCollectionRun) {
			t.Errorf("%s 用了 20004：那一条说的是「从没采过资产」，"+
				"与「采过、但这段窗口没有可用数据」不是同一句话", path)
		}
		if body["data"] != nil {
			t.Errorf("%s data = %v, want null", path, body["data"])
		}
	}
}

func authedGet(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// GET /api/v1/clusters 读注册表而非 Reader（本 task 之前的行为），
// 所以要装配一个装了集群的 memRegistry，而不是只给 fixtureReader。
func TestClustersEndpoint(t *testing.T) {
	reg := newMemRegistry()
	for _, c := range fixtureClusters() {
		reg.clusters[c.ID] = c
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	rec := authedGet(t, h, cookie, "/api/v1/clusters")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int                `json:"code"`
		Data []registry.Cluster `json:"data"`
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
	h, _, _ := newTestRouter(t, fixtureReader())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestTopologyEndpoint(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
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
	h, _, cookie := newTestRouter(t, fixtureReader())
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
	h, _, cookie := newTestRouter(t, fixtureReader())
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
//
// GET /clusters 这个 task 起改成读 Registry 而非 Reader，所以这里同时
// 装一个会失败的 memRegistry，用同一段敏感文本覆盖两条读路径 ——
// writeReaderError 与 writeRegistryError 对内部故障的处理是同一个判据，
// 分开测只会让两边各自的覆盖都显得不完整。
func TestReaderFailureIsInternalErrorAndLeaksNothing(t *testing.T) {
	failingRegistry := newMemRegistry()
	failingRegistry.failWith = errors.New("bigquery: connection refused at 10.0.0.5:9050")
	h, _, cookie := newTestRouterWithRegistry(t, brokenReader{}, failingRegistry)

	// 流量两条也要覆盖：它们走同一个 writeReaderError，
	// 只测 fleet 三条等于默认另外两条不会退化。
	for _, path := range []string{
		"/api/v1/clusters",
		"/api/v1/clusters/prod-asia-1/topology",
		"/api/v1/clusters/prod-asia-1/quality",
		"/api/v1/flows",
		"/api/v1/flows/flow-0001/decision",
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
