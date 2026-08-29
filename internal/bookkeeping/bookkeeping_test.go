package bookkeeping_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/bookkeeping"
	"github.com/imkerbos/Distill/internal/flow"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	l, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return l
}

func at(s string) time.Time {
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return parsed
}

// fakePreviewer 按窗口回一份写死的候选集。
type fakePreviewer struct {
	preview store.PolicyPreview
	err     error
	calls   []store.TimeWindow
}

func (f *fakePreviewer) PolicyPreviewAtGranularity(
	_ context.Context, _, _ string, w store.TimeWindow, g policygen.Granularity,
) (store.PolicyPreview, error) {
	f.calls = append(f.calls, w)
	if g != policygen.GranularityWorkload {
		return store.PolicyPreview{}, errors.New("evidence must be accounted at workload granularity")
	}
	if f.err != nil {
		return store.PolicyPreview{}, f.err
	}
	return f.preview, nil
}

type recorded struct {
	cluster  string
	from, to time.Time
	complete bool
	rules    []snapshotstore.RuleEvidence
}

// fakeStore 记下每一次记账，并报告上一次记到哪个窗口。
type fakeStore struct {
	records  []recorded
	lastEnd  map[string]time.Time
	lastErr  error
	writeErr error
}

func (f *fakeStore) RecordRuleEvidence(
	_ context.Context, clusterID string, from, to time.Time,
	complete bool, rules []snapshotstore.RuleEvidence,
) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.records = append(f.records, recorded{clusterID, from, to, complete, rules})
	return nil
}

func (f *fakeStore) LastAccountedWindowEnd(
	_ context.Context, clusterID string,
) (time.Time, bool, error) {
	if f.lastErr != nil {
		return time.Time{}, false, f.lastErr
	}
	end, ok := f.lastEnd[clusterID]
	return end, ok, nil
}

// fakeWindows 报告每个集群最近一次摄入窗口。
type fakeWindows struct {
	windows map[string]store.TimeWindow
	errs    map[string]error
}

func (f fakeWindows) DefaultWindow(_ context.Context, clusterID string) (store.TimeWindow, error) {
	if err, ok := f.errs[clusterID]; ok {
		return store.TimeWindow{}, err
	}
	w, ok := f.windows[clusterID]
	if !ok {
		return store.TimeWindow{}, errors.New("no window")
	}
	return w, nil
}

type fakeClusters []registry.Cluster

func (f fakeClusters) Clusters(context.Context) ([]registry.Cluster, error) {
	return []registry.Cluster(f), nil
}

func previewWith(c flow.Completeness) store.PolicyPreview {
	return store.PolicyPreview{
		WindowCompleteness: c,
		Candidates: []policygen.CandidatePolicy{
			{
				Namespace: "shop", Workload: "api",
				Rules: []policygen.Rule{
					{Fingerprint: "fp-a", FlowCount: 7},
					{Fingerprint: "fp-b", FlowCount: 2},
				},
			},
			{
				Namespace: "shop", Workload: "worker",
				Rules: []policygen.Rule{{Fingerprint: "fp-a", FlowCount: 1}},
			},
		},
	}
}

// 一个窗口里的每一条候选规则都要记进证据表，包括被否决的那些。
func TestRecordWindowRecordsEveryCandidateRule(t *testing.T) {
	p := &fakePreviewer{preview: previewWith(flow.CompletenessComplete)}
	s := &fakeStore{}
	rec := bookkeeping.NewRecorder(p, s, testLogger(t))

	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	if err := rec.RecordWindow(context.Background(), "c1", from, to); err != nil {
		t.Fatalf("RecordWindow: %v", err)
	}

	if len(s.records) != 1 {
		t.Fatalf("记账 %d 次，want 1", len(s.records))
	}
	got := s.records[0]
	if got.cluster != "c1" || !got.from.Equal(from) || !got.to.Equal(to) {
		t.Errorf("记的是 %s %v~%v，want c1 %v~%v", got.cluster, got.from, got.to, from, to)
	}
	if !got.complete {
		t.Error("窗口完整度是 COMPLETE，记账却说不完整")
	}
	if len(got.rules) != 3 {
		t.Fatalf("记了 %d 条规则，want 3 —— 主体不同的同一指纹是两份证据", len(got.rules))
	}
	// 主体必须跟着规则走：指纹不含主体，只按指纹归集会把一次采集算成多个窗口。
	want := map[string]int64{"shop/api/fp-a": 7, "shop/api/fp-b": 2, "shop/worker/fp-a": 1}
	for _, r := range got.rules {
		key := snapshotstore.EvidenceKey(r.Namespace, r.Workload, r.Fingerprint)
		obs, ok := want[key]
		if !ok {
			t.Errorf("记了一条没预期的证据 %s", key)
			continue
		}
		if r.Observations != obs {
			t.Errorf("%s 的观测次数是 %d，want %d", key, r.Observations, obs)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("%s 没有被记进证据", key)
	}
}

// 证明不了完整的窗口必须记成不完整。
//
// 记成完整会让"看了二十次"被读成"看全了二十次"，而后者是写回门禁读的那个数。
func TestRecordWindowMarksAWindowItCannotProveComplete(t *testing.T) {
	for _, c := range []flow.Completeness{flow.CompletenessUnknown, flow.CompletenessDegraded} {
		p := &fakePreviewer{preview: previewWith(c)}
		s := &fakeStore{}
		rec := bookkeeping.NewRecorder(p, s, testLogger(t))
		if err := rec.RecordWindow(context.Background(), "c1",
			at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")); err != nil {
			t.Fatalf("RecordWindow(%s): %v", c, err)
		}
		if len(s.records) != 1 {
			t.Fatalf("完整度 %s：记账 %d 次，want 1", c, len(s.records))
		}
		if s.records[0].complete {
			t.Errorf("完整度 %s 却记成了 COMPLETE", c)
		}
	}
}

// 候选集算不出来时不记账，且报错 —— 静默跳过会让这个窗口从每条规则的证据里
// 消失，而缺席看起来与"这段时间没有流量"一模一样。
func TestRecordWindowRefusesWhenCandidatesCannotBeGenerated(t *testing.T) {
	p := &fakePreviewer{err: errors.New("boom")}
	s := &fakeStore{}
	rec := bookkeeping.NewRecorder(p, s, testLogger(t))
	if err := rec.RecordWindow(context.Background(), "c1",
		at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")); err == nil {
		t.Fatal("候选集算不出来却当成了一次成功的记账")
	}
	if len(s.records) != 0 {
		t.Errorf("候选集算不出来却记了 %d 次账", len(s.records))
	}
}

// 推送式接入下，记账按集群的最近一次摄入窗口走。
func TestAccountOnceRecordsTheLatestIngestWindow(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	s := &fakeStore{lastEnd: map[string]time.Time{}}
	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "c1", DataSource: registry.DataSourceCollected}},
		fakeWindows{windows: map[string]store.TimeWindow{"c1": {From: from, To: to}}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, s, testLogger(t)), Accounted: s})

	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(s.records) != 1 {
		t.Fatalf("记账 %d 次，want 1", len(s.records))
	}
	if !s.records[0].to.Equal(to) {
		t.Errorf("记的窗口末端是 %v，want %v", s.records[0].to, to)
	}
}

// **已经记过的窗口不得再记一次。**
//
// windows 是 `windows + 1`，按窗口不幂等。记账周期比 agent 推送快、或者 agent
// 整个停了，同一个窗口会被反复记 —— 于是一条规则在无人观测的情况下看起来
// 越来越可信，而那正是这个平台唯一那个单向的失败方向。
func TestAccountOnceSkipsAWindowItAlreadyAccounted(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	s := &fakeStore{lastEnd: map[string]time.Time{"c1": to}}
	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "c1", DataSource: registry.DataSourceCollected}},
		fakeWindows{windows: map[string]store.TimeWindow{"c1": {From: from, To: to}}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, s, testLogger(t)), Accounted: s})

	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(s.records) != 0 {
		t.Fatalf("已经记过的窗口又记了 %d 次 —— 无人观测时证据会自己变强", len(s.records))
	}
	if len(p.calls) != 0 {
		t.Errorf("跳过的窗口仍然算了 %d 次候选集，白花的开销", len(p.calls))
	}
}

// 窗口往前走了就要记。
func TestAccountOnceRecordsAWindowNewerThanTheLastAccounted(t *testing.T) {
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	s := &fakeStore{lastEnd: map[string]time.Time{"c1": at("2026-08-27T10:01:00Z")}}
	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "c1", DataSource: registry.DataSourceCollected}},
		fakeWindows{windows: map[string]store.TimeWindow{
			"c1": {From: at("2026-08-27T10:01:00Z"), To: at("2026-08-27T10:02:00Z")},
		}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, s, testLogger(t)), Accounted: s})

	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(s.records) != 1 {
		t.Fatalf("窗口往前走了却记账 %d 次，want 1", len(s.records))
	}
}

// 一个集群失败不得带走其余集群这一轮的记账。
func TestAccountOnceKeepsGoingWhenOneClusterFails(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	s := &fakeStore{lastEnd: map[string]time.Time{}}
	a := bookkeeping.NewAccountant(
		fakeClusters{
			{ID: "broken", DataSource: registry.DataSourceCollected},
			{ID: "c1", DataSource: registry.DataSourceCollected},
		},
		fakeWindows{
			windows: map[string]store.TimeWindow{"c1": {From: from, To: to}},
			errs:    map[string]error{"broken": errors.New("no ingest yet")},
		},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, s, testLogger(t)), Accounted: s})

	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(s.records) != 1 {
		t.Fatalf("一个集群没有摄入，其余 %d 个也跟着没记账", 1-len(s.records))
	}
	if s.records[0].cluster != "c1" {
		t.Errorf("记的是 %s，want c1", s.records[0].cluster)
	}
}

// 读不出"记到哪个窗口"时跳过这一轮，**不当成"没记过"**。
//
// 当成没记过，这条查询坏掉的每一轮都会重记同一个窗口；而证据只增不减，
// 涨上去就下不来 —— 一次数据库故障会永久地把一批规则说得比实际可信。
func TestAccountOnceSkipsWhenItCannotTellWhatWasAlreadyAccounted(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	s := &fakeStore{lastErr: errors.New("connection reset")}
	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "c1", DataSource: registry.DataSourceCollected}},
		fakeWindows{windows: map[string]store.TimeWindow{"c1": {From: from, To: to}}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, s, testLogger(t)), Accounted: s})

	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(s.records) != 0 {
		t.Fatalf("读不出已记窗口却照样记了 %d 次账", len(s.records))
	}
}

// **演示集群不得积累证据。**
//
// fixture 数据集是合成的，它的"观测"不描述任何真实集群。给它记账会让一条
// 规则带着一份看起来很扎实的证据出现在屏幕上，而那份证据背后一条真实连接
// 都没有 —— 这正是这个平台能造成的最严重后果的那个形态。
//
// 采集侧 Reader 的来源门禁也会拒掉它（DefaultWindow 对 FIXTURE 答
// ErrClusterNotFound），这里是第二道：那一道守的是"读谁的数据"，这一道守的
// 是"给谁记账"，装配被拨反时仍然成立。
func TestAccountOnceNeverAccountsAFixtureCluster(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	s := &fakeStore{lastEnd: map[string]time.Time{}}
	a := bookkeeping.NewAccountant(
		fakeClusters{
			{ID: "demo", DataSource: registry.DataSourceFixture},
			{ID: "c1", DataSource: registry.DataSourceCollected},
		},
		fakeWindows{windows: map[string]store.TimeWindow{
			"demo": {From: from, To: to},
			"c1":   {From: from, To: to},
		}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, s, testLogger(t)), Accounted: s})

	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	for _, r := range s.records {
		if r.cluster == "demo" {
			t.Fatal("给演示集群记了证据；合成数据会长成一份看起来扎实的推荐依据")
		}
	}
	if len(s.records) != 1 || s.records[0].cluster != "c1" {
		t.Errorf("记账 %d 次，want 只给 c1 记一次", len(s.records))
	}
}

// 来源没登记的集群同样不记：封闭枚举落到未知取值时失败方向朝关。
func TestAccountOnceSkipsAnUnregisteredDataSource(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	s := &fakeStore{lastEnd: map[string]time.Time{}}
	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "weird", DataSource: registry.DataSource("SOMETHING_ELSE")}},
		fakeWindows{windows: map[string]store.TimeWindow{"weird": {From: from, To: to}}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, s, testLogger(t)), Accounted: s})

	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(s.records) != 0 {
		t.Errorf("来源未登记的集群被记了 %d 次账", len(s.records))
	}
}

// ---- 对账记账 ----

// fakeReconciler 回一份写死的对账报告。
type fakeReconciler struct {
	report store.ReconciliationReport
	err    error
	calls  int
}

func (f *fakeReconciler) Reconciliation(
	_ context.Context, _ string, _ store.TimeWindow,
) (store.ReconciliationReport, error) {
	f.calls++
	if f.err != nil {
		return store.ReconciliationReport{}, f.err
	}
	return f.report, nil
}

type savedRun struct {
	run snapshotstore.ReconciliationRun
}

type fakeReconcileStore struct {
	saved   []savedRun
	lastEnd map[string]time.Time
	lastErr error
}

func (f *fakeReconcileStore) SaveReconciliation(
	_ context.Context, run snapshotstore.ReconciliationRun,
) error {
	f.saved = append(f.saved, savedRun{run})
	return nil
}

func (f *fakeReconcileStore) LastAccountedWindowEnd(
	_ context.Context, clusterID string,
) (time.Time, bool, error) {
	if f.lastErr != nil {
		return time.Time{}, false, f.lastErr
	}
	end, ok := f.lastEnd[clusterID]
	return end, ok, nil
}

func reportWith(reported bool) store.ReconciliationReport {
	subjects := []reconcile.SubjectCounts{
		{Subject: reconcile.Subject{Namespace: "shop", Workload: "api"}},
		{Subject: reconcile.Subject{Namespace: "shop", Workload: "worker"}},
	}
	return store.ReconciliationReport{
		SourceReportsVerdicts: reported,
		Report:                reconcile.Report{BySubject: subjects},
	}
}

// 执行面报判定时，逐主体的明细照常落库 —— 那是一致率趋势的下钻依据。
func TestAgreementRecorderKeepsSubjectsWhenTheSourceReportsVerdicts(t *testing.T) {
	r := &fakeReconciler{report: reportWith(true)}
	s := &fakeReconcileStore{}
	rec := bookkeeping.NewAgreementRecorder(r, s, testLogger(t))

	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	if err := rec.RecordWindow(context.Background(), "c1", from, to); err != nil {
		t.Fatalf("RecordWindow: %v", err)
	}
	if len(s.saved) != 1 {
		t.Fatalf("落库 %d 次，want 1", len(s.saved))
	}
	if got := len(s.saved[0].run.Report.BySubject); got != 2 {
		t.Errorf("落了 %d 个主体，want 2", got)
	}
	if !s.saved[0].run.SourceReports {
		t.Error("来源报判定，落库却说不报")
	}
}

// **来源不报判定时只落这一轮本身，不落逐主体明细。**
//
// conntrack 接入下每个主体都是 SOURCE_SILENT，几百行长得一模一样、零信息量，
// 而这一轮每几分钟就跑一次 —— 那是纯粹的账单（CLAUDE.md §5）。
//
// 但**这一轮必须落**：趋势页在没有 run 时返回空数组，而空趋势读起来是
// "这个集群还没对过账"，操作者会去等一份永远不会出现的曲线。落下来它才说得出
// "这条接入方式对不了账"。
func TestAgreementRecorderDropsSubjectsWhenTheSourceIsSilent(t *testing.T) {
	r := &fakeReconciler{report: reportWith(false)}
	s := &fakeReconcileStore{}
	rec := bookkeeping.NewAgreementRecorder(r, s, testLogger(t))

	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	if err := rec.RecordWindow(context.Background(), "c1", from, to); err != nil {
		t.Fatalf("RecordWindow: %v", err)
	}
	if len(s.saved) != 1 {
		t.Fatalf("落库 %d 次，want 1 —— 空趋势会被读成「没问题」", len(s.saved))
	}
	if got := len(s.saved[0].run.Report.BySubject); got != 0 {
		t.Errorf("来源不报判定，却落了 %d 行零信息量的主体明细", got)
	}
	if s.saved[0].run.SourceReports {
		t.Error("来源不报判定，落库却说报")
	}
	if !s.saved[0].run.WindowFrom.Equal(from) || !s.saved[0].run.WindowTo.Equal(to) {
		t.Errorf("落的窗口是 %v~%v，want %v~%v",
			s.saved[0].run.WindowFrom, s.saved[0].run.WindowTo, from, to)
	}
}

// 算不出来就不落：一份空报告落进趋势会变成一个没人解释的凹陷。
func TestAgreementRecorderRefusesWhenReconciliationFails(t *testing.T) {
	r := &fakeReconciler{err: errors.New("boom")}
	s := &fakeReconcileStore{}
	rec := bookkeeping.NewAgreementRecorder(r, s, testLogger(t))
	if err := rec.RecordWindow(context.Background(), "c1",
		at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")); err == nil {
		t.Fatal("对账算不出来却当成了一次成功")
	}
	if len(s.saved) != 0 {
		t.Errorf("对账算不出来却落了 %d 行", len(s.saved))
	}
}

// 调度器把每个任务各跑一次，且各用各的守卫。
func TestAccountOnceRunsEveryTask(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	es := &fakeStore{lastEnd: map[string]time.Time{}}
	r := &fakeReconciler{report: reportWith(false)}
	rs := &fakeReconcileStore{lastEnd: map[string]time.Time{}}

	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "c1", DataSource: registry.DataSourceCollected}},
		fakeWindows{windows: map[string]store.TimeWindow{"c1": {From: from, To: to}}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, es, testLogger(t)), Accounted: es},
		bookkeeping.Task{Name: "agreement",
			Recorder: bookkeeping.NewAgreementRecorder(r, rs, testLogger(t)), Accounted: rs},
	)
	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(es.records) != 1 {
		t.Errorf("证据记账 %d 次，want 1", len(es.records))
	}
	if len(rs.saved) != 1 {
		t.Errorf("对账记账 %d 次，want 1", len(rs.saved))
	}
}

// 一个任务已经记过这个窗口，不影响另一个任务照常记。
//
// 两个任务各有各的守卫：合成一个的话，证据记成功、对账失败之后，下一轮会
// 因为"这个窗口记过了"把对账永久跳过。
func TestAccountOnceGuardsEachTaskSeparately(t *testing.T) {
	from, to := at("2026-08-27T10:00:00Z"), at("2026-08-27T10:01:00Z")
	p := &fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}
	es := &fakeStore{lastEnd: map[string]time.Time{"c1": to}} // 证据已记过
	r := &fakeReconciler{report: reportWith(false)}
	rs := &fakeReconcileStore{lastEnd: map[string]time.Time{}} // 对账没记过

	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "c1", DataSource: registry.DataSourceCollected}},
		fakeWindows{windows: map[string]store.TimeWindow{"c1": {From: from, To: to}}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(p, es, testLogger(t)), Accounted: es},
		bookkeeping.Task{Name: "agreement",
			Recorder: bookkeeping.NewAgreementRecorder(r, rs, testLogger(t)), Accounted: rs},
	)
	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(es.records) != 0 {
		t.Errorf("证据已经记过这个窗口，却又记了 %d 次", len(es.records))
	}
	if len(rs.saved) != 1 {
		t.Errorf("对账被证据那一侧的守卫连累，记了 %d 次，want 1", len(rs.saved))
	}
}

// **从累积证据并进来、本窗口没观测到的规则不记账。**
//
// 这是 2026-08-29 那次事故的复现用例。候选集里混着两种规则：这个窗口学到的，
// 和 policygen.MergeLearned 并进来的。后者的 FlowCount 是**累计**观测数，
// 把它当成本窗口的增量记进去，落库那句
//
//	observations = observations + VALUES(observations)
//
// 就会每轮把累计数加到自己身上——指数翻倍。UAT 实测 65 轮之后 observations
// 涨到 1.38e19，超出 int64，读回时 Scan 失败，预览挂掉、记账跟着挂掉，
// 整条链停了 13 小时而界面上只是数字不再更新。
func TestUnobservedRulesAreNotAccountedAsFreshObservations(t *testing.T) {
	pv := previewWith(flow.CompletenessComplete)
	// 候选集里多一条从累积并进来的规则，FlowCount 是累计数。
	pv.Candidates = append(pv.Candidates, policygen.CandidatePolicy{
		Namespace: "devops", Workload: "nacos",
		Rules: []policygen.Rule{{Fingerprint: "fp-accumulated", FlowCount: 40_000}},
	})
	pv.UnobservedRules = []policygen.UnobservedRule{{
		Namespace: "devops", Workload: "nacos", Fingerprint: "fp-accumulated",
		LastSeen: at("2026-08-29T00:00:00Z"),
	}}

	p := &fakePreviewer{preview: pv}
	s := &fakeStore{}
	rec := bookkeeping.NewRecorder(p, s, testLogger(t))

	if err := rec.RecordWindow(context.Background(), "c1",
		at("2026-08-29T10:00:00Z"), at("2026-08-29T10:01:00Z")); err != nil {
		t.Fatalf("RecordWindow: %v", err)
	}
	if len(s.records) != 1 {
		t.Fatalf("记账 %d 次，want 1", len(s.records))
	}
	for _, r := range s.records[0].rules {
		if r.Fingerprint == "fp-accumulated" {
			t.Fatalf("本窗口没观测到的规则被当成一次新观测记了进去"+
				"（observations=%d）——落库那句加法会让它每轮翻倍", r.Observations)
		}
	}
	// 窗口里真正观测到的那些照记不误。
	if len(s.records[0].rules) == 0 {
		t.Error("把观测到的规则也一并跳过了")
	}
}
