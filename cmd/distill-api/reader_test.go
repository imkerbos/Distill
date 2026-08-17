package main

import (
	"context"
	"errors"
	"testing"

	"github.com/imkerbos/Distill/internal/collectstore"
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

// COLLECTED 集群的读取路径上不得有通往 fixture 的分支。
//
// 两半各自独立，两半都要成立：
//
//  1. 装配层不选它 —— readerFor 对 COLLECTED 只会返回采集侧的 Reader。
//  2. **即便有人把上面那个开关拨反**，装出来的 fixture Reader 也答不出这个
//     集群：它的数据源被 fixtureOnlySource 收窄过，一个登记为 COLLECTED 的
//     集群在里面根本不存在，于是六个读方法一律 ErrClusterNotFound，而不是
//     一份数字合理、流程走得通的合成报告。
//
// 第二半才是这条性质真正的落脚点。只有第一半的话，这条纪律就只是一个后人
// 可以拨反的条件 —— 而拨反它不会有任何症状。
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
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Topology", func() error {
			_, err := fr.Topology(ctx, fixtureBackedID, store.LevelNamespace)
			return err
		}},
		{"Flows", func() error {
			_, err := fr.Flows(ctx, store.FlowFilter{Cluster: fixtureBackedID, Window: window})
			return err
		}},
		{"Quality", func() error {
			_, err := fr.Quality(ctx, fixtureBackedID)
			return err
		}},
		{"Security", func() error {
			_, err := fr.Security(ctx, fixtureBackedID, window)
			return err
		}},
		{"PolicyPreview", func() error {
			_, err := fr.PolicyPreview(ctx, fixtureBackedID, "payment", window)
			return err
		}},
	} {
		if err := tc.call(); !errors.Is(err, store.ErrClusterNotFound) {
			t.Errorf("%s(%s declared COLLECTED) error = %v, want ErrClusterNotFound: "+
				"the fixture reader must not hold any data for a collected cluster",
				tc.name, fixtureBackedID, err)
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
