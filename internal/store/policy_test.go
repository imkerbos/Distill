package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/store"
)

func reader() *store.FixtureReader {
	return store.NewFixtureReader(fixture.Load())
}

func fullWindow(r *store.FixtureReader) store.TimeWindow { return r.DataWindow() }

func TestPolicyPreviewRequiresWindow(t *testing.T) {
	_, err := reader().PolicyPreview(context.Background(), "prod-asia-1", "", store.TimeWindow{})
	if err == nil {
		t.Fatal("PolicyPreview with an empty window succeeded, want ErrWindowRequired")
	}
}

func TestPolicyPreviewRejectsUnknownCluster(t *testing.T) {
	r := reader()
	_, err := r.PolicyPreview(context.Background(), "nope", "", fullWindow(r))
	if err == nil {
		t.Fatal("PolicyPreview on an unknown cluster succeeded, want ErrClusterNotFound")
	}
}

// 三块产物必须同时非空/可解释：只给候选策略而不给缺口，
// 界面就会把一份残缺的推荐显示成完整方案。
func TestPolicyPreviewReturnsAllFourBlocks(t *testing.T) {
	r := reader()
	pv, err := r.PolicyPreview(context.Background(), "prod-asia-1", "", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	if len(pv.Candidates) == 0 {
		t.Error("no candidate policies")
	}
	if len(pv.Ungeneratable) == 0 {
		t.Error("no ungeneratable items; the fixture contains an unlabelled pod and mesh traffic")
	}
	if pv.Prediction.TotalEvaluated == 0 {
		t.Error("prediction evaluated nothing")
	}
	if pv.Window != fullWindow(r) {
		t.Errorf("Window = %+v, want the effective window echoed back", pv.Window)
	}
	for _, k := range predict.AllChangeKinds() {
		if _, ok := pv.Prediction.Counts[k]; !ok {
			t.Errorf("prediction counts missing key %q; a zero must be shown, not omitted", k)
		}
	}
}

// namespace 过滤只影响候选策略的范围，不影响缺口清单。
func TestPolicyPreviewNamespaceFilterNarrowsCandidates(t *testing.T) {
	r := reader()
	all, _ := r.PolicyPreview(context.Background(), "prod-asia-1", "", fullWindow(r))
	one, err := r.PolicyPreview(context.Background(), "prod-asia-1", "payment", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview(payment) error = %v", err)
	}
	if len(one.Candidates) == 0 {
		t.Fatal("namespace filter produced no candidates")
	}
	if len(one.Candidates) >= len(all.Candidates) {
		t.Errorf("filtered candidates = %d, unfiltered = %d; filter had no effect",
			len(one.Candidates), len(all.Candidates))
	}
	for _, c := range one.Candidates {
		if c.Namespace != "payment" {
			t.Errorf("candidate in namespace %q leaked through the payment filter", c.Namespace)
		}
	}
}

// 时间窗必须真的裁剪观测集，否则接真存储后就是一次全表扫描。
func TestPolicyPreviewWindowNarrowsObservations(t *testing.T) {
	r := reader()
	full := fullWindow(r)
	narrow := store.TimeWindow{From: full.From, To: full.From.Add(30 * time.Second)}
	wide, _ := r.PolicyPreview(context.Background(), "prod-asia-1", "", full)
	small, err := r.PolicyPreview(context.Background(), "prod-asia-1", "", narrow)
	if err != nil {
		t.Fatalf("PolicyPreview(narrow) error = %v", err)
	}
	if small.Prediction.TotalEvaluated >= wide.Prediction.TotalEvaluated {
		t.Errorf("narrow window evaluated %d, full window %d; the window is not applied",
			small.Prediction.TotalEvaluated, wide.Prediction.TotalEvaluated)
	}
}
