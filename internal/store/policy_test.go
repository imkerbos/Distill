package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/flow"
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
	// **缺失只算"有推导对象、却推不出规则"的那些**，没有推导对象的走
	// 不适用（design doc 2026-08-18-baseline-applicability）。
	//
	// asia 因此一条不缺：gateway 有暴露面也是抓取目标，payment 是抓取目标，
	// 其余 namespace 既没有暴露面也没有 Pod 声明要被抓 —— 它们没有健康检查
	// 与抓取流量要放行，报缺就是误报。
	//
	// eu 的 partner 真缺 METRICS_SCRAPE：它有 Pod 声明要被抓，却没有抓取端
	// 登记到它。**LB_HEALTH_CHECK 不再缺**：partner 有一个 type=LoadBalancer
	// 的 Service，deriveLBHealth 现在直接从它推出健康检查放行（此前只认
	// Ingress，会把它误报成缺失、让写回 gate 永久卡死这个 namespace）。
	want := map[string]map[string][]baseline.Kind{
		"prod-asia-1": {},
		"prod-eu-1": {
			"partner": {baseline.KindMetrics},
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
		// 对照组：缺失为空必须是"检查过、没有缺口"，不是"两栏都返回空"。
		// asia 那五个 namespace 的两类必须出现在不适用栏里 —— 少了这条，
		// 一个把两份清单都返回空的实现照样过。
		if cluster != "prod-asia-1" {
			continue
		}
		na := map[string][]baseline.Kind{}
		for _, m := range pv.NotApplicableBaselines {
			na[m.Namespace] = m.Kinds
		}
		wantNA := map[string][]baseline.Kind{
			"batch":       {baseline.KindLBHealth, baseline.KindMetrics},
			"checkout":    {baseline.KindLBHealth, baseline.KindMetrics},
			"kube-system": {baseline.KindLBHealth, baseline.KindMetrics},
			"legacy":      {baseline.KindLBHealth, baseline.KindMetrics},
			"payment":     {baseline.KindLBHealth},
		}
		if !reflect.DeepEqual(na, wantNA) {
			t.Errorf("%s: NotApplicableBaselines = %v, want %v", cluster, na, wantNA)
		}
	}
}

// 缺失清单同样只按 namespace 裁剪展示，内容不随筛选改变。
func TestPolicyPreviewMissingBaselinesFilteredForDisplayOnly(t *testing.T) {
	r := reader()
	// 用 eu/partner：它是唯一真缺 Baseline 的 namespace（有暴露对象、有被抓
	// 声明，两者都推不出规则）。asia 现在一条不缺，拿它做筛选对照，一个
	// 恒返回空清单的实现照样能过。
	all, _ := r.PolicyPreview(context.Background(), "prod-eu-1", "", fullWindow(r))
	one, err := r.PolicyPreview(context.Background(), "prod-eu-1", "partner", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview(partner) error = %v", err)
	}
	if len(one.MissingBaselines) != 1 || one.MissingBaselines[0].Namespace != "partner" {
		t.Fatalf("MissingBaselines = %+v, want only partner", one.MissingBaselines)
	}
	for _, m := range all.MissingBaselines {
		if m.Namespace != "partner" {
			continue
		}
		if !reflect.DeepEqual(m.Kinds, one.MissingBaselines[0].Kinds) {
			t.Errorf("partner kinds = %v under the filter, %v without it; "+
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

// 两个 Reader 都要说得出"未评估的 Baseline 有哪些"，而合成数据集五类依据
// 齐备，因此它恒为空（design doc 2026-08-17 §11）。
//
// 断言非 nil 而不只是长度为 0：空清单要读作"我们检查了五类依据，都在"，
// 与"这个 Reader 根本没回答这个问题"必须能区分 —— 序列化出去前者是 []、
// 后者是 null，而前端拿到 null 时唯一能做的就是把这一栏藏掉。
//
// 对照组是 MissingBaselines 非空：fixture 确实缺 LB 与部分 namespace 的
// METRICS_SCRAPE（见 TestPolicyPreviewMissingBaselinesContent）。少了它，
// 一个把两份清单都返回空的实现照样能过 —— 而那正好抹掉这个字段要区分的
// 那件事：METRICS_SCRAPE 在这里是"依据齐备、推导不出"，不是"没看过"。
func TestPolicyPreviewFixtureAssessesEveryBaselineKind(t *testing.T) {
	r := reader()
	// eu 而不是 asia：对照组要求 MissingBaselines 非空，而 asia 现在一条不缺
	// （它那些 namespace 没有推导对象，走的是不适用）。eu 的 partner 是真缺
	// —— 有暴露对象、有被抓声明，两者都推不出规则，正是"依据齐备、推导不出"
	// 这一种，与"没看过"必须分得开。
	pv, err := r.PolicyPreview(context.Background(), "prod-eu-1", "", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	// 窗口完整度两个 Reader 都要填。fixture 这边它是一个常量 COMPLETE，
	// 因为合成数据集不是一次观测 —— 没有采样、没有丢弃、没有覆盖不满的
	// 窗口。这条断言证明的是**字段被填了、且取值在封闭枚举内**（零值
	// 空串不在 flow.Completeness 里），它证明不了任何推导逻辑；推导那一半
	// 由 collectstore 的 TestTheReportStatesItsWindowCompleteness 承担。
	if pv.WindowCompleteness != flow.CompletenessComplete {
		t.Errorf("WindowCompleteness = %q, want COMPLETE; an empty value is not a registered "+
			"flow.Completeness and leaves the caller guessing how to read the WOULD_BREAK count",
			pv.WindowCompleteness)
	}
	if pv.NotAssessedBaselines == nil {
		t.Fatal("NotAssessedBaselines is nil; an empty list says \"we checked all five kinds of " +
			"evidence and they were all there\", and that sentence must survive serialisation")
	}
	if len(pv.NotAssessedBaselines) != 0 {
		t.Errorf("NotAssessedBaselines = %v, want empty: the synthetic dataset carries all five "+
			"kinds of evidence", pv.NotAssessedBaselines)
	}
	if len(pv.MissingBaselines) == 0 {
		t.Fatal("MissingBaselines is empty; the fixture genuinely cannot derive LB_HEALTH_CHECK, " +
			"so an implementation returning two empty lists proves nothing")
	}
	var metrics bool
	for _, m := range pv.MissingBaselines {
		for _, k := range m.Kinds {
			if k == baseline.KindMetrics {
				metrics = true
			}
		}
	}
	if !metrics {
		t.Error("METRICS_SCRAPE is not reported missing anywhere in the fixture; here its evidence " +
			"IS present and simply does not cover every namespace — that is a conclusion about the " +
			"cluster, and it must not drift into the not-assessed column")
	}
}

// 删除一份集群里根本没有的策略，影响必须是「什么都不变」，而不是一个
// 算出来的数字（design doc 2026-08-24 §4.2 NOT_APPLIED）。
//
// 仓库里有、集群里没有的文件是真实存在的一类：从没被 apply 过、或已经被人
// 手工删掉过。把它算成一次有影响的删除，会让操作者以为删掉它会打断东西，
// 于是永远不敢清理仓库。
func TestDeletionImpactOfAPolicyTheClusterNeverHad(t *testing.T) {
	r := reader()
	window := fullWindow(r)

	impact, err := r.DeletionImpact(context.Background(), "prod-asia-1", window,
		[]networkingv1.NetworkPolicy{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "not-in-this-cluster"},
		}})
	if err != nil {
		t.Fatalf("DeletionImpact: %v", err)
	}
	if impact.Live != 0 {
		t.Errorf("Live = %d, want 0 —— 这份策略集群里没有", impact.Live)
	}
	if impact.InWindow != 0 {
		t.Errorf("InWindow = %d, want 0", impact.InWindow)
	}
	if impact.Removed != 1 {
		t.Errorf("Removed = %d, want 1", impact.Removed)
	}
	for _, k := range predict.AllChangeKinds() {
		if k == predict.ChangeWouldBreak || k == predict.ChangeWouldOpen {
			if got := impact.Counts[k]; got != 0 {
				t.Errorf("Counts[%s] = %d, want 0 —— 删掉一份不存在的策略不改变任何判定", k, got)
			}
		}
	}
}

// 删除一份集群里真实存在的 default-deny，影响必须被算出来且落在 Counts 里。
//
// 这一条守的是「删除是一次被评估过的变更」：没有它，删除是这个平台唯一
// 一类不被预测的写操作（2026-08-14 design doc §3 放开删除的前提之一）。
func TestDeletionImpactOfARealPolicyIsPredicted(t *testing.T) {
	r := reader()
	window := fullWindow(r)
	var cluster fixture.Cluster
	for _, c := range fixture.Load().Clusters {
		if c.ID == "prod-asia-1" {
			cluster = c
		}
	}
	if len(cluster.Policies) == 0 {
		t.Fatal("fixture cluster has no policies —— 这条用例没有被测对象")
	}

	impact, err := r.DeletionImpact(context.Background(), "prod-asia-1", window, cluster.Policies)
	if err != nil {
		t.Fatalf("DeletionImpact: %v", err)
	}
	if impact.Live != len(cluster.Policies) {
		t.Errorf("Live = %d, want %d —— 集群里就是这些策略",
			impact.Live, len(cluster.Policies))
	}
	// 合成数据集只有一份静态快照，「现在」与「窗口那一刻」必然相等。
	if impact.InWindow != impact.Live {
		t.Errorf("InWindow = %d, Live = %d —— fixture 只有一份快照，两者应当相等",
			impact.InWindow, impact.Live)
	}
	if !impact.TrafficObserved {
		t.Error("TrafficObserved = false —— 合成数据集是带流量的，删除影响算得出来")
	}
	total := 0
	for _, k := range predict.AllChangeKinds() {
		total += impact.Counts[k]
	}
	if total == 0 {
		t.Fatal("删除全部策略之后一条观测都没有被评估 —— 这不是一次预测")
	}
	// 把一个集群的策略全部删掉，等于把每一片从"有规则"变成默认放行 ——
	// 原本被拦下的连接会被放开。这是删除唯一可能的方向，也是它必须被
	// 预测出来的理由。
	if impact.Counts[predict.ChangeWouldOpen] == 0 {
		t.Errorf("删掉全部策略之后 WOULD_OPEN = 0，四类计数 = %v", impact.Counts)
	}
}

// fixture 集群的证据是 nil，不是空 map。
//
// 两者含义不同：空 map 是"记过，暂时还没有证据"，nil 是"我们根本没在记"。
// 演示集群没有采集，也就没有跨窗口的观测积累 —— 给一个空 map 会让界面把
// 它渲染成"观察了 0 个窗口"，读起来像证据不足，而实际上那句话本身就不成立。
// 与 TrafficObserved 那条纪律同源。
func TestFixturePreviewDoesNotClaimZeroEvidence(t *testing.T) {
	r := reader()
	pv, err := r.PolicyPreview(context.Background(), "prod-asia-1", "", fullWindow(r))
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	if pv.Evidence != nil {
		t.Errorf("Evidence = %v, want nil: fixture 集群没有跨窗口证据，"+
			"一个空 map 会被读成「观察了 0 个窗口」", pv.Evidence)
	}
}
