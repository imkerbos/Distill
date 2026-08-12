package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// memRegistry 是内存版 registry.Store，用于 handler 测试。
//
// 不连数据库：handler 要验证的是参数解析、错误码映射与响应形状，
// 而事务与外键行为在 internal/mysqlregistry 的集成测试里验证。
type memRegistry struct {
	clusters  map[string]registry.Cluster
	imports   map[string][]registry.PolicyImport
	overrides map[string][]registry.RuleOverride
	failWith  error
}

func newMemRegistry() *memRegistry {
	return &memRegistry{
		clusters:  map[string]registry.Cluster{},
		imports:   map[string][]registry.PolicyImport{},
		overrides: map[string][]registry.RuleOverride{},
	}
}

func (m *memRegistry) Clusters(context.Context) ([]registry.Cluster, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	out := make([]registry.Cluster, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, c)
	}
	return out, nil
}

func (m *memRegistry) Cluster(_ context.Context, id string) (registry.Cluster, bool, error) {
	if m.failWith != nil {
		return registry.Cluster{}, false, m.failWith
	}
	c, ok := m.clusters[id]
	return c, ok, nil
}

func (m *memRegistry) CreateCluster(_ context.Context, _ registry.Actor, c registry.Cluster) error {
	if m.failWith != nil {
		return m.failWith
	}
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	m.clusters[c.ID] = c
	return nil
}

func (m *memRegistry) UpdateCluster(_ context.Context, _ registry.Actor, c registry.Cluster) error {
	if m.failWith != nil {
		return m.failWith
	}
	if _, ok := m.clusters[c.ID]; !ok {
		return registry.ErrNotFound
	}
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	m.clusters[c.ID] = c
	return nil
}

func (m *memRegistry) SoftDeleteCluster(_ context.Context, _ registry.Actor, id string) error {
	if m.failWith != nil {
		return m.failWith
	}
	if _, ok := m.clusters[id]; !ok {
		return registry.ErrNotFound
	}
	delete(m.clusters, id)
	return nil
}

func (m *memRegistry) PolicyImports(_ context.Context, clusterID string) ([]registry.PolicyImport, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	return m.imports[clusterID], nil
}

func (m *memRegistry) CreatePolicyImport(_ context.Context, _ registry.Actor, p registry.PolicyImport) error {
	if m.failWith != nil {
		return m.failWith
	}
	// 与 mysqlregistry.CreatePolicyImport 一样先过校验：这个替身若比真实
	// 实现宽松，handler 测试就会在一条真实实现会拒绝的输入上通过。
	if err := registry.ValidatePolicyImport(p); err != nil {
		return err
	}
	m.imports[p.ClusterID] = append(m.imports[p.ClusterID], p)
	return nil
}

func (m *memRegistry) SoftDeletePolicyImport(_ context.Context, _ registry.Actor, clusterID, importID string) error {
	if m.failWith != nil {
		return m.failWith
	}
	kept := m.imports[clusterID][:0]
	for _, p := range m.imports[clusterID] {
		if p.ImportID != importID {
			kept = append(kept, p)
		}
	}
	m.imports[clusterID] = kept
	return nil
}

func (m *memRegistry) RuleOverrides(_ context.Context, clusterID string) ([]registry.RuleOverride, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	return m.overrides[clusterID], nil
}

// CreateRuleOverride 与 mysqlregistry 一样覆盖旧值而非报冲突：这个替身若
// 让重复决定并存，handler 测试就验证不出「重复决定该覆盖」这条约束。
func (m *memRegistry) CreateRuleOverride(_ context.Context, _ registry.Actor, o registry.RuleOverride) error {
	if m.failWith != nil {
		return m.failWith
	}
	if err := registry.ValidateOverride(o); err != nil {
		return err
	}
	list := m.overrides[o.ClusterID]
	for i, existing := range list {
		if existing.Namespace == o.Namespace && existing.Workload == o.Workload &&
			existing.Fingerprint == o.Fingerprint {
			list[i] = o
			return nil
		}
	}
	m.overrides[o.ClusterID] = append(list, o)
	return nil
}

func (m *memRegistry) SoftDeleteRuleOverride(
	_ context.Context, _ registry.Actor, clusterID, namespace, workload, fingerprint string,
) error {
	if m.failWith != nil {
		return m.failWith
	}
	list := m.overrides[clusterID]
	for i, existing := range list {
		if existing.Namespace == namespace && existing.Workload == workload &&
			existing.Fingerprint == fingerprint {
			m.overrides[clusterID] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return registry.ErrNotFound
}

// authedPostJSON 与 session_handler_test.go 的 postJSON 同形，多带一个
// 会话 Cookie —— 同名会与那个无 Cookie 的版本签名冲突，所以另起一个
// 名字，呼应已有的 authedGet。
func authedPostJSON(t *testing.T, h http.Handler, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postJSONNoAuth 就是 session_handler_test.go 里那个不带 Cookie 的
// postJSON —— 起别名是为了让调用点读起来能看出"故意不登录"这层意图。
func postJSONNoAuth(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, h, path, body)
}

func authedPatchJSON(t *testing.T, h http.Handler, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateClusterRequiresSession(t *testing.T) {
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// 请求体不是合法 JSON 是协议层问题，不是业务失败 —— 与 handleCreateSession
// 对畸形登录请求的处理保持一致（见 session_handler.go），必须是真实的 400，
// 不是 200 + code。这条路径此前没有测试锁住，是审阅时点出的相邻缺口。
func TestCreateClusterRejectsMalformedJSON(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

func TestUpdateClusterRejectsMalformedJSON(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/clusters/c1", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

func TestCreateClusterRoundTrips(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "new-1", "displayName": "New", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "state": "REGISTERED",
		"apiServers":         []map[string]any{{"host": "10.9.0.2", "cidr": "10.9.0.0/28", "port": 443}},
		"healthCheckSources": []string{"35.191.0.0/16"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0", got)
	}
	if _, ok := reg.clusters["new-1"]; !ok {
		t.Error("cluster was not stored")
	}
}

// 网段写错是业务失败，不该计入服务错误率，也不该只回一句「参数不合法」——
// 一个集群有四类网段，不说是哪一类会让操作者逐个试。
func TestCreateClusterRejectsMalformedCIDR(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "bad-1", "displayName": "Bad", "podCidr": "10.20.0/14",
		"nodeCidr": "10.140.0.0/20", "state": "REGISTERED",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad field value is a business failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
	// msg 必须点名是哪一类网段错了：一个集群有四类网段，只回一句
	// 「参数不合法」会让操作者逐个试。这段文案是 registry.ValidateCluster
	// 自己写的，不是驱动或第三方库的错误文本，回传它不泄露任何内部拓扑。
	if msg, _ := bodyOf(t, rec)["msg"].(string); !strings.Contains(msg, "podCIDR") {
		t.Errorf("msg = %q, want it to name podCIDR", msg)
	}
}

// 接入状态由服务端决定：spec 要求创建一律从 REGISTERED 起步，只在字段
// 为空时兜底不足以兑现这句话 —— 一个显式的 {"state":"READY"} 必须
// 同样被忽略，否则调用方可以把「还没有数据」标成「可以出推荐了」。
func TestCreateClusterIgnoresCallerSuppliedState(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "sneaky-1", "displayName": "Sneaky", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "state": "READY",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	stored, ok := reg.clusters["sneaky-1"]
	if !ok {
		t.Fatal("cluster was not stored")
	}
	if stored.State != registry.StateRegistered {
		t.Errorf("state = %q, want REGISTERED — an explicit READY in the request must be ignored", stored.State)
	}
}

// PATCH 必须保留库里已有的接入状态：既不能被请求体里任意的 state 值
// 改写，也不该在修改网段这类操作时被悄悄打回 REGISTERED。
func TestUpdateClusterPreservesExistingState(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPatchJSON(t, h, cookie, "/api/v1/clusters/c1", map[string]any{
		"displayName": "C1 renamed", "podCidr": "10.4.0.0/14",
		"nodeCidr": "10.128.0.0/20", "state": "REGISTERED",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if reg.clusters["c1"].State != registry.StateReady {
		t.Errorf("state = %q, want READY to survive the update", reg.clusters["c1"].State)
	}
	if reg.clusters["c1"].DisplayName != "C1 renamed" {
		t.Errorf("displayName = %q, want the update applied", reg.clusters["c1"].DisplayName)
	}
}

func TestUpdateClusterUnknownIsBusinessNotFound(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	rec := authedPatchJSON(t, h, cookie, "/api/v1/clusters/no-such", map[string]any{
		"displayName": "X", "podCidr": "10.4.0.0/14", "nodeCidr": "10.128.0.0/20",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a missing resource is not a server fault", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}

func TestClustersEndpointReadsRegistry(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateRegistered,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedGet(t, h, cookie, "/api/v1/clusters")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	data, ok := bodyOf(t, rec)["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want one cluster from the registry", bodyOf(t, rec)["data"])
	}
}

// registry 内部故障（比如数据库连不上）必须计入服务错误率，走真实的
// 500 —— 但错误细节只进日志，不能顺着响应把内部拓扑（主机、端口）
// 交给调用方。这一条与 writeReaderError 对 Reader 故障的处理对称，
// 因为两者共享同一个"该不该计入服务错误率"的判据。
func TestCreateClusterFailurePropagatesRegistryInternalError(t *testing.T) {
	reg := newMemRegistry()
	reg.failWith = errors.New("mysql: dial tcp 10.0.0.5:3306: connection refused")
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "x", "displayName": "X", "podCidr": "10.4.0.0/14", "nodeCidr": "10.128.0.0/20",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(50001) {
		t.Errorf("code = %v, want 50001", got)
	}
	// 与 ErrInvalid 分支的对照组：这条错误不是 registry 自己写的校验文案，
	// 是驱动的原始报错，所以 msg 必须是固定文案，一个字都不能带出去 ——
	// WriteInvalid 只喂给「我们自己写的」错误，这里走的是 default 分支，
	// 还是 response.WriteSystem，行为不该受这次改动影响。
	if got := bodyOf(t, rec)["msg"]; got != response.CodeInternal.Message() {
		t.Errorf("msg = %q, want the fixed internal-error message %q", got, response.CodeInternal.Message())
	}
	for _, secret := range []string{"mysql", "10.0.0.5", "3306"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("response leaked %q: %s", secret, rec.Body.String())
		}
	}
}

func TestDeleteClusterRoundTrips(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateRegistered,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/clusters/c1", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if _, ok := reg.clusters["c1"]; ok {
		t.Error("cluster still present after delete")
	}
}

// gitBindingRef 是 Secret Manager 中那条引用的取值，凭据本身永不入库。
//
// 提成常量而不是写在两处字面量里：除了让期望值与请求体共用同一个值，
// 也避免 gosec G101 把「字段名带 credential」的字面量当成硬编码凭据 ——
// 这里存的是引用，不是凭据，而消掉误报比挂一条 //nolint 更诚实。
const gitBindingRef = "sm://distill/git"

// 请求体 → 领域对象的整体比对：逐字段挑着断言挡不住漏映射。
//
// 审阅时的实证是 —— 把 toCluster 里 APIServers / HealthCheckSources / Git
// 三行赋值删掉，./internal/httpapi 全绿。而这三项正是 control-plane 与
// 健康检查两类 Baseline 的推导依据，漏掉的后果是少一条放行规则，
// 表现为生产阻断而不是注册时的报错。
//
// 比对整个结构体而不是选几个字段：新增一个字段却忘记映射时，
// 表现必须是这个测试失败，而不是没有人注意到。
func TestCreateClusterCarriesEveryFieldIntoTheDomainObject(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "rt-1", "displayName": "Round Trip", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "ccnpPresent": true,
		// state 是服务端决定的，请求体里给一个相反的值，
		// 期望值里写 REGISTERED —— 这条断言顺带锁住「忽略调用方的 state」。
		"state": "READY",
		"apiServers": []map[string]any{
			{"host": "10.9.0.2", "cidr": "10.9.0.0/28", "port": 443},
			{"host": "10.9.0.3", "cidr": "10.9.0.0/28", "port": 443},
		},
		"healthCheckSources": []string{"35.191.0.0/16", "130.211.0.0/22"},
		"git": map[string]any{
			"repoUrl": "https://gitlab.example.com/net/policies.git", "branch": "main",
			"policyPath": "clusters/rt-1", "credentialRef": gitBindingRef,
			"lastWrittenCommit": "0123456789abcdef0123456789abcdef01234567",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	want := registry.Cluster{
		ID: "rt-1", DisplayName: "Round Trip",
		PodCIDR: "10.20.0.0/14", NodeCIDR: "10.140.0.0/20",
		CCNPPresent: true, State: registry.StateRegistered,
		APIServers: []registry.APIServer{
			{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
			{Host: "10.9.0.3", CIDR: "10.9.0.0/28", Port: 443},
		},
		HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		Git: &registry.GitBinding{
			RepoURL: "https://gitlab.example.com/net/policies.git", Branch: "main",
			PolicyPath: "clusters/rt-1", CredentialRef: gitBindingRef,
			LastWrittenCommit: "0123456789abcdef0123456789abcdef01234567",
		},
	}
	got, ok := reg.clusters["rt-1"]
	if !ok {
		t.Fatal("cluster was not stored")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored cluster =\n%+v\nwant\n%+v", got, want)
	}
	if got.Git == nil || *got.Git != *want.Git {
		t.Errorf("git binding = %+v, want %+v", got.Git, want.Git)
	}
}
