package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/store"
)

func newReader() *store.FixtureReader {
	return store.NewFixtureReader(fixture.Load())
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
	topo, err := newReader().Topology(context.Background(), "prod-asia-1")
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
	_, err := newReader().Topology(context.Background(), "no-such-cluster")
	if err == nil {
		t.Fatal("Topology() = nil error for an unknown cluster")
	}
}

// mesh namespace 的节点必须被标出来，界面靠它显示 DEGRADED。
func TestTopologyMarksMeshNamespace(t *testing.T) {
	topo, _ := newReader().Topology(context.Background(), "prod-asia-1")
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
	topo, err := newReader().Topology(context.Background(), "prod-asia-1")
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
	topo, _ := newReader().Topology(context.Background(), "prod-asia-1")
	for _, n := range topo.Nodes {
		if n.Cluster != "prod-asia-1" && !n.Foreign {
			t.Errorf("node %q is from another cluster but is not marked Foreign", n.ID)
		}
	}
}

func TestFlowsRespectsLimit(t *testing.T) {
	got, err := newReader().Flows(context.Background(), store.FlowFilter{Limit: 10})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if len(got) != 10 {
		t.Errorf("got %d flows, want 10", len(got))
	}
}

func TestFlowsFilterByVerdict(t *testing.T) {
	got, err := newReader().Flows(context.Background(), store.FlowFilter{Verdict: "DENY", Limit: 500})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no DENY flows; the dataset should produce some")
	}
	for _, f := range got {
		if f.Verdict != "DENY" {
			t.Fatalf("filter returned a %s flow", f.Verdict)
		}
	}
}

func TestFlowsFilterByConfidence(t *testing.T) {
	got, err := newReader().Flows(context.Background(), store.FlowFilter{Confidence: "DEGRADED", Limit: 500})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no DEGRADED flows; the mesh namespace should produce some")
	}
}

func TestFlowReturnsFullDecision(t *testing.T) {
	r := newReader()
	list, _ := r.Flows(context.Background(), store.FlowFilter{Limit: 1})
	if len(list) == 0 {
		t.Fatal("no flows")
	}

	dec, ok, err := r.Flow(context.Background(), list[0].ID)
	if err != nil {
		t.Fatalf("Flow: %v", err)
	}
	if !ok {
		t.Fatalf("flow %s not found", list[0].ID)
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
