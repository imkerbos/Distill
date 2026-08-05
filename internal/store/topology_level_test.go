package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/store"
)

// workload 粒度必须比 namespace 粒度更细。相等说明聚合没生效，
// 而界面会照常显示"workload 视图"。
func TestWorkloadTopologyIsFinerThanNamespace(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load())
	ns, err := r.Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	wl, err := r.Topology(context.Background(), "prod-asia-1", store.LevelWorkload)
	if err != nil {
		t.Fatalf("workload: %v", err)
	}
	if len(wl.Nodes) <= len(ns.Nodes) {
		t.Errorf("workload 节点 %d 个，namespace 节点 %d 个 —— 未见细化",
			len(wl.Nodes), len(ns.Nodes))
	}
	if wl.Level != string(store.LevelWorkload) {
		t.Errorf("Level 未回显: %q", wl.Level)
	}
}

// 每条边的两端都必须在节点集合里。悬空引用会让渲染器要么报错、
// 要么悄悄丢掉那条边 —— 而最容易被丢掉的正是跨集群边。
func TestWorkloadTopologyHasNoDanglingEdges(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load())
	for _, cluster := range []string{"prod-asia-1", "prod-eu-1"} {
		topo, err := r.Topology(context.Background(), cluster, store.LevelWorkload)
		if err != nil {
			t.Fatalf("%s: %v", cluster, err)
		}
		known := map[string]bool{}
		for _, n := range topo.Nodes {
			known[n.ID] = true
		}
		for _, e := range topo.Edges {
			if !known[e.Source] {
				t.Errorf("%s: 边的源 %q 不在节点集合里", cluster, e.Source)
			}
			if !known[e.Target] {
				t.Errorf("%s: 边的目的 %q 不在节点集合里", cluster, e.Target)
			}
		}
	}
}

// 没有 app 标签的 Pod 必须以 UNKNOWN 出现，而不是被藏起来。
// 藏起来会让 workload 拓扑看上去比实际完整。
func TestWorkloadTopologySurfacesUnlabelledPods(t *testing.T) {
	f := fixture.Load()
	var unlabelled int
	for _, c := range f.Clusters {
		for _, p := range c.Pods {
			if p.Labels["app"] == "" {
				unlabelled++
			}
		}
	}
	if unlabelled == 0 {
		t.Skip("数据集里所有 Pod 都有 app 标签，本用例无从验证")
	}

	r := store.NewFixtureReader(f)
	topo, err := r.Topology(context.Background(), "prod-asia-1", store.LevelWorkload)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	var found bool
	for _, n := range topo.Nodes {
		if strings.HasSuffix(n.ID, "/UNKNOWN") {
			found = true
		}
	}
	if !found {
		t.Error("存在无 app 标签的 Pod，但 workload 拓扑里没有 UNKNOWN 节点")
	}
}

// 边必须说明是哪一侧做的判定，否则"该改哪边的策略"无从回答。
func TestEdgesReportDecidingDirection(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load())
	topo, err := r.Topology(context.Background(), "prod-asia-1", store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	seen := map[string]int{}
	for _, e := range topo.Edges {
		seen[e.DecidedBy]++
	}
	if seen["INGRESS"]+seen["EGRESS"]+seen["MIXED"] == 0 {
		t.Fatalf("没有任何边给出判定方向: %v", seen)
	}
	t.Logf("判定方向分布: %v", seen)
}

func TestTopologyRejectsUnknownLevel(t *testing.T) {
	if store.ValidTopologyLevel("pod") {
		t.Error("pod 不应是合法粒度")
	}
	if !store.ValidTopologyLevel("workload") || !store.ValidTopologyLevel("namespace") {
		t.Error("合法粒度被拒")
	}
}
