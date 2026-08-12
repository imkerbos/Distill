package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

func reader() *store.FixtureReader {
	return store.NewFixtureReader(fixture.Load(), fixtureSource())
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

// 不存在的 namespace 必须报错，不能返回一份空的绿色报告。
//
// 空结果在界面上与"这个 namespace 一切正常"完全一样：候选策略 0 条、
// 缺失 Baseline 0 项、会被拦断的连接 0 条。一次拼写错误就此伪装成体检报告。
func TestPolicyPreviewRejectsUnknownNamespace(t *testing.T) {
	r := reader()
	pv, err := r.PolicyPreview(context.Background(), "prod-asia-1", "no-such-ns", fullWindow(r))
	if !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Fatalf("error = %v, want ErrNamespaceNotFound", err)
	}
	if len(pv.Candidates) != 0 || pv.Prediction.TotalEvaluated != 0 {
		t.Errorf("preview = %+v, want the zero value alongside the error", pv)
	}
}

// namespace 过滤只裁剪展示，预测必须逐项相同。
//
// 过滤掉候选策略却保留全量流量，会让目的地在其他 namespace 的流量
// 因为没有对应策略而落到 ALLOW：WOULD_OPEN 凭空出现、WOULD_BREAK
// 同时被低估，两个方向同时错，且都朝着让人放心的方向。
func TestPolicyPreviewPredictionIgnoresNamespaceFilter(t *testing.T) {
	r := reader()
	all, err := r.PolicyPreview(context.Background(), "prod-asia-1", "", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	one, err := r.PolicyPreview(context.Background(), "prod-asia-1", "payment", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview(payment) error = %v", err)
	}
	for _, k := range predict.AllChangeKinds() {
		if all.Prediction.Counts[k] != one.Prediction.Counts[k] {
			t.Errorf("%s: unfiltered = %d, namespace-filtered = %d; "+
				"the namespace filter must not change the prediction",
				k, all.Prediction.Counts[k], one.Prediction.Counts[k])
		}
	}
	if all.Prediction.TotalEvaluated != one.Prediction.TotalEvaluated {
		t.Errorf("TotalEvaluated %d vs %d", all.Prediction.TotalEvaluated, one.Prediction.TotalEvaluated)
	}
}

// 缺失清单的内容必须逐项断言，不能只断言"非空"。
//
// 只断言长度非零的话，一条选不中任何 Pod 的 METRICS_SCRAPE 规则会把
// 该 namespace 从缺失清单里去掉，而测试照样通过 —— 这份清单是
// 进入 Enforcing 的前置校验，报错报漏都是放行一次不该放行的上线。
func TestPolicyPreviewMissingBaselinesContent(t *testing.T) {
	// asia: 只有 gateway 有暴露面，只有 payment 与 gateway 是抓取目标。
	// eu: 没有任何 Gateway，因此每个 namespace 都缺 LB；只有 payment 被抓取。
	want := map[string]map[string][]baseline.Kind{
		"prod-asia-1": {
			"batch":       {baseline.KindLBHealth, baseline.KindMetrics},
			"checkout":    {baseline.KindLBHealth, baseline.KindMetrics},
			"kube-system": {baseline.KindLBHealth, baseline.KindMetrics},
			"legacy":      {baseline.KindLBHealth, baseline.KindMetrics},
			"payment":     {baseline.KindLBHealth},
		},
		"prod-eu-1": {
			"kube-system": {baseline.KindLBHealth, baseline.KindMetrics},
			"partner":     {baseline.KindLBHealth, baseline.KindMetrics},
			"payment":     {baseline.KindLBHealth},
		},
	}
	r := reader()
	for cluster, expect := range want {
		pv, err := r.PolicyPreview(context.Background(), cluster, "", fullWindow(r))
		if err != nil {
			t.Fatalf("%s: PolicyPreview() error = %v", cluster, err)
		}
		got := map[string][]baseline.Kind{}
		for _, m := range pv.MissingBaselines {
			got[m.Namespace] = m.Kinds
		}
		if !reflect.DeepEqual(got, expect) {
			t.Errorf("%s: MissingBaselines = %v, want %v", cluster, got, expect)
		}
	}
}

// 缺失清单同样只按 namespace 裁剪展示，内容不随筛选改变。
func TestPolicyPreviewMissingBaselinesFilteredForDisplayOnly(t *testing.T) {
	r := reader()
	all, _ := r.PolicyPreview(context.Background(), "prod-asia-1", "", fullWindow(r))
	one, err := r.PolicyPreview(context.Background(), "prod-asia-1", "batch", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview(batch) error = %v", err)
	}
	if len(one.MissingBaselines) != 1 || one.MissingBaselines[0].Namespace != "batch" {
		t.Fatalf("MissingBaselines = %+v, want only batch", one.MissingBaselines)
	}
	for _, m := range all.MissingBaselines {
		if m.Namespace != "batch" {
			continue
		}
		if !reflect.DeepEqual(m.Kinds, one.MissingBaselines[0].Kinds) {
			t.Errorf("batch kinds = %v under the filter, %v without it; "+
				"the filter must not change what a namespace is missing",
				one.MissingBaselines[0].Kinds, m.Kinds)
		}
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

// 无覆盖时两套预测必须逐项相同 —— 这是「覆盖层是恒等变换」的证据，
// 也是判断 Apply 有没有意外改动策略集的唯一可靠信号。
func TestOverriddenViewIsIdentityWithoutOverrides(t *testing.T) {
	r := reader()
	pv, err := r.PolicyPreview(context.Background(), "prod-asia-1", "", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	if !reflect.DeepEqual(pv.Prediction.Counts, pv.Overridden.Prediction.Counts) {
		t.Errorf("counts differ with no overrides: %v vs %v",
			pv.Prediction.Counts, pv.Overridden.Prediction.Counts)
	}
	if !reflect.DeepEqual(pv.Candidates, pv.Overridden.Candidates) {
		t.Error("candidates differ with no overrides")
	}
	if len(pv.StaleOverrides) != 0 {
		t.Errorf("StaleOverrides = %d, want 0", len(pv.StaleOverrides))
	}
}

// 启用一条默认禁用的规则后，两套预测必须真的不同 —— 否则覆盖层
// 接上了但没生效，而页面会显示两组一模一样的数字。
func TestOverriddenViewReflectsAnEnabledRule(t *testing.T) {
	r := reader()
	ctx := context.Background()
	base, err := r.PolicyPreview(ctx, "prod-asia-1", "", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}

	// 限定 INTERNET_EGRESS / CROSS_CLUSTER：同一条集群内连接会在两侧
	// 各生成一条独立规则（源端 egress、目的端 ingress，指纹不同），
	// NetworkPolicy 要求两侧都放行才算放行，只启用其中一条不会翻转判定
	// （已用 fixture 验证：批处理→payment:3306 那条 TRUSTED_DENY 规则单独
	// 启用后 Overridden 与默认预测逐项相同）。这两类证据的对端是公网 IP
	// 或另一个集群，不是本集群候选策略集里的主体，启用一条就足以让判定
	// 翻转，测的才是「覆盖生效」而不是巧合。
	var ns, wl, fp string
	for _, p := range base.Candidates {
		for _, rule := range p.Rules {
			isSingleSided := rule.Evidence == policygen.EvidenceInternetEgress ||
				rule.Evidence == policygen.EvidenceCrossCluster
			if rule.Origin == policygen.OriginLearned && !rule.Enabled && isSingleSided && fp == "" {
				ns, wl, fp = p.Namespace, p.Workload, rule.Fingerprint
			}
		}
	}
	if fp == "" {
		t.Fatal("fixture produced no disabled learned rule whose peer is outside this cluster's candidate set")
	}

	withOverride := readerWithOverrides([]registry.RuleOverride{{
		ClusterID: "prod-asia-1", Namespace: ns, Workload: wl, Fingerprint: fp,
		Decision: policygen.DecisionEnable, Reason: "r", DecidedBy: "admin",
		DecidedAt: time.Now().UTC(),
	}})
	pv, err := withOverride.PolicyPreview(ctx, "prod-asia-1", "", fullWindow(withOverride))
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}

	if reflect.DeepEqual(pv.Prediction.Counts, pv.Overridden.Prediction.Counts) {
		t.Errorf("counts identical after enabling a rule: %v", pv.Prediction.Counts)
	}
	// 默认那一套必须不受影响 —— 它回答的是「平台推荐了什么」。
	if !reflect.DeepEqual(base.Prediction.Counts, pv.Prediction.Counts) {
		t.Errorf("default prediction changed: %v vs %v",
			base.Prediction.Counts, pv.Prediction.Counts)
	}
}
