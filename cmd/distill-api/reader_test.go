package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

// stubClusterSource 是一份写死的注册表，供装配层的测试使用。
//
// 不连库：这一层要验证的是「按登记的来源选 Reader」，而那件事与数据从
// 哪张表读出来无关。持久化那一半由 internal/mysqlregistry 的集成测试守。
type stubClusterSource struct {
	clusters []registry.Cluster
}

func (s stubClusterSource) Clusters(context.Context) ([]registry.Cluster, error) {
	return s.clusters, nil
}

func (s stubClusterSource) Cluster(_ context.Context, id string) (registry.Cluster, bool, error) {
	for _, c := range s.clusters {
		if c.ID == id {
			return c, true, nil
		}
	}
	return registry.Cluster{}, false, nil
}

func (s stubClusterSource) RuleOverrides(context.Context, string) ([]registry.RuleOverride, error) {
	return nil, nil
}

// fixtureBackedID 是 internal/fixture 里确实有整套数据的集群。
//
// 测试里刻意用它来登记 COLLECTED：来源若是从「有没有数据」推断出来的，
// 这个 ID 一定会被推断成 FIXTURE，而这正是要挡住的那件事。
const fixtureBackedID = "prod-asia-1"

// fixtureBackedPeer 是 fixture 里的另一个集群，用来做对照组。
const fixtureBackedPeer = "prod-eu-1"

// emptyID 是 fixture 里**没有**任何数据的集群 ID。
const emptyID = "prod-new-1"

// 来源是显式登记的，不由「有没有数据」推断 —— 推断意味着一次采集故障
// 会让一个真集群悄悄变回演示集群，而页面上完全看不出来（design doc §2）。
//
// 两个方向一起断言，因为推断的两个方向各自都会出事：
//
//   - 有数据 ⇒ FIXTURE：一个登记为 COLLECTED、ID 恰好又在 fixture 里的集群
//     会拿到那份合成数据。这是本轮最严重的那个后果的完整形态。
//   - 没数据 ⇒ COLLECTED：一个登记为 FIXTURE 的集群会因为 fixture 里暂时没有
//     它的数据而被当成真集群，于是演示环境在没人改代码的情况下自己坏掉。
//
// 只断言其中一个方向是不够的：一句无条件的 `return fixtureReader` 也能让
// 前一半通过。
func TestSourceIsDeclaredNotInferred(t *testing.T) {
	src := stubClusterSource{}
	collected := collectstore.New(nil, src)

	// 有数据，却登记为 COLLECTED：不得因为 fixture 里找得到它就给它 fixture。
	withData := registry.Cluster{ID: fixtureBackedID, DataSource: registry.DataSourceCollected}
	r, err := readerFor(withData, src, collected)
	if err != nil {
		t.Fatalf("readerFor(%s declared COLLECTED) error = %v, want the collected reader",
			fixtureBackedID, err)
	}
	if r != store.Reader(collected) {
		t.Errorf("readerFor(%s declared COLLECTED) = %T, want *collectstore.Reader: "+
			"the source is declared, not inferred from the fixture happening to hold that ID",
			fixtureBackedID, r)
	}

	// 没数据，却登记为 FIXTURE：仍然装 fixture 的 Reader。
	withoutData := registry.Cluster{ID: emptyID, DataSource: registry.DataSourceFixture}
	r, err = readerFor(withoutData, src, collected)
	if err != nil {
		t.Fatalf("readerFor(%s declared FIXTURE) error = %v, want none: "+
			"an empty fixture is not a reason to treat a declared demo cluster as collected",
			emptyID, err)
	}
	if _, ok := r.(*store.FixtureReader); !ok {
		t.Errorf("readerFor(%s declared FIXTURE) = %T, want *store.FixtureReader", emptyID, r)
	}
}

// 装配方没接上采集侧读取面时，COLLECTED 集群仍然不得拿到 fixture。
//
// 这是同一条纪律在"装配漏了一半"这种情形下的落点：正确答案是「没有数据」，
// 而不是"既然没有采集侧 Reader，就先用合成数据顶着"（规范 §49）。
func TestACollectedClusterWithoutACollectedReaderIsRefused(t *testing.T) {
	c := registry.Cluster{ID: fixtureBackedID, DataSource: registry.DataSourceCollected}
	r, err := readerFor(c, stubClusterSource{}, nil)
	if !errors.Is(err, ErrNoCollectedReader) {
		t.Errorf("readerFor(COLLECTED, no collected reader) error = %v, want ErrNoCollectedReader", err)
	}
	if r != nil {
		t.Errorf("readerFor(COLLECTED, no collected reader) = %T, want no reader at all", r)
	}
}

// 没登记来源的集群一律拒绝装配。
//
// 失败方向朝关（规范 §49）：一个来源为空的集群多半是新增了写路径却没同步
// 登记，而此时任何一种兜底都是在替一个没人做过的决定作答。
func TestAnUnregisteredDataSourceGetsNoReader(t *testing.T) {
	src := stubClusterSource{}
	for _, ds := range []registry.DataSource{"", "FIXTURES", "collected"} {
		c := registry.Cluster{ID: "prod-x", DataSource: ds}
		r, err := readerFor(c, src, collectstore.New(nil, src))
		if err == nil {
			t.Errorf("readerFor(data source %q) error = nil, want a refusal", ds)
		}
		if r != nil {
			t.Errorf("readerFor(data source %q) = %T, want no reader", ds, r)
		}
	}
}

// leakCase 是「一个 COLLECTED 集群在 fixture Reader 上不可见」这条性质在
// 某一个读方法上的落点。
//
// method 是 store.Reader 上的方法名，assertEveryReaderMethodCovered 拿它
// 对账；name 描述的是这一格具体在问什么（同一个方法可以有多格，例如
// Flows 的点名与不点名两条路是两道不同的门）。
//
// leak 返回一句「泄漏了什么」，空串表示这个读方法确实拒绝了。用字符串而
// 不是 error：Flow 的拒绝形态是 ok=false、err=nil（见 store.FixtureReader.Flow），
// 不点名集群的 Flows 的拒绝形态则是「返回了列表，但列表里没有那个集群的
// 记录」—— 三种形态断言不到同一个 error 上。
type leakCase struct {
	method string
	name   string
	leak   func() string
}

// wantClusterNotFound 把「按集群解析的读方法必须报 ErrClusterNotFound」
// 翻译成 leakCase 要的那种描述。
func wantClusterNotFound(err error) string {
	if errors.Is(err, store.ErrClusterNotFound) {
		return ""
	}
	return fmt.Sprintf("error = %v, want ErrClusterNotFound", err)
}

// assertEveryReaderMethodCovered 要求 store.Reader 的每个方法在表里都有
// 至少一格。
//
// 这是这条性质唯一的防漏机制：I1 那道缝之所以活过四轮任务评审，就是因为
// 上一版的表里恰好少了 Flow 一格，而少一格不会让任何东西变红。按接口反射
// 取方法名之后，新增第七、第八个读方法却忘记在这里补一格，表现是这条测试
// 直接失败并点名那个方法，而不是默默地放它过去。
//
// 反向也要对账：表里出现一个接口上没有的方法名，多半是重构改了方法名而
// 这一格被留在原地 —— 那一格从此测的是一个不存在的入口。
func assertEveryReaderMethodCovered(t *testing.T, cases []leakCase) {
	t.Helper()
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.method] = true
	}
	iface := reflect.TypeOf((*store.Reader)(nil)).Elem()
	declared := map[string]bool{}
	var missing []string
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		declared[name] = true
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("store.Reader methods with no COLLECTED-cluster case: %v — "+
			"every read method needs one; a method that is merely absent from this "+
			"table leaks silently (design doc §2)", missing)
	}
	for _, c := range cases {
		if !declared[c.method] {
			t.Errorf("case %q names method %q, which store.Reader does not declare",
				c.name, c.method)
		}
	}
}

// crossClusterFlowID 取一条一端在 a、另一端在 b 的 fixture 流量 ID。
func crossClusterFlowID(t *testing.T, a, b string) string {
	t.Helper()
	for _, f := range fixture.Load().Flows {
		src, dst := f.Flow.Source.ClusterID, f.Flow.Dest.ClusterID
		if (src == a && dst == b) || (src == b && dst == a) {
			return f.ID
		}
	}
	t.Fatalf("fixture has no flow between %s and %s; the probe would prove nothing", a, b)
	return ""
}

// flowIDsInvolving 列出任一端属于该集群的全部 fixture 流量 ID。
func flowIDsInvolving(clusterID string) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, f := range fixture.Load().Flows {
		if f.Flow.Source.ClusterID == clusterID || f.Flow.Dest.ClusterID == clusterID {
			ids[f.ID] = struct{}{}
		}
	}
	return ids
}

// COLLECTED 集群的读取路径上不得有通往 fixture 的分支。
//
// 两半各自独立，两半都要成立：
//
//  1. 装配层不选它 —— readerFor 对 COLLECTED 只会返回采集侧的 Reader。
//  2. **即便有人把上面那个开关拨反**，装出来的 fixture Reader 也交不出任何
//     关于这个集群的东西：它的数据源被 fixtureOnlySource 收窄过，一个登记为
//     COLLECTED 的集群在里面根本不存在，于是按集群解析的读方法一律
//     ErrClusterNotFound，而按 ID 下钻与不点名集群的全量列表则连那条记录
//     都取不出来 —— 而不是一份数字合理、流程走得通的合成报告。
//
// 第二半才是这条性质真正的落脚点。只有第一半的话，这条纪律就只是一个后人
// 可以拨反的条件 —— 而拨反它不会有任何症状。
//
// 表按 store.Reader 的方法名对账（assertEveryReaderMethodCovered）：上一版
// 逐个方法手写、恰好漏掉 Flow，而少一格不会让任何东西变红，这条缝因此活过
// 了四轮任务评审。
func TestACollectedClusterNeverGetsTheFixtureReader(t *testing.T) {
	// 对照组用同一份注册表：一个 FIXTURE 集群必须照常被服务，否则「什么都
	// 答不出来」也能让这个测试通过，而那是把演示环境一起关掉。
	src := stubClusterSource{clusters: []registry.Cluster{
		{ID: fixtureBackedID, DataSource: registry.DataSourceCollected},
		{ID: fixtureBackedPeer, DataSource: registry.DataSourceFixture},
	}}
	ctx := context.Background()

	collected := registry.Cluster{ID: fixtureBackedID, DataSource: registry.DataSourceCollected}
	r, err := readerFor(collected, src, collectstore.New(nil, src))
	if err != nil {
		t.Errorf("readerFor(COLLECTED) error = %v, want the collected reader", err)
	}
	if _, isFixture := r.(*store.FixtureReader); isFixture {
		t.Errorf("readerFor(COLLECTED) handed back the fixture reader")
	}

	// 第二半：直接拿 fixture Reader 去问那个 COLLECTED 集群。
	fr := newFixtureReader(src)
	window := fr.DataWindow()

	// 跨集群那条记录才是真正的探针：它的一端是 COLLECTED 的 asia、另一端是
	// 仍然登记为 FIXTURE 的 eu。「任一端已注册即放行」的判据会把它放出来 ——
	// 一条写着真集群名字的合成判定，而页面上没有任何症状（design doc §2）。
	// 挑一条两端都在 asia 的记录测不出这件事，那种记录任何一版判据都会挡住。
	crossID := crossClusterFlowID(t, fixtureBackedID, fixtureBackedPeer)
	leakedIDs := flowIDsInvolving(fixtureBackedID)

	refusals := []leakCase{
		{method: "Topology", name: "Topology(collected cluster)", leak: func() string {
			_, err := fr.Topology(ctx, fixtureBackedID, store.LevelNamespace)
			return wantClusterNotFound(err)
		}},
		{method: "Flows", name: "Flows(cluster=collected)", leak: func() string {
			_, err := fr.Flows(ctx, store.FlowFilter{Cluster: fixtureBackedID, Window: window})
			return wantClusterNotFound(err)
		}},
		{method: "Flows", name: "Flows(no cluster filter)", leak: func() string {
			// 不点名集群的全量列表：它不解析集群，只能靠取数那一步的门禁。
			page, err := fr.Flows(ctx, store.FlowFilter{Window: window, Limit: 10_000})
			if err != nil {
				return fmt.Sprintf("unexpected error %v", err)
			}
			for _, rec := range page.Items {
				if _, leaked := leakedIDs[rec.ID]; leaked {
					return fmt.Sprintf("record %s (%s -> %s) names the collected cluster",
						rec.ID, rec.SourceLabel, rec.DestLabel)
				}
			}
			return ""
		}},
		{method: "Flow", name: "Flow(cross-cluster record id)", leak: func() string {
			// 按 ID 下钻同样不解析集群。判给「找不到」而不是另一个错误：
			// 见 store.FixtureReader.Flow 的说明。
			d, ok, err := fr.Flow(ctx, crossID)
			if err != nil {
				return fmt.Sprintf("unexpected error %v", err)
			}
			if ok {
				return fmt.Sprintf("resolved %s: %s -> %s verdict=%s",
					crossID, d.SourceLabel, d.DestLabel, d.Verdict)
			}
			return ""
		}},
		{method: "Quality", name: "Quality(collected cluster)", leak: func() string {
			_, err := fr.Quality(ctx, fixtureBackedID)
			return wantClusterNotFound(err)
		}},
		{method: "Security", name: "Security(collected cluster)", leak: func() string {
			_, err := fr.Security(ctx, fixtureBackedID, window)
			return wantClusterNotFound(err)
		}},
		{method: "PolicyPreview", name: "PolicyPreview(collected cluster)", leak: func() string {
			_, err := fr.PolicyPreview(ctx, fixtureBackedID, "payment", window)
			return wantClusterNotFound(err)
		}},
		{method: "EnsureRuleExists", name: "EnsureRuleExists(collected cluster)", leak: func() string {
			err := fr.EnsureRuleExists(ctx, fixtureBackedID, "payment", "payment",
				"deadbeef", policygen.DecisionDisable, window)
			return wantClusterNotFound(err)
		}},
	}

	// 先钉住覆盖面，再跑用例：漏掉一个读方法的表现必须是这里红，而不是
	// 沉默地少测一格 —— I1 那条缝正是这样活过四轮任务评审的。
	assertEveryReaderMethodCovered(t, refusals)

	for _, tc := range refusals {
		if leak := tc.leak(); leak != "" {
			t.Errorf("%s [%s]: %s — the fixture reader must not surface anything "+
				"about a cluster declared COLLECTED (design doc §2)", tc.name, tc.method, leak)
		}
	}

	// 对照组：同一个 Reader 对登记为 FIXTURE 的集群照常供数。少了这一条，
	// 一个什么都答不出来的 Reader 也能让上面全部通过。
	topo, err := fr.Topology(ctx, fixtureBackedPeer, store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology(%s declared FIXTURE) error = %v, want the demo cluster still served",
			fixtureBackedPeer, err)
	}
	if len(topo.Nodes) == 0 {
		t.Errorf("Topology(%s declared FIXTURE) returned no nodes; the fixture must stay reachable "+
			"for declared demo clusters (design doc §6)", fixtureBackedPeer)
	}
}
