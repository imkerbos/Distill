package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

const collectionPath = "/api/v1/clusters/prod-asia-1/collection"

// apiserverDetail 是一段像真的 apiserver 报错：里面有内部主机名与内部
// 端口。断言"它没有出现在响应的任何位置"时用它。
//
// 取自 collect 在 FORBIDDEN 路径上会拿到的那种文本形状。它之所以必须被
// 挡住，不是因为这一句本身多敏感，而是因为这一列的内容完全由目标集群的
// apiserver 决定，平台无从预知它会说出什么（规范 §19、§22）。
const apiserverDetail = `networkpolicies.networking.k8s.io is forbidden: ` +
	`User "system:serviceaccount:distill:collector" cannot list resource ` +
	`"networkpolicies" at https://apiserver.internal.prod-asia-1.corp:6443`

// fakeCollection 是一个不碰数据库的采集摘要读取端。
type fakeCollection struct {
	summary snapshotstore.CollectionSummary
	err     error
	// calls 记下被问过几次，用于确认拒绝发生在 handler 之前。
	calls int
}

func (f *fakeCollection) Latest(_ context.Context, _ string) (snapshotstore.CollectionSummary, error) {
	f.calls++
	if f.err != nil {
		return snapshotstore.CollectionSummary{}, f.err
	}
	return f.summary, nil
}

// partialSummary 是一次部分成功的采集：Pod 采到了 42 个、NetworkPolicy
// 一条也没采到（权限不足）、Namespace 确实是 0 个。
//
// 三种情形同时在场是必需的：只有一个真实的 0 与一个失败并排出现时，
// "把失败渲染成 0" 这个缺陷才有可能被断言区分出来。
func partialSummary() snapshotstore.CollectionSummary {
	at := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	return snapshotstore.CollectionSummary{
		ClusterID:  "prod-asia-1",
		RunID:      "run-1",
		ObservedAt: at,
		StartedAt:  at,
		FinishedAt: at.Add(9 * time.Second),
		Status:     string(snapshot.RunPartial),
		Resources: []snapshotstore.ResourceOutcome{
			snapshotstore.NewObservedResource(string(snapshot.ResourceNamespace), 0),
			snapshotstore.NewFailedResource(string(snapshot.ResourceNetworkPolicy),
				snapshotstore.FailureRecord{
					Reason: string(snapshot.FailureForbidden),
					Detail: apiserverDetail,
				}),
			snapshotstore.NewObservedResource(string(snapshot.ResourcePod), 42),
		},
		Warnings: []snapshotstore.WarningCount{
			{Kind: string(snapshot.WarningPodIPOutsideCluster), Count: 3},
		},
		WarningTotal: 3,
	}
}

// collectionResources 取出响应里的资源数组，逐条保持原始 JSON。
//
// 不解析成 struct：这些断言要问的正是"某个键在不在报文里"，而任何
// 目标结构体都会给缺席的键补一个零值，把要测的那件事抹平。
func collectionResources(t *testing.T, body map[string]any) map[string]map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(body["data"])
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var payload struct {
		Resources []map[string]json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("data is not a collection summary: %v", err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, item := range payload.Resources {
		var name string
		if err := json.Unmarshal(item["resource"], &name); err != nil {
			t.Fatalf("resource is not a string: %v", err)
		}
		out[name] = item
	}
	return out
}

/* --------------------------------------------------------------------- */
/* 1. 失败与 0 必须分得开（spec §4.2）                                      */
/* --------------------------------------------------------------------- */

// 一次 FORBIDDEN 的资源在报文里**没有 count 这个键**。
//
// 这是这条端点存在的全部理由那一半：计数表里「NetworkPolicy = 0」与
// 「NetworkPolicy 因权限不足根本没采到」长得一模一样，而后者被读成前者
// 的后果是平台推荐一份 default-deny。断言键的缺席而不是断言它等于某个
// 哨兵值——前端拿不到数字，就渲染不出数字。
func TestFailedResourceCarriesNoCountOnTheWire(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg, &fakeCollection{summary: partialSummary()})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	rec := callWith(t, h, http.MethodGet, collectionPath, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resources := collectionResources(t, bodyOf(t, rec))

	failed, ok := resources[string(snapshot.ResourceNetworkPolicy)]
	if !ok {
		t.Fatal("NETWORKPOLICY is missing from the summary — a resource nobody was permitted " +
			"to read must still appear, otherwise its absence reads as 'there are none'")
	}
	if _, has := failed["count"]; has {
		t.Errorf("a failed resource carries a count key: %s — "+
			"that number will be rendered as 'this cluster has no policies'", failed["count"])
	}
	var reason string
	if raw, has := failed["failureReason"]; !has {
		t.Fatal("a failed resource carries no failureReason — the operator cannot act on it")
	} else if err := json.Unmarshal(raw, &reason); err != nil {
		t.Fatalf("failureReason is not a string: %v", err)
	}
	if reason != string(snapshot.FailureForbidden) {
		t.Errorf("failureReason = %q, want FORBIDDEN", reason)
	}
}

// 一个真实的 0 必须仍然是 0，而不是被当成失败。
//
// 与上一条互为反面：只挡住失败的那一半，很容易顺手把所有 0 都藏起来，
// 而"这个集群确实一个 Namespace 都没有"是一个采到了的事实。
func TestObservedZeroStaysAZeroAndCarriesNoFailure(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg, &fakeCollection{summary: partialSummary()})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	resources := collectionResources(t, bodyOf(t, callWith(t, h, http.MethodGet, collectionPath, cookie)))

	zero, ok := resources[string(snapshot.ResourceNamespace)]
	if !ok {
		t.Fatal("NAMESPACE is missing from the summary")
	}
	if _, has := zero["failureReason"]; has {
		t.Error("an observed resource carries a failureReason — a real zero has been turned into a failure")
	}
	var count int
	if err := json.Unmarshal(zero["count"], &count); err != nil {
		t.Fatalf("count is not a number: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// ResourceReplicaSet 的缺席不是失败，也不是 0。
//
// 它只用于把 Pod 解到顶层控制器，从来不是被观测的资产（spec §4.2）。
// 按枚举补齐会凭空造出一条失败，而那条失败会让操作者去查一个不存在的
// RBAC 问题。
func TestReplicaSetAbsenceIsNeitherACountNorAFailure(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg, &fakeCollection{summary: partialSummary()})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	resources := collectionResources(t, bodyOf(t, callWith(t, h, http.MethodGet, collectionPath, cookie)))

	if item, ok := resources[string(snapshot.ResourceReplicaSet)]; ok {
		t.Errorf("REPLICASET appears in the summary as %v — it is never observed, "+
			"so anything shown for it is invented", item)
	}
}

/* --------------------------------------------------------------------- */
/* 2. 原始错误文本不过边界（规范 §19、§22）                                  */
/* --------------------------------------------------------------------- */

// 失败记录的 Detail 一个字都不出现在响应里。
//
// Detail 的内容完全由目标集群的 apiserver 决定：主机名、内部地址、
// ServiceAccount 名都在里面。能据以行动的是 Reason 这个封闭枚举
// （FORBIDDEN → 改 RBAC，TIMEOUT/UNAVAILABLE → 查网络），Detail 留在
// 库里给能读库的人。
//
// 断言整个响应体而不是某个字段：换一个字段名藏同一段文本，这条断言
// 照样红。
func TestRawAPIServerErrorNeverCrossesTheBoundary(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg, &fakeCollection{summary: partialSummary()})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	body := callWith(t, h, http.MethodGet, collectionPath, cookie).Body.String()

	for _, fragment := range []string{
		apiserverDetail,
		"apiserver.internal.prod-asia-1.corp",
		"system:serviceaccount:distill:collector",
		`"detail"`,
	} {
		if strings.Contains(body, fragment) {
			t.Errorf("the response carries %q — raw apiserver error text names internal hosts "+
				"and must not reach a client (规范 §19、§22)", fragment)
		}
	}
}

// 库里的原因不在封闭枚举里时，交出去的是 UNRECOGNIZED，不是那段文本。
//
// reason 在库里只是一列 VARCHAR(32)，封闭性只由写入侧的 Go 常量保证。
// 一次写错类型的改动会让 apiserver 的报错落进这一列 —— 而它短到放得下。
func TestUnknownFailureReasonIsNotPassedThrough(t *testing.T) {
	leak := `forbidden at 10.9.0.2:6443`
	summary := partialSummary()
	summary.Resources = []snapshotstore.ResourceOutcome{
		snapshotstore.NewFailedResource(string(snapshot.ResourceNetworkPolicy),
			snapshotstore.FailureRecord{Reason: leak, Detail: apiserverDetail}),
	}

	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg, &fakeCollection{summary: summary})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	rec := callWith(t, h, http.MethodGet, collectionPath, cookie)
	if strings.Contains(rec.Body.String(), "10.9.0.2") {
		t.Fatalf("an out-of-enum reason was passed through verbatim: %s", rec.Body.String())
	}
	resources := collectionResources(t, bodyOf(t, rec))
	var reason string
	if err := json.Unmarshal(resources[string(snapshot.ResourceNetworkPolicy)]["failureReason"], &reason); err != nil {
		t.Fatalf("failureReason is not a string: %v", err)
	}
	// 不折成 OTHER：OTHER 的含义是"采集器判定不出更具体的原因"，而这里的
	// 含义是"库里那个取值我们不认识"。合并会让漏登记一个原因永远看不出来。
	if reason != "UNRECOGNIZED" {
		t.Errorf("failureReason = %q, want UNRECOGNIZED", reason)
	}
}

// ObservedAt 是各 observed_* 表的 join 键，属于落库形态，不回传（§20、§35）。
func TestSummaryDoesNotLeakTheInternalJoinKey(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg, &fakeCollection{summary: partialSummary()})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	if body := callWith(t, h, http.MethodGet, collectionPath, cookie).Body.String(); strings.Contains(body, "observedAt") {
		t.Errorf("the response carries observedAt: %s", body)
	}
}

/* --------------------------------------------------------------------- */
/* 3. 三种"没有数字"必须分得开                                              */
/* --------------------------------------------------------------------- */

// 从未采集过的集群回业务失败，不是一张空摘要。
//
// 一张空摘要会让"这个集群从没被采过"看起来和"采过、什么都没有"一样，
// 而这两件事的下一步动作完全不同。
func TestClusterWithNoRunIsNotAnEmptySummary(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg,
		&fakeCollection{err: snapshotstore.ErrNoRun})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	rec := callWith(t, h, http.MethodGet, collectionPath, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — 从未采集是正常状态，不该计进服务错误率", rec.Code)
	}
	body := bodyOf(t, rec)
	if got := body["code"]; got != float64(response.CodeNotFound) {
		t.Errorf("code = %v, want %d", got, response.CodeNotFound)
	}
	if body["data"] != nil {
		t.Errorf("data = %v, want null — 没有运行就没有摘要", body["data"])
	}
}

// 本部署没有采集读取端时，答的不是"这个集群没有采集记录"。
//
// 这是当前真实部署的形态：cmd/distill-api 还没有装配 Collection。
// 把它答成 20002，一次装配缺失会看起来和一个刚注册的集群完全一样。
func TestMissingCollectionReaderIsNotReportedAsNoRun(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	rec := callWith(t, h, http.MethodGet, collectionPath, cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeDependencyUnavailable) {
		t.Errorf("code = %v, want %d", got, response.CodeDependencyUnavailable)
	}
}

// 读取失败只进日志，回给调用方的是固定文案。
func TestReadFailureAnswersWithoutTheUnderlyingError(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithCollection(t, reg,
		&fakeCollection{err: errors.New("dial tcp 10.9.0.31:3306: connect: connection refused")})
	cookie := sessionCookie(t, sessions, reg, adminUser, registry.RoleAdmin)

	rec := callWith(t, h, http.MethodGet, collectionPath, cookie)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.9.0.31") {
		t.Errorf("the response carries the database address: %s", rec.Body.String())
	}
}

/* --------------------------------------------------------------------- */
/* 4. 授权                                                                */
/* --------------------------------------------------------------------- */

// 只读账号够不着这条端点，且拒绝发生在读取之前。
//
// adminRoutes() 已经把这条路径列进去，这条用例多验证一件事：被拒的请求
// 一次库都没查。授权若发生在 handler 内部，一个 403 仍然会先把真实集群
// 的资产盘点读出来。
func TestViewerNeverReachesTheCollectionReader(t *testing.T) {
	reg := fixtureSource()
	fake := &fakeCollection{summary: partialSummary()}
	h, sessions, _ := newTestRouterWithCollection(t, reg, fake)
	cookie := sessionCookie(t, sessions, reg, "readonly", registry.RoleViewer)

	rec := callWith(t, h, http.MethodGet, collectionPath, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — 采集摘要是管理员级读取", rec.Code)
	}
	if fake.calls != 0 {
		t.Errorf("the reader was queried %d times on a refused request", fake.calls)
	}
}

// 未登录拿到的是 401，不是 403：两者的处置相反。
func TestAnonymousCannotReadTheCollectionSummary(t *testing.T) {
	reg := fixtureSource()
	h, _, _ := newTestRouterWithCollection(t, reg, &fakeCollection{summary: partialSummary()})

	rec := callWith(t, h, http.MethodGet, collectionPath, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
