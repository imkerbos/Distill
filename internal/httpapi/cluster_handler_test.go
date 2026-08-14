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
	"time"

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
	// failWritesWith 只让写方法失败，读仍然正常。
	//
	// 与 failWith 分开是必要的，不是方便：先读后写的 handler（重校验就是
	// 一个）在 failWith 下会停在读那一步，写路径上的错误处理根本走不到，
	// 而一条"没有泄漏"的断言在一个没被执行到的分支上永远成立。
	failWritesWith error
	// trace 记录写方法的调用序列，与 stubGitVerifier 共用一份切片。
	// 只有验证「校验发生在落库之前」的用例会用到它，其余用例留 nil。
	trace *[]string
	// lastCluster 是最近一次集群写入**拿到的原始参数**。
	//
	// 与 clusters 里存下的那份分开是必要的，不是方便：这个替身跟真实实现
	// 一样会丢弃 c.Git（绑定不走集群写路径），于是「handler 有没有把一个
	// 绑定交出来」在落库结果上根本看不见 —— 一条只看 clusters 的断言，
	// 无论 clusterPayload 还带不带 git 都会通过。
	lastCluster registry.Cluster
	// setting 是这个替身持有的平台设置，见 Setting/UpdateSetting。
	setting registry.PlatformSetting
}

// writeErr 返回本次写调用该失败的错误，两个字段都没设时为 nil。
func (m *memRegistry) writeErr() error {
	if m.failWith != nil {
		return m.failWith
	}
	return m.failWritesWith
}

// record 记一次写调用。
func (m *memRegistry) record(op string) {
	if m.trace != nil {
		*m.trace = append(*m.trace, op)
	}
}

func newMemRegistry() *memRegistry {
	return &memRegistry{
		clusters:  map[string]registry.Cluster{},
		imports:   map[string][]registry.PolicyImport{},
		overrides: map[string][]registry.RuleOverride{},
		// 一份能过 ValidatePlatformSetting 的设置：零值那份读出来是
		// 「会话立即过期、超时保护关掉」，不是一个可用的初始状态。
		setting: registry.PlatformSetting{
			SessionTTL:          8 * time.Hour,
			HTTPReadTimeout:     10 * time.Second,
			HTTPWriteTimeout:    20 * time.Second,
			HTTPShutdownTimeout: 15 * time.Second,
			SecretsBackend:      registry.SecretsBackendNone,
			GitVerifyTimeout:    10 * time.Second,
		},
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

// CreateCluster 与 mysqlregistry 一样**忽略 c.Git**：绑定有自己的写入路径
// （BindGitRepo）与自己的审计行。一个会顺手把集群对象上的绑定落下来的替身，
// 会让「集群写路径悄悄改了绑定」这类 bug 在 handler 测试里全绿通过。
func (m *memRegistry) CreateCluster(_ context.Context, _ registry.Actor, c registry.Cluster) error {
	m.record("create")
	m.lastCluster = c
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	c.Git = nil
	m.clusters[c.ID] = c
	return nil
}

// UpdateCluster 同样不碰绑定：真实实现的 UPDATE 语句里根本没有
// cluster_git_binding 这张表，改一次网段不会顺手解绑。
func (m *memRegistry) UpdateCluster(_ context.Context, _ registry.Actor, c registry.Cluster) error {
	m.record("update")
	m.lastCluster = c
	if err := m.writeErr(); err != nil {
		return err
	}
	existing, ok := m.clusters[c.ID]
	if !ok {
		return registry.ErrNotFound
	}
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	c.Git = existing.Git
	m.clusters[c.ID] = c
	return nil
}

// BindGitRepo 写入或替换绑定。
//
// 与 mysqlregistry.BindGitRepo 一样先过 ValidateGitBinding、且**不**校验
// 集群其余字段：这次拆分买到的正是这一条，替身若在这里顺手跑一遍
// ValidateCluster，「绑定的合法性与集群其余字段无关」就没有东西守得住了。
func (m *memRegistry) BindGitRepo(
	_ context.Context, _ registry.Actor, clusterID string, b registry.GitBinding,
) error {
	m.record("bind")
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidateGitBinding(b); err != nil {
		return err
	}
	c, ok := m.clusters[clusterID]
	if !ok {
		return registry.ErrNotFound
	}
	// 空值按 NOT_VERIFIED 落库，与真实实现一致：空串不是登记过的枚举值。
	if b.VerifyResult == "" {
		b.VerifyResult = registry.VerifyNotVerified
	}
	c.Git = &b
	m.clusters[clusterID] = c
	return nil
}

// UnbindGitRepo 解除绑定。集群不存在与未绑定同样返回 ErrNotFound ——
// 两者从调用方视角都是「要解绑的那个东西不在」。
func (m *memRegistry) UnbindGitRepo(_ context.Context, _ registry.Actor, clusterID string) error {
	m.record("unbind")
	if err := m.writeErr(); err != nil {
		return err
	}
	c, ok := m.clusters[clusterID]
	if !ok || c.Git == nil {
		return registry.ErrNotFound
	}
	c.Git = nil
	m.clusters[clusterID] = c
	return nil
}

// SetGitVerifyResult 只写结论与时间。
//
// 只动这两个字段而不是整体替换绑定：真实实现的 UPDATE 只有 verify_result
// 与 verified_at 两列，一个顺手重写整行的替身会让「跑一次校验改写了仓库
// 地址」这种事在测试里看不出来。
func (m *memRegistry) SetGitVerifyResult(
	_ context.Context, _ registry.Actor, clusterID string,
	result registry.VerifyResult, at time.Time,
) error {
	m.record("set-verdict")
	if err := m.writeErr(); err != nil {
		return err
	}
	if !result.Valid() {
		return registry.NewInvalidError("verifyResult 不在已登记的取值范围内")
	}
	c, ok := m.clusters[clusterID]
	if !ok || c.Git == nil {
		return registry.ErrNotFound
	}
	g := *c.Git
	g.VerifyResult = result
	g.VerifiedAt = &at
	c.Git = &g
	m.clusters[clusterID] = c
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

// setting 是这个替身持有的平台设置。
//
// handler 层目前不读设置：校验器由装配方（cmd/distill-api）按当前设置现装，
// httpapi 只拿到一个 GitVerifier 接口。这两个方法因此只为满足
// registry.Store 而存在，行为保持最朴素的一份可用设置 —— 一旦有 handler
// 真的读它，那个 handler 自己的测试会给这里提出具体要求。
func (m *memRegistry) Setting(context.Context) (registry.PlatformSetting, error) {
	if m.failWith != nil {
		return registry.PlatformSetting{}, m.failWith
	}
	return m.setting, nil
}

func (m *memRegistry) UpdateSetting(_ context.Context, _ registry.Actor, s registry.PlatformSetting) error {
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidatePlatformSetting(s); err != nil {
		return err
	}
	m.record("UpdateSetting")
	m.setting = s
	return nil
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

func authedPutJSON(t *testing.T, h http.Handler, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
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
	req := httptest.NewRequest(http.MethodPut, "/api/v1/clusters/c1", bytes.NewReader([]byte("{not json")))
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

// 更新必须保留库里已有的接入状态：既不能被请求体里任意的 state 值
// 改写，也不该在修改网段这类操作时被悄悄打回 REGISTERED。
func TestUpdateClusterPreservesExistingState(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", map[string]any{
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

// 端点是整体替换，动词必须说的是同一件事。PATCH 在 HTTP 里承诺的是
// 「只改我给的字段」，而这个 handler 写整行 —— 留着这条路由，第一个
// 按 PATCH 语义只发 {"git":{...}} 的调用方就会把 podCIDR 清成空串。
// 它不该被友好地接受，也不该被静默改写，而该在路由层就不存在。
func TestUpdateClusterRejectsPatchVerb(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/clusters/c1",
		bytes.NewReader([]byte(`{"displayName":"C1"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 — PATCH must not name a full-replacement endpoint", rec.Code)
	}
}

// 整体替换必须表现得像整体替换：只带 displayName 的请求体不会被合并进
// 现有行，它是一个缺了 podCIDR/nodeCIDR 的完整集群，因此被校验拒绝。
//
// 这条测试守的是「不要好心补全」：一旦有人在 handler 里用库里的值填上
// 请求体没给的字段，这个请求就会成功 —— 而成功的代价是调用方从此无法
// 表达「把这一项清空」，且 PUT 的语义与实现再次分家。
func TestUpdateClusterIsReplacementNotMerge(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", map[string]any{
		"displayName": "C1 renamed",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a bad body is a business failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 — a name-only body is not a complete cluster", got)
	}
	// 被拒绝的请求不得留下任何痕迹：一次半成功的替换比一次失败更难排查。
	if got := reg.clusters["c1"].PodCIDR; got != "10.4.0.0/14" {
		t.Errorf("podCIDR = %q, want it untouched by a rejected update", got)
	}
}

// fullClusterBody 是一个完整的集群 PUT 请求体。
//
// extra 里的键会并进顶层对象，用于往请求体里塞那些**不该被采纳**的字段 ——
// 请求体必须能表达它们，测试才证明得了它们被忽略。
func fullClusterBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"displayName": "C1", "podCidr": "10.4.0.0/14", "nodeCidr": "10.128.0.0/20",
		"apiServers":         []map[string]any{{"host": "10.9.0.2", "cidr": "10.9.0.0/28", "port": 443}},
		"healthCheckSources": []string{"35.191.0.0/16"},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// 集群写入永不发出站：绑定已经不在这条路径上了，一次 SSH 握手在这里
// 既没有请求体上的目标，也会在远端日志里留下一条没人能解释的连接。
//
// 两个动词都打：create 与 update 是两处独立的调用点，只测一个的话，
// 另一处留着 verifyOnSave 也不会有测试变红。
func TestClusterWritesNeverReachOut(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			reg := newMemRegistry()
			reg.clusters["c1"] = boundCluster()
			stub := &stubGitVerifier{result: registry.VerifyOK}
			h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

			body := fullClusterBody(nil)
			if method == http.MethodPost {
				body["id"] = "new-4"
				if got := bodyOf(t, authedPostJSON(t, h, cookie, "/api/v1/clusters", body))["code"]; got != float64(0) {
					t.Fatalf("code = %v, want 0", got)
				}
			} else if got := bodyOf(t, authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", body))["code"]; got != float64(0) {
				t.Fatalf("code = %v, want 0", got)
			}
			if stub.calls != 0 {
				t.Errorf("verifier calls = %d, want 0 — a cluster write has no binding to verify", stub.calls)
			}
		})
	}
}

// 改集群不得动绑定。
//
// 绑定有自己的生命周期与自己的审计行；一次改网段顺手把绑定清掉，是
// 绑定还嵌在集群写模型里时才会发生的事，而它不会报错 —— 表现只是
// 某个集群某天起不再下发策略。
func TestUpdateClusterLeavesTheBindingAlone(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", fullClusterBody(nil))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	stored := reg.clusters["c1"].Git
	if stored == nil || *stored != *boundCluster().Git {
		t.Errorf("git = %+v, want the binding untouched by a cluster update", stored)
	}
}

func TestUpdateClusterUnknownIsBusinessNotFound(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/no-such", map[string]any{
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
const gitBindingRef = "distill-git"

// 请求体 → 领域对象的整体比对：逐字段挑着断言挡不住漏映射。
//
// 审阅时的实证是 —— 把 toCluster 里 APIServers / HealthCheckSources
// 两行赋值删掉，./internal/httpapi 全绿。而这两项正是 control-plane 与
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
	}
	got, ok := reg.clusters["rt-1"]
	if !ok {
		t.Fatal("cluster was not stored")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored cluster =\n%+v\nwant\n%+v", got, want)
	}
}
