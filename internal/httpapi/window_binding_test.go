package httpapi_test

import (
	"context"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// 本文件绑的是**调用方**：默认时间窗按集群解析这件事，只有在每一条会用到它
// 的路由上都真的发生了才算数。
//
// 守卫本身（分派、fixture 的固定范围、采集侧取最近一次摄入窗口）在
// cmd/distill-api 与 internal/collectstore 各自有测试。那些测试全绿并不能说明
// 边界层真的去问了它 —— 而「守卫被验过、没有东西证明调用方仍然在调它」正是
// 这个项目已经出过二十三次的那种测不出来的测试，最近三次全在这条链路上。
//
// **路由清单从路由器自己走出来（chi.Walk），不写死六条。** 上一版的形态是
// design doc 里一张列了六条路径的表，而一张手写的表漏掉一格不会让任何东西
// 变红。新增第七条窗口路径时，它会被自动走到并接受同一条断言。
//
// 剩下的口子如实记在这里：一条**需要特定请求体才走得到窗口**的新路由，若没
// 有人往 routeBody 里补一格，遍历会在它到达窗口之前就被参数校验拦下，于是它
// 不受第一条断言保护。这个口子不是沉默的 —— 第二条断言按 store.Reader 反射
// 出「带时间窗入参的读方法」，少覆盖一个就会点名报错。

// probeCluster 是遍历里全部路由点名的集群。取 fixture 里真实存在的那个：
// 一个不存在的集群会让多数 handler 在触到窗口之前就早退成 404。
const probeCluster = "prod-asia-1"

// probeWindowFor 造一段只属于这个集群的时间窗。
//
// 按集群 ID 散列，而不是全局一个常量：这样第一条断言同时管住两件事 ——
// 「窗口不是某个装配时算好的常量」与「问窗口用的集群就是查询用的那个集群」。
// 后者错了同样是静默的：一份关于 A 的报告被 B 的观测范围筛过，两个数字都
// 答得出、都不报错。
//
// 起点刻意取 2077：它与 fixture 数据集那段 2026-07-31 的约 36 秒毫无交集，
// 一旦某条路径塌回那个旧常量，断言里两个时间戳一眼就能看出差别。
func probeWindowFor(clusterID string) store.TimeWindow {
	h := fnv.New32a()
	_, _ = h.Write([]byte(clusterID))
	from := time.Date(2077, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(h.Sum32()%100_000) * time.Second)
	return store.TimeWindow{From: from, To: from.Add(time.Hour)}
}

// windowCall 是一次带时间窗的读调用留下的痕迹。
type windowCall struct {
	method  string
	cluster string
	window  store.TimeWindow
}

// windowProbeReader 记下每个带时间窗的读方法收到的集群与窗口。
//
// 不返回任何有内容的结果：这些用例断言的是「窗口从哪来」，不是 handler 的
// 结论。返回值只要能让 handler 走完就够。
type windowProbeReader struct {
	calls []windowCall
}

func (p *windowProbeReader) DefaultWindow(_ context.Context, clusterID string) (store.TimeWindow, error) {
	return probeWindowFor(clusterID), nil
}

func (p *windowProbeReader) Topology(
	context.Context, string, store.TopologyLevel,
) (store.Topology, error) {
	return store.Topology{}, nil
}

func (p *windowProbeReader) Quality(context.Context, string) (store.Quality, error) {
	return store.Quality{}, nil
}

func (p *windowProbeReader) Flow(context.Context, string) (store.Decision, bool, error) {
	return store.Decision{}, false, nil
}

func (p *windowProbeReader) Flows(_ context.Context, f store.FlowFilter) (store.FlowPage, error) {
	p.calls = append(p.calls, windowCall{method: "Flows", cluster: f.Cluster, window: f.Window})
	return store.FlowPage{Window: f.Window}, nil
}

func (p *windowProbeReader) Security(
	_ context.Context, clusterID string, w store.TimeWindow,
) (store.SecurityReport, error) {
	p.calls = append(p.calls, windowCall{method: "Security", cluster: clusterID, window: w})
	return store.SecurityReport{}, nil
}

func (p *windowProbeReader) PolicyPreviewAtGranularity(
	_ context.Context, clusterID, _ string, w store.TimeWindow, _ policygen.Granularity,
) (store.PolicyPreview, error) {
	p.calls = append(p.calls,
		windowCall{method: "PolicyPreviewAtGranularity", cluster: clusterID, window: w})
	return store.PolicyPreview{Window: w}, nil
}

func (p *windowProbeReader) EnsureRuleExists(
	_ context.Context, clusterID, _, _, _ string,
	_ policygen.OverrideDecision, w store.TimeWindow,
) error {
	p.calls = append(p.calls, windowCall{method: "EnsureRuleExists", cluster: clusterID, window: w})
	return nil
}

// routeParams 填掉路由模式里的路径参数。取值本身无关紧要，
// 除了 {clusterID} —— 那一个正是被断言的东西。
var routeParams = map[string]string{
	"{clusterID}": probeCluster,
	"{importID}":  "1",
	"{flowID}":    "no-such-flow",
	"{repoID}":    "policies",
	"{username}":  "target-user",
	"{agentID}":   "0011223344556677",
}

// routeBody 是某些路由走到时间窗之前必须先通过的请求体。
//
// 只列那些「参数校验排在取窗口之前」的路由。缺一格不会静默：那条路由到不了
// 窗口，于是它带时间窗的那个读方法在覆盖面断言里少一格并被点名。
var routeBody = map[string]string{
	"/api/v1/clusters/{clusterID}/rule-overrides": `{
		"namespace": "payment", "workload": "payment",
		"fingerprint": "` + strings.Repeat("ab", 32) + `",
		"decision": "ENABLE", "reason": "window binding probe"
	}`,
}

// newWindowProbeRouter 装一个尽量“什么前提都齐了”的路由器。
//
// 集群绑好策略仓库、校验结论 OK、写入器装着：这些前提不齐的路由会在触到时间
// 窗之前就早退（写回出计划会停在「没有绑定」上），于是它悄悄退出遍历的覆盖面。
// 前提越齐，第一条断言管得越宽。
func newWindowProbeRouter(t *testing.T, reader store.Reader) (http.Handler, *http.Cookie) {
	t.Helper()
	reg := fixtureSource()
	reg.repos[gitRepoID] = boundRepo()
	c := reg.clusters[probeCluster]
	c.Git = &registry.GitBinding{
		RepoID: gitRepoID, PolicyPath: writebackPolicyPath,
		VerifyResult: registry.BindingVerifyNotVerified,
	}
	reg.clusters[probeCluster] = c

	h, _, cookie := buildTestRouterWithLog(t, reader, reg,
		&stubGitVerifier{result: registry.BindingVerifyOK}, &fakePolicyWriter{},
		nil, "ERROR", io.Discard)
	return h, cookie
}

// walkedRoute 是一条被遍历到的路由，以及它在探针上留下的调用。
type walkedRoute struct {
	method string
	route  string
	path   string
	calls  []windowCall
}

// walkWindowedRoutes 走一遍**真实路由表**，返回其中确实用到时间窗的那些路由。
//
// 每条路由重装一次路由器与注册表：被走到的路由里有 DELETE 集群、DELETE 会话
// 这类会改掉后续前提的动作，共用一份状态会让遍历后半段整体早退成 404 或 401，
// 而那种失败是安静的 —— 断言不会变红，它只是不再检查任何东西。
func walkWindowedRoutes(t *testing.T) []walkedRoute {
	t.Helper()

	probeRouter, _ := newWindowProbeRouter(t, &windowProbeReader{})
	routes, ok := probeRouter.(chi.Routes)
	if !ok {
		t.Fatalf("router is %T, which cannot be walked — this test can no longer see the route table", probeRouter)
	}

	var out []walkedRoute
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := route
		for name, value := range routeParams {
			path = strings.ReplaceAll(path, name, value)
		}
		if strings.Contains(path, "{") {
			t.Errorf("route %s has a path parameter this test does not know how to fill", route)
			return nil
		}
		if method == http.MethodPost && path == "/api/v1/sessions" {
			// 登录：没有会话也就没有集群可言。
			return nil
		}

		probe := &windowProbeReader{}
		h, cookie := newWindowProbeRouter(t, probe)
		// cluster 查询参数只有 /flows 读它；其余路由忽略未知参数。统一挂上
		// 去，是为了让不带路径参数的那条路由也点名同一个集群。
		callProbe(t, h, method, path+"?cluster="+probeCluster, cookie, routeBody[route])

		if len(probe.calls) > 0 {
			out = append(out, walkedRoute{method: method, route: route, path: path, calls: probe.calls})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the walk reached no windowed read at all; every assertion below would be vacuous")
	}
	return out
}

// callProbe 发一次带会话的请求。body 为空时按方法给一个空对象。
func callProbe(
	t *testing.T, h http.Handler, method, path string, cookie *http.Cookie, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	if body == "" && (method == http.MethodPost || method == http.MethodPut) {
		body = "{}"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// windowedReaderMethods 按反射列出 store.Reader 上**入参带时间窗**的读方法。
//
// 反射而不是写死清单：新增一个带时间窗的读方法却没有任何路由把按集群解析出来
// 的窗口交给它时，必须有东西变红。FlowFilter 那种把窗口包在结构体里的入参也
// 算 —— 少认它一格，Flows 就整条从覆盖面里消失了。
//
// DefaultWindow 自己不在其中：它的时间窗是**返回值**，它是窗口的来源而不是
// 窗口的消费方。
func windowedReaderMethods(t *testing.T) []string {
	t.Helper()
	iface := reflect.TypeOf((*store.Reader)(nil)).Elem()
	winT := reflect.TypeOf(store.TimeWindow{})
	var out []string
	for i := range iface.NumMethod() {
		m := iface.Method(i)
		for j := range m.Type.NumIn() {
			if carriesWindow(m.Type.In(j), winT) {
				out = append(out, m.Name)
				break
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("store.Reader declares no method taking a time window; this test can no longer see them")
	}
	return out
}

// carriesWindow 判断一个入参类型是不是时间窗，或直接装着一个时间窗。
func carriesWindow(in, winT reflect.Type) bool {
	if in == winT {
		return true
	}
	if in.Kind() != reflect.Struct {
		return false
	}
	for i := range in.NumField() {
		if in.Field(i).Type == winT {
			return true
		}
	}
	return false
}

// 每一条用到时间窗的路由，交给数据层的都必须是**这个集群**的默认窗口。
//
// 这条断言替代的是删掉的 httpapi.Deps.DefaultWindow：那是一个装配时算一次、
// 六条路径共用的常量，取的是合成数据集自己的约 36 秒（design doc 2026-08-18
// §3.1）。塌回任何一个跨集群共用的窗口，或者拿错集群去问窗口，都会在这里
// 变红并把两段时间一起打印出来。
func TestEveryWindowedPathResolvesTheWindowFromTheCluster(t *testing.T) {
	walked := walkWindowedRoutes(t)

	seen := map[string]bool{}
	for _, rt := range walked {
		for _, c := range rt.calls {
			seen[c.method] = true
			want := probeWindowFor(c.cluster)
			if c.window != want {
				t.Errorf("%s %s handed %s the window %v–%v for cluster %q, want %v–%v — "+
					"the default window must be resolved per cluster from the reader, not from a "+
					"constant shared across data sources (design doc 2026-08-18 §3.1)",
					rt.method, rt.route, c.method,
					c.window.From, c.window.To, c.cluster, want.From, want.To)
			}
		}
	}

	// 覆盖面：每个带时间窗入参的读方法都必须至少被一条路由喂到过。少一个说明
	// 遍历根本没走到它，于是上面那条断言对它不成立 —— 而那正是「守卫被验过、
	// 没有东西证明调用方在调它」的形状。
	for _, m := range windowedReaderMethods(t) {
		if !seen[m] {
			t.Errorf("no walked route ever handed a resolved window to store.Reader.%s; "+
				"the walk cannot reach it (a missing routeBody entry?), so nothing proves that "+
				"path resolves its window per cluster", m)
		}
	}
}

// 一个还没有摄入运行的 COLLECTED 集群，在**每一条**用到时间窗的路由上都答
// 「还没有可用的采集数据」。
//
// 路由清单不是手写的：它就是上一条测试从路由表里走出来的那一组，因此两条
// 断言永远看着同一批路径。
//
// 这一条钉住的是「答不出窗口时不得兜底」（design doc 2026-08-18 §3.1）。
// 兜底的那一段时间没有任何证据支持，而它算出来的判定与一份有证据的判定在
// 界面上长得一模一样：窗口里没被观测到的连接会被读成「这条规则没有流量、
// 可以收紧」—— 这个平台唯一那个单向的失败方向。
//
// 20005 而不是 500：这不是服务故障，操作者该去跑摄入，不是去查服务。
func TestEveryWindowedPathRefusesWhenTheClusterHasNoIngestRun(t *testing.T) {
	for _, rt := range walkWindowedRoutes(t) {
		h, cookie := newWindowProbeRouter(t, noCollectionReader{})
		rec := callProbe(t, h, rt.method, rt.path+"?cluster="+probeCluster, cookie, routeBody[rt.route])

		if rec.Code != http.StatusOK {
			t.Errorf("%s %s status = %d, want 200 — a cluster that has never been ingested is not "+
				"a server fault", rt.method, rt.route, rec.Code)
		}
		if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNoUsableCollection) {
			t.Errorf("%s %s code = %v, want %d（该集群还没有可用的采集数据）— answering anything "+
				"else means this path found a window somewhere other than the cluster's own "+
				"ingest evidence", rt.method, rt.route, got, int(response.CodeNoUsableCollection))
		}
		if bodyOf(t, rec)["data"] != nil {
			t.Errorf("%s %s returned data alongside the refusal", rt.method, rt.route)
		}
	}
}

// 同一批路由上，「资产采过、流量没摄入过」答的是 20009，不是 20005。
//
// 与上一条并列而不是替代它：上一条钉的是「答不出窗口时不得兜底」，这一条钉
// 的是**拒绝时说的是哪一句**。两句话指向完全相反的处置 —— 20005 让人去重跑
// 资产采集（已经成功过），20009 让人去部署采集器或开流量日志（真正缺的那一步）。
//
// 走同一份路由表，因为映射在 writeReaderError 里是共用的、路由却不是：
// 只测其中一条，另外几条改走别的错误处理时全套仍然全绿。
func TestEveryWindowedPathNamesTheMissingIngestRunSpecifically(t *testing.T) {
	// 这条路由**故意**不在没有摄入时拒绝：记一个人工决定不需要看过流量，
	// 候选集来自资产（override_handler 里那段 ErrNoFlowIngest 吸收）。挡住它，
	// 操作者能看见几百条推荐却一条都确认不了。
	//
	// 写成名单而不是在断言里跳过，是为了让"名单里那条路由已经不存在了"能红：
	// 下面那个 exempted 检查走的是同一份走出来的路由表。
	const overrideRoute = "/api/v1/clusters/{clusterID}/rule-overrides"
	exempted := false

	for _, rt := range walkWindowedRoutes(t) {
		if rt.route == overrideRoute && rt.method == http.MethodPost {
			exempted = true
			continue
		}
		h, cookie := newWindowProbeRouter(t, noFlowIngestReader{})
		rec := callProbe(t, h, rt.method, rt.path+"?cluster="+probeCluster, cookie, routeBody[rt.route])

		if rec.Code != http.StatusOK {
			t.Errorf("%s %s status = %d, want 200 — a cluster that has assets but no flow "+
				"ingest is not a server fault", rt.method, rt.route, rec.Code)
		}
		if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNoIngestRun) {
			t.Errorf("%s %s code = %v, want %d（还没有过任何一次流量摄入）— %d tells the "+
				"operator to re-run an asset collection that has already succeeded, which "+
				"will never make this screen answer",
				rt.method, rt.route, got, int(response.CodeNoIngestRun),
				int(response.CodeNoUsableCollection))
		}
	}

	if !exempted {
		t.Errorf("the walk never reached POST %s; the exemption above now covers nothing, "+
			"and whatever that route became is unguarded", overrideRoute)
	}
}
