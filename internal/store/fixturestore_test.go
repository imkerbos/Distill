package store_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

// fixtureClusters 镜像 internal/fixture/asset.go 里两个集群的注册信息。
// 值必须与那里的 Registry/APIServers 逐字一致，否则既有用例会在
// "fixture 的兜底值"与"注册信息"之间读到不同的网段。
func fixtureClusters() []registry.Cluster {
	return []registry.Cluster{
		{
			ID: "prod-asia-1", DisplayName: "Asia Prod",
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
			State:              registry.StateReady,
			APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
		{
			ID: "prod-eu-1", DisplayName: "EU Prod",
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.132.0.0/20",
			State:              registry.StateReady,
			APIServers:         []registry.APIServer{{Host: "10.13.0.2", CIDR: "10.13.0.0/28", Port: 443}},
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
	}
}

func newReader() *store.FixtureReader {
	return store.NewFixtureReader(fixture.Load(), fixtureClusters())
}

// allTime 是覆盖整个 fixture 数据集的时间窗。Flows 的时间窗是必填的
// （见 store.FlowFilter.Window），只关心其他筛选条件的用例用它把时间维打开。
func allTime() store.TimeWindow {
	return newReader().DataWindow()
}

func TestClusters(t *testing.T) {
	got, err := newReader().Clusters(context.Background())
	if err != nil {
		t.Fatalf("Clusters: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2", len(got))
	}
	for _, c := range got {
		if c.PodCount == 0 {
			t.Errorf("cluster %s reports no pods", c.ID)
		}
	}
}

func TestTopologyHasNodesAndEdges(t *testing.T) {
	topo, err := newReader().Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if len(topo.Nodes) == 0 {
		t.Fatal("topology has no nodes")
	}
	if len(topo.Edges) == 0 {
		t.Fatal("topology has no edges")
	}
	for _, e := range topo.Edges {
		if e.Verdict == "" || e.Confidence == "" {
			t.Errorf("edge %s->%s missing verdict or confidence", e.Source, e.Target)
		}
	}
}

func TestTopologyUnknownClusterIsNotFound(t *testing.T) {
	_, err := newReader().Topology(context.Background(), "no-such-cluster", store.LevelNamespace)
	if err == nil {
		t.Fatal("Topology() = nil error for an unknown cluster")
	}
}

// mesh namespace 的节点必须被标出来，界面靠它显示 DEGRADED。
func TestTopologyMarksMeshNamespace(t *testing.T) {
	topo, _ := newReader().Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	for _, n := range topo.Nodes {
		if n.Namespace == "checkout" && n.InMesh {
			return
		}
	}
	t.Error("checkout namespace is not marked InMesh")
}

// 每条边的两端都必须出现在节点集合里。悬空引用会让前端图要么报错，
// 要么静默丢掉这条边 —— 而跨集群敞口正是最不该被静默丢掉的东西。
func TestTopologyEdgesOnlyReferenceKnownNodes(t *testing.T) {
	topo, err := newReader().Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}

	known := map[string]bool{}
	for _, n := range topo.Nodes {
		known[n.ID] = true
	}
	for _, e := range topo.Edges {
		if !known[e.Source] {
			t.Errorf("edge source %q has no node", e.Source)
		}
		if !known[e.Target] {
			t.Errorf("edge target %q has no node", e.Target)
		}
	}
}

func TestTopologyMarksForeignNamespace(t *testing.T) {
	topo, _ := newReader().Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	for _, n := range topo.Nodes {
		if n.Cluster != "prod-asia-1" && !n.Foreign {
			t.Errorf("node %q is from another cluster but is not marked Foreign", n.ID)
		}
	}
}

func TestFlowsRespectsLimit(t *testing.T) {
	got, err := newReader().Flows(context.Background(), store.FlowFilter{Window: allTime(), Limit: 10})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if len(got.Items) != 10 {
		t.Errorf("got %d flows, want 10", len(got.Items))
	}
}

// 截断必须能被调用方看见：Total 报的是筛选后、截断前的条数。
// 只回一个被切短的切片，界面会把"只给你看了 10 条"显示成"一共就这些"。
func TestFlowsReportsTotalBeforeTruncation(t *testing.T) {
	r := newReader()
	all, err := r.Flows(context.Background(), store.FlowFilter{Window: allTime(), Limit: 1000})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	page, err := r.Flows(context.Background(), store.FlowFilter{Window: allTime(), Limit: 10})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if page.Total != all.Total {
		t.Errorf("truncated page reports Total = %d, want %d", page.Total, all.Total)
	}
	if page.Total <= len(page.Items) {
		t.Errorf("Total = %d but %d items returned; truncation is invisible", page.Total, len(page.Items))
	}
	if page.Limit != 10 {
		t.Errorf("Limit = %d, want 10", page.Limit)
	}
}

// 默认查询必须包含"不知道"的那部分。数据集刻意制造了 UNKNOWN 与跨集群
// 流量，而它们恰好排在 ID 序列的后段 —— 一个截断在前段的默认视图会让
// demo 开屏全绿，正是这个平台最不该给人的印象。
func TestDefaultFlowsIncludeTheGaps(t *testing.T) {
	page, err := newReader().Flows(context.Background(), store.FlowFilter{Window: allTime()})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}

	var unknown, cross int
	for _, f := range page.Items {
		if f.Verdict == "UNKNOWN" {
			unknown++
		}
		if f.CrossCluster {
			cross++
		}
	}
	if unknown == 0 {
		t.Error("the default flow list contains no UNKNOWN rows; the demo opens all-green")
	}
	if cross == 0 {
		t.Error("the default flow list contains no cross-cluster rows; the known enforcement gap is invisible")
	}
}

// 不存在的集群在 Flows 上也必须报错。返回空列表会让同一个笔误在拓扑页
// 得到 20002、在流量页得到"没有流量"，两块屏对同一件事给出两种解释。
func TestFlowsUnknownClusterIsNotFound(t *testing.T) {
	_, err := newReader().Flows(context.Background(), store.FlowFilter{Window: allTime(), Cluster: "no-such"})
	if !errors.Is(err, store.ErrClusterNotFound) {
		t.Errorf("Flows(cluster=no-such) error = %v, want ErrClusterNotFound", err)
	}
}

func TestFlowsFilterByVerdict(t *testing.T) {
	got, err := newReader().Flows(context.Background(), store.FlowFilter{Window: allTime(), Verdict: "DENY", Limit: 500})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if len(got.Items) == 0 {
		t.Fatal("no DENY flows; the dataset should produce some")
	}
	for _, f := range got.Items {
		if f.Verdict != "DENY" {
			t.Fatalf("filter returned a %s flow", f.Verdict)
		}
	}
}

func TestFlowsFilterByConfidence(t *testing.T) {
	got, err := newReader().Flows(context.Background(), store.FlowFilter{Window: allTime(), Confidence: "DEGRADED", Limit: 500})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if len(got.Items) == 0 {
		t.Fatal("no DEGRADED flows; the mesh namespace should produce some")
	}
}

// NetworkPolicy 管不到 hostNetwork Pod。这类流量拿到的 ALLOW 与"策略确实
// 放行了它"必须能区分开，否则界面会把一个封不住的敞口画成一条绿边。
func TestFlowToHostNetworkPodIsMarkedUnmanaged(t *testing.T) {
	page, err := newReader().Flows(context.Background(), store.FlowFilter{Window: allTime(), Limit: 1000})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	for _, f := range page.Items {
		if strings.Contains(f.DestLabel, "kube-proxy") {
			if !f.Unmanaged {
				t.Errorf("flow %s to a hostNetwork pod is not marked unmanaged: %+v", f.ID, f)
			}
			return
		}
	}
	t.Fatal("no flow to the hostNetwork pod; the dataset should contain some")
}

func TestFlowReturnsFullDecision(t *testing.T) {
	r := newReader()
	list, _ := r.Flows(context.Background(), store.FlowFilter{Window: allTime(), Limit: 1})
	if len(list.Items) == 0 {
		t.Fatal("no flows")
	}

	dec, ok, err := r.Flow(context.Background(), list.Items[0].ID)
	if err != nil {
		t.Fatalf("Flow: %v", err)
	}
	if !ok {
		t.Fatalf("flow %s not found", list.Items[0].ID)
	}
	if dec.Reason.Direction == "" && dec.Verdict != "ALLOW" {
		t.Error("a non-ALLOW decision must record which direction decided it")
	}
}

func TestFlowUnknownIDIsNotFound(t *testing.T) {
	_, ok, err := newReader().Flow(context.Background(), "flow-999999")
	if err != nil {
		t.Fatalf("Flow: %v", err)
	}
	if ok {
		t.Error("unknown flow ID resolved")
	}
}

// 数据质量必须展示 UNKNOWN 的构成，而不是一个孤零零的比例。
func TestQualityReportsUnknownComposition(t *testing.T) {
	q, err := newReader().Quality(context.Background(), "prod-asia-1")
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.TotalFlows == 0 {
		t.Fatal("quality reports zero flows")
	}
	if len(q.UnknownComposition) == 0 {
		t.Error("UnknownComposition is empty; the dataset deliberately contains unknowns")
	}
	if q.UnmanagedPodCount == 0 {
		t.Error("UnmanagedPodCount is zero; the dataset contains hostNetwork pods")
	}
}

// 明细各项之和必须等于 UNKNOWN 总数：一条没写原因的 UNKNOWN 若被丢掉，
// 运维照着这份账去排查时，永远找不齐那几条。
func TestQualityUnknownCompositionSumsToUnknownCount(t *testing.T) {
	q, err := newReader().Quality(context.Background(), "prod-asia-1")
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	sum := 0
	for _, n := range q.UnknownComposition {
		sum += n
	}
	if sum != q.UnknownCount {
		t.Errorf("composition sums to %d but UnknownCount = %d; the parts do not add up to the whole",
			sum, q.UnknownCount)
	}
	if q.UnknownCount == 0 {
		t.Error("UnknownCount is zero; the dataset deliberately contains unknowns")
	}
}

// 源端集群同样要看到自己发出的跨集群流量。eu 的 partner 正在往 asia 发流量，
// 而 eu 的界面报 crossClusterCount: 0 —— 那不是"没有"，是一句确信的假话。
func TestQualityCountsCrossClusterOnTheSourceCluster(t *testing.T) {
	q, err := newReader().Quality(context.Background(), "prod-eu-1")
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if q.CrossClusterCount == 0 {
		t.Error("prod-eu-1 reports no cross-cluster flows, but its partner namespace sends them")
	}
}

// 同一条敞口在拓扑上也必须画得出来，否则 eu 的图里看不到任何出集群的边。
func TestTopologyShowsCrossClusterEdgeOnTheSourceCluster(t *testing.T) {
	topo, err := newReader().Topology(context.Background(), "prod-eu-1", store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	for _, e := range topo.Edges {
		if e.CrossCluster {
			return
		}
	}
	t.Error("prod-eu-1 topology has no cross-cluster edge; the enforcement gap is invisible there")
}

// 画不出的 flow 必须被计数报出来。默默 continue 会让拓扑页与数据质量页
// 对同一个集群给出两个不同的总数，而差额里装着刻意制造的那几类"不知道"。
func TestTopologyReportsUnplaceableFlows(t *testing.T) {
	topo, err := newReader().Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if topo.UnplaceableFlowCount == 0 {
		t.Error("UnplaceableFlowCount is zero, but the dataset contains flows with no resolvable endpoint")
	}

	var placed int
	for _, e := range topo.Edges {
		placed += e.FlowCount
	}
	q, err := newReader().Quality(context.Background(), "prod-asia-1")
	if err != nil {
		t.Fatalf("Quality: %v", err)
	}
	if placed+topo.UnplaceableFlowCount != q.TotalFlows {
		t.Errorf("topology accounts for %d+%d flows but quality reports %d; the two screens disagree",
			placed, topo.UnplaceableFlowCount, q.TotalFlows)
	}
}

// 一条通往 hostNetwork Pod 的边不能显示成普通放行。
func TestTopologyMarksUnmanagedEdge(t *testing.T) {
	topo, err := newReader().Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	for _, e := range topo.Edges {
		if strings.HasSuffix(e.Target, "/kube-system") && e.Unmanaged {
			return
		}
	}
	t.Error("no edge into kube-system is marked unmanaged; NetworkPolicy cannot govern that traffic")
}

func TestQualityRatesAreFractions(t *testing.T) {
	q, _ := newReader().Quality(context.Background(), "prod-asia-1")
	for name, v := range map[string]float64{
		"TrustedRate": q.TrustedRate, "UnknownRate": q.UnknownRate,
		"DegradedRate": q.DegradedRate, "PolicyCoverage": q.PolicyCoverage,
	} {
		if v < 0 || v > 1 {
			t.Errorf("%s = %v, want a fraction in [0,1]", name, v)
		}
		if math.IsNaN(v) {
			t.Errorf("%s is NaN; a zero denominator must yield 0, not NaN", name)
		}
	}
}

// 筛选取值是封闭枚举：大小写不同、拼错的取值都必须被拒，
// 否则界面会把一次输入错误显示成"没有这类流量"。
func TestFilterValueEnumsAreClosed(t *testing.T) {
	for value, want := range map[string]bool{
		"ALLOW": true, "DENY": true, "UNKNOWN": true,
		"allow": false, "ALOW": false, "": false, "TRUSTED": false,
	} {
		if got := store.ValidVerdict(value); got != want {
			t.Errorf("ValidVerdict(%q) = %v, want %v", value, got, want)
		}
	}
	for value, want := range map[string]bool{
		"TRUSTED": true, "DEGRADED": true,
		"trusted": false, "TRUSTD": false, "": false, "ALLOW": false,
	} {
		if got := store.ValidConfidence(value); got != want {
			t.Errorf("ValidConfidence(%q) = %v, want %v", value, got, want)
		}
	}
}

// 从质量页点进流量列表必须看到同一批流量。两个数字对不上时，
// total 字段会把一个不完整的集合断言成完整的 —— 而差额恰好是
// 跨集群敞口，正是这个界面存在的理由。
func TestFlowsClusterFilterMatchesQualityTotal(t *testing.T) {
	r := newReader()
	ctx := context.Background()

	for _, cluster := range []string{"prod-asia-1", "prod-eu-1"} {
		q, err := r.Quality(ctx, cluster)
		if err != nil {
			t.Fatalf("Quality(%s): %v", cluster, err)
		}
		page, err := r.Flows(ctx, store.FlowFilter{Window: allTime(), Cluster: cluster, Limit: 1000})
		if err != nil {
			t.Fatalf("Flows(%s): %v", cluster, err)
		}
		if page.Total != q.TotalFlows {
			t.Errorf("%s: flow list total %d != quality totalFlows %d — the drill-down disagrees with the screen it came from",
				cluster, page.Total, q.TotalFlows)
		}
	}
}

// 跨集群流量必须出现在它涉及的两个集群的列表里，否则从任一侧看
// 这个敞口都是隐形的。
func TestCrossClusterFlowsAppearForBothClusters(t *testing.T) {
	r := newReader()
	ctx := context.Background()

	for _, cluster := range []string{"prod-asia-1", "prod-eu-1"} {
		page, err := r.Flows(ctx, store.FlowFilter{Window: allTime(), Cluster: cluster, Limit: 1000})
		if err != nil {
			t.Fatalf("Flows(%s): %v", cluster, err)
		}
		cross := 0
		for _, f := range page.Items {
			if f.CrossCluster {
				cross++
			}
		}
		if cross == 0 {
			t.Errorf("%s: no cross-cluster rows in its own flow list", cluster)
		}
	}
}

// 集群元数据来自注册信息而非 fixture 硬编码：Baseline 的推导依据
// 因此可以在后台修改，而不必改代码重新编译。
func TestReaderUsesRegisteredCIDRs(t *testing.T) {
	clusters := []registry.Cluster{{
		ID: "prod-asia-1", DisplayName: "Asia", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.200.0.0/20", State: registry.StateReady,
		APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
		HealthCheckSources: []string{"35.191.0.0/16"},
	}}
	r := store.NewFixtureReader(fixture.Load(), clusters)

	pv, err := r.PolicyPreview(context.Background(), "prod-asia-1", "", r.DataWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	found := false
	for _, p := range pv.Candidates {
		for _, rule := range p.Rules {
			for _, peer := range rule.Peers {
				if peer == "10.200.0.0/20" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no rule uses the registered node CIDR 10.200.0.0/20; " +
			"the reader is still reading the hardcoded fixture value")
	}
}

// 未注册的集群即便 fixture 里有数据也不可见：注册表是集群是否
// 被平台管理的唯一判据。
func TestReaderHidesUnregisteredClusters(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load(), nil)
	list, err := r.Clusters(context.Background())
	if err != nil {
		t.Fatalf("Clusters() error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("Clusters() = %d entries with an empty registry, want 0", len(list))
	}
}
