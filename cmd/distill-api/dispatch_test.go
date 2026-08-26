package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

// mixedFleet 是本文件全部用例共用的注册表：一个登记为 COLLECTED 的集群，
// 一个登记为 FIXTURE 的集群。
//
// COLLECTED 那个刻意取 fixture 里**确实有整套数据**的 ID：来源若是从「有没有
// 数据」推断出来的，它一定会被推断成 FIXTURE，而那正是要挡住的那件事
// （design doc 2026-08-18 §2）。
func mixedFleet() stubClusterSource {
	return stubClusterSource{clusters: []registry.Cluster{
		{ID: fixtureBackedID, DataSource: registry.DataSourceCollected},
		{ID: fixtureBackedPeer, DataSource: registry.DataSourceFixture},
	}}
}

// demoWindow 取合成数据集的实际时间范围，与装配层注入 DefaultWindow 的取法
// 一致。写死一个"最近 N 天"会让这些用例在某天自己变绿（没有数据 ⇒ 没有泄漏）。
func demoWindow(src store.ClusterSource) store.TimeWindow {
	return newFixtureReader(src).DataWindow()
}

// dispatchCase 是「这个读方法确实经过了分派」这条性质在某一个方法上的落点。
//
// method 是 store.Reader 上的方法名，assertEveryReaderMethodCovered 拿它对账 ——
// 与 reader_test.go 共用同一个对账函数，理由也一样：手写的表漏掉一格不会让
// 任何东西变红，而 I1 那条缝正是这样活过四轮任务评审的。
//
// call 只负责调用，把 error 交回来；断言在调用方统一做。
type dispatchCase struct {
	method string
	name   string
	call   func(d *dispatchReader) error
}

// dispatchCasesFor 列出六个读方法各自打在指定集群上的一次调用。
//
// 每个方法一格、且六格全部指向同一个集群：分派是逐方法写的（每个方法各自
// 调一次 readerOf），因此「有一个方法没走分派」是一个逐方法的失败模式 ——
// 它会照常作答，只是答的是另一份数据，而页面上没有任何症状。
func dispatchCasesFor(ctx context.Context, clusterID string, window store.TimeWindow) []dispatchCase {
	return []dispatchCase{
		{method: "DefaultWindow", name: "DefaultWindow", call: func(d *dispatchReader) error {
			_, err := d.DefaultWindow(ctx, clusterID)
			return err
		}},
		{method: "Topology", name: "Topology", call: func(d *dispatchReader) error {
			_, err := d.Topology(ctx, clusterID, store.LevelNamespace)
			return err
		}},
		{method: "Quality", name: "Quality", call: func(d *dispatchReader) error {
			_, err := d.Quality(ctx, clusterID)
			return err
		}},
		{method: "Security", name: "Security", call: func(d *dispatchReader) error {
			_, err := d.Security(ctx, clusterID, window)
			return err
		}},
		{method: "PolicyPreviewAtGranularity", name: "PolicyPreviewAtGranularity",
			call: func(d *dispatchReader) error {
				_, err := d.PolicyPreviewAtGranularity(ctx, clusterID, "payment", window,
					policygen.GranularityNamespace)
				return err
			}},
		{method: "Reconciliation", name: "Reconciliation", call: func(d *dispatchReader) error {
			_, err := d.Reconciliation(ctx, clusterID, window)
			return err
		}},
		{method: "Retirement", name: "Retirement", call: func(d *dispatchReader) error {
			_, err := d.Retirement(ctx, clusterID, window)
			return err
		}},
		{method: "LivePolicies", name: "LivePolicies", call: func(d *dispatchReader) error {
			_, err := d.LivePolicies(ctx, clusterID)
			return err
		}},
		{method: "DeletionImpact", name: "DeletionImpact", call: func(d *dispatchReader) error {
			_, err := d.DeletionImpact(ctx, clusterID, window, []networkingv1.NetworkPolicy{{
				ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "allow-gateway"},
			}})
			return err
		}},
		{method: "EnsureRuleExists", name: "EnsureRuleExists", call: func(d *dispatchReader) error {
			return d.EnsureRuleExists(ctx, clusterID, "payment", "payment",
				"deadbeef", policygen.DecisionDisable, window)
		}},
		{method: "Flows", name: "Flows(cluster named)", call: func(d *dispatchReader) error {
			_, err := d.Flows(ctx, store.FlowFilter{Cluster: clusterID, Window: window})
			return err
		}},
		{method: "Flow", name: "Flow(collected-shaped id)", call: func(d *dispatchReader) error {
			_, _, err := d.Flow(ctx, collectedFlowID(clusterID))
			return err
		}},
	}
}

// 六个读方法**每一个**都必须经过分派，一个都不能直连某个 Reader。
//
// 探针是「装配方没给采集侧 Reader」这种形态：此时 readerFor 对一个登记为
// COLLECTED 的集群返回 ErrNoCollectedReader，于是「这次调用有没有按登记的
// 来源分派」在每个方法上都有一个可观测的落点 —— 走了分派就是这个哨兵，
// 没走（或走了却选错）就不是。
//
// 这条测试绑的是**调用方**，不是守卫本身。readerFor 那三条性质已经在
// reader_test.go 里各自有测试；那些测试全绿并不能说明六个读方法真的去调了
// 它 —— 而「守卫被验过、没有东西证明调用方仍然在调它」正是这个项目已经出过
// 二十次的那种测不出来的测试。
//
// 集群刻意取 fixture 里有整套数据的那个：分派若改成从「有没有数据」推断，
// 它会被推断成 FIXTURE 并照常作答一份合成报告，于是这里的哨兵断言变红。
func TestEveryReadMethodDispatchesOnTheRegisteredSource(t *testing.T) {
	src := mixedFleet()
	ctx := context.Background()
	window := demoWindow(src)

	// collected 为 nil：装配方没接上采集侧读取面。正确答案仍然是「没有数据」，
	// 不是"既然没有采集侧 Reader，就先用合成数据顶着"（安全规范 §49）。
	d := newDispatchReader(src, nil)

	cases := dispatchCasesFor(ctx, fixtureBackedID, window)
	// 先钉住覆盖面，再跑用例：新增第七、第八个读方法却忘记在这里补一格，
	// 表现必须是这里红并点名那个方法，而不是沉默地少测一格。
	assertEveryReaderMethodCovered(t, toLeakCases(cases))

	for _, tc := range cases {
		err := tc.call(d)
		if !errors.Is(err, ErrNoCollectedReader) {
			t.Errorf("%s [%s] on a cluster declared COLLECTED: error = %v, "+
				"want ErrNoCollectedReader — this read method did not dispatch on the "+
				"registered data source (design doc 2026-08-18 §2)", tc.name, tc.method, err)
		}
	}

	// 对照组：同一个分派器对登记为 FIXTURE 的集群照常供数。少了这一条，
	// 一个无条件返回 ErrNoCollectedReader 的分派器也能让上面全部通过 ——
	// 而那是把演示环境一起关掉。
	topo, err := d.Topology(ctx, fixtureBackedPeer, store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology(%s declared FIXTURE) error = %v, want the demo cluster still served",
			fixtureBackedPeer, err)
	}
	if len(topo.Nodes) == 0 {
		t.Errorf("Topology(%s declared FIXTURE) returned no nodes; a declared demo cluster "+
			"must still be served through the dispatcher", fixtureBackedPeer)
	}
}

// 一个登记为 FIXTURE 的集群的默认窗口是合成数据集自己的范围，而且它是**固定的**。
//
// 期望值不取 DataWindow()，而是从 fixture.Load() 自己扫一遍最早与最晚那条
// flow：拿被测对象的答案当期望，等于让测试自问自答 —— DataWindow 整个换成
// 一个"最近 N 天"的相对区间时，那种写法照样全绿。
//
// 固定这件事本身就是要求（design doc 2026-08-18 §3.1）：fixture 的数据钉在
// 过去某一天，任何相对窗口都会随真实时间推移而在某天返回 0 条 —— demo 会在
// 没有人改动代码的情况下自己坏掉，而症状是"页面上什么都没有"。
func TestDefaultWindowOfAFixtureClusterIsTheDatasetRange(t *testing.T) {
	src := mixedFleet()
	d := newDispatchReader(src, collectstore.New(nil, src))

	got, err := d.DefaultWindow(context.Background(), fixtureBackedPeer)
	if err != nil {
		t.Fatalf("DefaultWindow(%s declared FIXTURE) error = %v", fixtureBackedPeer, err)
	}

	flows := fixture.Load().Flows
	if len(flows) == 0 {
		t.Fatal("the fixture holds no flows; this test would prove nothing")
	}
	first, last := flows[0].Flow.Timestamp, flows[0].Flow.Timestamp
	for _, f := range flows {
		if f.Flow.Timestamp.Before(first) {
			first = f.Flow.Timestamp
		}
		if f.Flow.Timestamp.After(last) {
			last = f.Flow.Timestamp
		}
	}
	// 上界是末条之后一秒：窗口左闭右开，取末条时间戳本身会把最后一条排除在外。
	want := store.TimeWindow{From: first, To: last.Add(time.Second)}
	if got != want {
		t.Errorf("DefaultWindow(%s declared FIXTURE) = %v–%v, want the dataset's own range %v–%v — "+
			"a demo cluster's window must stay pinned to the data, not slide with wall-clock time",
			fixtureBackedPeer, got.From, got.To, want.From, want.To)
	}
	if !got.To.Before(time.Now()) {
		t.Errorf("DefaultWindow(%s declared FIXTURE) ends at %v, which is not in the fixture's "+
			"fixed past; a relative window makes the demo break by itself on some future day",
			fixtureBackedPeer, got.To)
	}
}

// 不点名集群时答不出默认窗口，且拒绝要说得出原因。
//
// 与 Flows 同一个哨兵（store.ErrClusterRequired → 20006）：默认窗口是**按
// 集群**推出来的结论，没有集群就没有这个结论。落到 readerOf 上会答成"没有
// 这个集群"，那是另一句话，会把操作者支去查一个集群 ID 拼写问题。
func TestDefaultWindowWithoutAClusterIsRefused(t *testing.T) {
	src := mixedFleet()
	d := newDispatchReader(src, collectstore.New(nil, src))

	if _, err := d.DefaultWindow(context.Background(), ""); !errors.Is(err, store.ErrClusterRequired) {
		t.Errorf("DefaultWindow(no cluster) error = %v, want store.ErrClusterRequired", err)
	}
}

// 分派器选出的 Reader 就是 readerFor 按登记来源选出的那一个。
//
// 与上一条互补：上一条证明每个方法都经过分派，这一条证明分派选对了 ——
// 一个 COLLECTED 集群拿到的是采集侧那个**具体的**实例，不是另装的一个。
func TestDispatchPicksTheReaderTheClusterDeclares(t *testing.T) {
	src := mixedFleet()
	collected := collectstore.New(nil, src)
	d := newDispatchReader(src, collected)
	ctx := context.Background()

	got, err := d.readerOf(ctx, fixtureBackedID)
	if err != nil {
		t.Fatalf("readerOf(%s declared COLLECTED) error = %v", fixtureBackedID, err)
	}
	if got != store.Reader(collected) {
		t.Errorf("readerOf(%s declared COLLECTED) = %T, want the collected reader the "+
			"assembly handed in", fixtureBackedID, got)
	}

	got, err = d.readerOf(ctx, fixtureBackedPeer)
	if err != nil {
		t.Fatalf("readerOf(%s declared FIXTURE) error = %v", fixtureBackedPeer, err)
	}
	if _, ok := got.(*store.FixtureReader); !ok {
		t.Errorf("readerOf(%s declared FIXTURE) = %T, want *store.FixtureReader", fixtureBackedPeer, got)
	}

	// 没登记的集群没有来源可读，因此没有 Reader 可选。答「没有这个集群」，
	// 不是随手挑一个：任何一种兜底都会让一次拼写错误得到一份看上去正常的
	// 报告（安全规范 §49）。
	if _, err := d.readerOf(ctx, "prod-typo-1"); !errors.Is(err, store.ErrClusterNotFound) {
		t.Errorf("readerOf(unregistered cluster) error = %v, want ErrClusterNotFound", err)
	}
}

// 不点名集群的流量列表一律拒绝，且拒绝要说得出原因。
//
// 三种可能里只有这一种能要（design doc 2026-08-18 §3.2）：合并两个 Reader
// 会让演示流量与真实流量同屏；只答 fixture 那一半会让真集群的流量整体缺席，
// 而**缺席在这个平台上读起来是「这条规则没有流量、可以收紧」**。
//
// 两件事一起断言，缺一不可：
//
//   - 返回的是 store.ErrClusterRequired（httpapi 认得它，映射成 20006）；
//   - **一条记录都没有**。只断言 error 的话，一个"既返回错误又返回列表"的
//     实现照样通过，而调用方里恰好有一个先取 page 再看 err 的，就把演示流量
//     当成全量交出去了。
func TestFlowsWithoutAClusterIsRefused(t *testing.T) {
	src := mixedFleet()
	ctx := context.Background()
	window := demoWindow(src)
	d := newDispatchReader(src, collectstore.New(nil, src))

	page, err := d.Flows(ctx, store.FlowFilter{Window: window, Limit: 1000})
	if !errors.Is(err, store.ErrClusterRequired) {
		t.Errorf("Flows(no cluster) error = %v, want store.ErrClusterRequired — a flow list "+
			"that spans two data sources is either half synthetic or silently missing the "+
			"real cluster (design doc 2026-08-18 §3.2)", err)
	}
	if len(page.Items) != 0 || page.Total != 0 {
		t.Errorf("Flows(no cluster) returned %d of %d records; the refusal must hand back "+
			"nothing at all", len(page.Items), page.Total)
	}

	// 对照组：点名之后照常供数。少了这一条，一个对所有 Flows 调用都拒绝的
	// 实现也能让上面通过 —— 而那是把流量页整个关掉。
	named, err := d.Flows(ctx, store.FlowFilter{Cluster: fixtureBackedPeer, Window: window, Limit: 1000})
	if err != nil {
		t.Fatalf("Flows(cluster=%s) error = %v, want the demo cluster still served", fixtureBackedPeer, err)
	}
	if len(named.Items) == 0 {
		t.Errorf("Flows(cluster=%s) returned no records; naming a cluster must still work",
			fixtureBackedPeer)
	}
}

// 按 ID 下钻：ID 解不开时交给 fixture Reader，而那条路交不出 COLLECTED
// 集群的任何东西。
//
// 这是「§3.1 第 2 步不是一条通往合成数据的漏路」在装配层的落点。两半各自
// 独立、两半都要成立：
//
//  1. 一条两端都是 FIXTURE 的记录照常解得开 —— 否则演示环境的下钻整个坏掉，
//     而"什么都答不出来"也能让第 2 半通过；
//  2. 一条**一端是 COLLECTED 集群**的记录解不开。它的另一端仍是 FIXTURE，
//     所以「任一端已注册即放行」的判据会把它放出来 —— 一条写着真集群名字的
//     合成判定。挑一条两端都在 COLLECTED 集群的记录测不出这件事。
//
// 第 2 半靠的是 newFixtureReader 收窄后的数据源（FixtureReader.visibleFlows
// 要求两端都在册），不是这里的分派：把 Flow 的分派整个拆掉，它仍然成立。
func TestFlowFallsThroughToFixtureWithoutLeakingACollectedCluster(t *testing.T) {
	src := mixedFleet()
	ctx := context.Background()
	d := newDispatchReader(src, collectstore.New(nil, src))

	crossID := crossClusterFlowID(t, fixtureBackedID, fixtureBackedPeer)
	if _, ok := collectstore.ClusterOfFlowID(crossID); ok {
		t.Fatalf("fixture flow id %q decodes as a collected id; this probe would exercise "+
			"the dispatch branch, not the fall-through", crossID)
	}
	if _, ok, err := d.Flow(ctx, crossID); err != nil || ok {
		t.Errorf("Flow(%s) = ok %v, err %v; a record with one endpoint in a cluster declared "+
			"COLLECTED must not resolve through the fixture fall-through (design doc §3.1)",
			crossID, ok, err)
	}

	// 对照组：两端都登记为 FIXTURE 的记录仍然解得开。
	peerID := intraClusterFlowID(t, fixtureBackedPeer)
	if _, ok, err := d.Flow(ctx, peerID); err != nil || !ok {
		t.Errorf("Flow(%s) = ok %v, err %v; a record wholly inside a declared demo cluster "+
			"must still resolve", peerID, ok, err)
	}
}

// intraClusterFlowID 取一条两端都在该集群里的 fixture 流量 ID。
func intraClusterFlowID(t *testing.T, clusterID string) string {
	t.Helper()
	for _, f := range fixture.Load().Flows {
		if f.Flow.Source.ClusterID == clusterID && f.Flow.Dest.ClusterID == clusterID {
			return f.ID
		}
	}
	t.Fatalf("fixture has no flow wholly inside %s; the control group would prove nothing", clusterID)
	return ""
}

// collectedFlowID 造一个采集侧形状、点名 clusterID 的流量 ID。
//
// 手写编码是不得已：flowIDOf 在 collectstore 包内，装配层拿不到它。因此这里
// **每次都用 collectstore.ClusterOfFlowID 自检一遍**（见 mustNameCluster）——
// 编码一旦改动，探针会当场失败并说明原因，而不是悄悄退化成一个"解不开的
// ID"，让用它的那条用例改去测 fixture 兜底那条路并照常变绿。
//
// 编码与 ClusterOfFlowID 的对账在 collectstore 包内另有一条内部测试
// （TestClusterOfFlowIDReadsBackWhatFlowIDOfWrote），那一条绑的是导出入口与
// 真正的编码器；这里绑的是探针与导出入口。
func collectedFlowID(clusterID string) string {
	from := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC).UnixMicro()
	to := time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC).UnixMicro()
	payload := strings.Join([]string{
		clusterID,
		strconv.FormatInt(from, 10),
		strconv.FormatInt(to, 10),
		"0",
		"0123456789abcdef",
	}, "\x1f")
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// 探针本身必须是一个真正的采集侧 ID。
//
// 单列一条测试而不是塞进用例里：探针失效的症状是"用例测的不是它以为在测的
// 那条路"，而那种失败必须自己有名字，否则它只会表现为另一条用例莫名其妙地
// 变绿。
func TestCollectedFlowIDProbeIsAGenuineCollectedID(t *testing.T) {
	id := collectedFlowID(fixtureBackedID)
	got, ok := collectstore.ClusterOfFlowID(id)
	if !ok {
		t.Fatalf("collectedFlowID produced %q, which collectstore does not recognise; "+
			"every case using it would silently exercise the fixture fall-through instead", id)
	}
	if got != fixtureBackedID {
		t.Errorf("collectedFlowID names %q, want %q", got, fixtureBackedID)
	}
}

// toLeakCases 把 dispatchCase 转成 assertEveryReaderMethodCovered 要的形状。
//
// 复用 reader_test.go 里那个对账函数而不是再写一个：两份对账会各自漂移，
// 而它们要守的是同一条性质 —— store.Reader 新增一个方法时，没有为它写
// 用例必须变红。
func toLeakCases(cases []dispatchCase) []leakCase {
	out := make([]leakCase, 0, len(cases))
	for _, c := range cases {
		out = append(out, leakCase{method: c.method, name: c.name})
	}
	return out
}
