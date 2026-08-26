package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/hubble"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// recordingCollectorStore 记下采集与摄入两侧各自收到的东西。
//
// 两侧分开存放，是这一组用例的全部前提：把摄入结果塞进采集那张表之后，
// 一个只数"落了几行"的断言依旧是绿的。
type recordingCollectorStore struct {
	// reconciliations 是落下的对账结果。
	reconciliations []snapshotstore.ReconciliationRun
	// evidence 是落下的规则证据。
	evidence []snapshotstore.RuleEvidence
	*recordingStore
	*recordingDeriveStore
	// trace 与 recordingDeriveStore 里那条是同一个，见 newCollectorStore。
	trace     *callTrace
	ingests   []snapshotstore.IngestRun
	ingestErr error
}

func newCollectorStore() *recordingCollectorStore {
	// 两侧共用同一条轨迹：先后关系只有在同一把尺子上量才成立。
	trace := &callTrace{}
	return &recordingCollectorStore{
		recordingStore:       &recordingStore{},
		recordingDeriveStore: &recordingDeriveStore{trace: trace},
		trace:                trace,
	}
}

// SaveReconciliation 记下一次对账落库。
//
// 替身里真的记下来而不是丢掉：有用例要断言"对账没跑时不落任何行"，
// 而一个把它吞掉的替身会让那条断言永远通过。
// RecordRuleEvidence 记下一次证据落库。
//
// 真的记下来而不是丢掉：有用例要断言"对账/证据没跑时不落任何行"，
// 一个把它吞掉的替身会让那条断言永远通过。
func (s *recordingCollectorStore) RecordRuleEvidence(
	_ context.Context, _ string, _, _ time.Time, _ bool,
	rules []snapshotstore.RuleEvidence,
) error {
	s.evidence = append(s.evidence, rules...)
	return nil
}

func (s *recordingCollectorStore) SaveReconciliation(
	_ context.Context, run snapshotstore.ReconciliationRun,
) error {
	s.reconciliations = append(s.reconciliations, run)
	return nil
}

func (s *recordingCollectorStore) SaveIngest(_ context.Context, run snapshotstore.IngestRun) error {
	s.trace.mark("ingest")
	s.ingests = append(s.ingests, run)
	return s.ingestErr
}

// onlyIngest 返回唯一一次摄入落库；不是恰好一次即致命。
func (s *recordingCollectorStore) onlyIngest(t *testing.T) snapshotstore.IngestRun {
	t.Helper()
	if len(s.ingests) != 1 {
		t.Fatalf("store received %d ingest runs, want exactly 1", len(s.ingests))
	}
	return s.ingests[0]
}

// stubSource 是一个不出网的摄入器，用来钉住装配层的行为。
//
// 它替代的是 internal/hubble，而不是 relay：relay 那一端的真实受力只有
// kind 集群上的端到端跑一次能证（spec §8 修订）。
type stubSource struct {
	result     flow.IngestResult
	err        error
	clusterIDs []string
	windows    []flow.Window
}

func (s *stubSource) Ingest(_ context.Context, clusterID string, w flow.Window) (flow.IngestResult, error) {
	s.clusterIDs = append(s.clusterIDs, clusterID)
	s.windows = append(s.windows, w)
	return s.result, s.err
}

func hubbleSource(result flow.IngestResult, err error) *flowSource {
	return &flowSource{source: &stubSource{result: result, err: err}, kind: flow.SourceHubble}
}

func testWindow() flow.Window {
	to := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	return flow.Window{From: to.Add(-time.Hour), To: to}
}

// oneConnection 是一条最小的、能过落库校验的连接。
func oneConnection() []flow.Connection {
	return []flow.Connection{{
		Source:        flow.Endpoint{IP: "10.4.0.7"},
		Dest:          flow.Endpoint{IP: "10.4.1.9"},
		Protocol:      flow.ProtocolTCP,
		Port:          8080,
		ObservedCount: 3,
	}}
}

// hubbleResult 组装一份来源为 Hubble 的摄入结果。
func hubbleResult(t *testing.T, covered flow.Window, conns []flow.Connection) flow.IngestResult {
	t.Helper()
	res, err := flow.NewIngestResult(flow.SourceHubble, testWindow(), covered, conns)
	if err != nil {
		t.Fatalf("flow.NewIngestResult() error = %v", err)
	}
	return res
}

// 摄入失败必须让整次运行失败，但已经采到的资产照样留下。
//
// **这条用例钉的是失败不被吞掉。** 让 collectAndIngest 在摄入出错时返回 nil
// （只落那一行 FAILED、不往上报），它会在第一个断言变红：编排系统看的是
// 退出码，一次"采成功、摄入挂掉"的运行报成功，下游拿到的就是一份没有流量
// 的窗口，而"没有流量"会被读成"覆盖这些连接的规则可以收紧"（spec §2）。
//
// 后半段同样要断言：资产值得留下 —— 一次摄入失败不得撤销一次成功的采集。
func TestAFailedIngestionFailsTheRunButKeepsTheAssets(t *testing.T) {
	store := newCollectorStore()
	src := hubbleSource(flow.IngestResult{}, hubble.ErrRelayUnavailable)

	err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testDynamic(), testFleet(),
		store, src, testWindow(), nil, nil, collectrun.ForeignPlanes{}, quietLogger())
	if !errors.Is(err, hubble.ErrRelayUnavailable) {
		t.Fatalf("collectAndIngest() error = %v, want it to wrap %v — "+
			"a swallowed ingest failure makes an incomplete picture exit zero", err, hubble.ErrRelayUnavailable)
	}

	saved := store.only(t)
	if saved.Status != snapshot.RunOK {
		t.Errorf("stored collection status = %q, want %q — the assets are worth keeping",
			saved.Status, snapshot.RunOK)
	}

	got := store.onlyIngest(t)
	if got.Status != snapshotstore.IngestFailed {
		t.Errorf("stored ingest status = %q, want %q", got.Status, snapshotstore.IngestFailed)
	}
	if got.ErrorReason != snapshotstore.IngestErrorUnreachable {
		t.Errorf("stored ingest reason = %q, want %q — UNREACHABLE sends the operator to the relay, "+
			"which is where the problem is", got.ErrorReason, snapshotstore.IngestErrorUnreachable)
	}
	if conns, _ := got.Result.Connections(); len(conns) != 0 {
		t.Errorf("a failed ingest stored %d connections; a half-read window must not read as the window", len(conns))
	}
}

// 采集失败时不得留下任何一行摄入记录。
//
// 反方向的同一条要求：一行"摄入成功"配着一次根本没读到集群的采集，在界面上
// 是一份有流量、没有资产的半张图，而那半张图看起来像"这个集群是空的"。
func TestAFailedCollectionRecordsNoIngestion(t *testing.T) {
	store := newCollectorStore()
	// 一个回答"你能写策略"的集群：只读自证会失败，采集在读任何资源之前中止。
	cs := fake.NewClientset()
	answerReviews(cs, true)
	src := hubbleSource(hubbleResult(t, testWindow(), oneConnection()), nil)

	err := collectAndIngest(t.Context(), testClusterID, cs, testDynamic(), testFleet(),
		store, src, testWindow(), nil, nil, collectrun.ForeignPlanes{}, quietLogger())
	if !errors.Is(err, ErrNotProvenReadOnly) {
		t.Fatalf("collectAndIngest() error = %v, want %v", err, ErrNotProvenReadOnly)
	}
	if len(store.ingests) != 0 {
		t.Errorf("store received %d ingest runs after a collection that never read the cluster, want 0",
			len(store.ingests))
	}
}

// 摄入运行与采集运行必须分开记（spec §6）。
//
// **这条用例钉的是两份记录不合并。** 把摄入写进采集那一行（复用它的 run id、
// 或者干脆走 Save），第二与第三个断言会变红。分开不是整洁：资产采集失败多半
// 是 RBAC，流量摄入失败多半是 relay 或配额，两者的处置人可能不同，合成一张表
// 会让排查方向变糊 —— 而合并之后再想拆开，历史数据已经分不出来了。
func TestIngestionAndCollectionAreRecordedApart(t *testing.T) {
	store := newCollectorStore()
	src := hubbleSource(hubbleResult(t, testWindow(), oneConnection()), nil)

	if err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testDynamic(), testFleet(),
		store, src, testWindow(), nil, nil, collectrun.ForeignPlanes{}, quietLogger()); err != nil {
		t.Fatalf("collectAndIngest() error = %v", err)
	}

	collection := store.only(t)
	ingest := store.onlyIngest(t)
	if len(store.aborted) != 0 {
		t.Errorf("store received %d aborted collection runs, want 0", len(store.aborted))
	}
	if ingest.RunID == "" {
		t.Fatal("the ingest run has no id; a run that cannot be joined is not a run")
	}
	if ingest.RunID == collection.Observation.RunID {
		t.Errorf("the ingest run reuses the collection run id %q: one identity for two runs makes "+
			"«assets failed» and «flows failed» indistinguishable in history", ingest.RunID)
	}
	if ingest.ClusterID != testClusterID {
		t.Errorf("stored ingest cluster id = %q, want %q — every identity row is keyed by cluster",
			ingest.ClusterID, testClusterID)
	}
	if ingest.Status != snapshotstore.IngestOK {
		t.Errorf("stored ingest status = %q, want %q", ingest.Status, snapshotstore.IngestOK)
	}
	if ingest.ErrorReason != snapshotstore.IngestErrorNone {
		t.Errorf("a successful ingest carries reason %q, want none", ingest.ErrorReason)
	}
}

// 已知漏了的摄入必须落成 PARTIAL；证明不了漏没漏的落 OK。
//
// 通过 collectAndIngest 断言而不是直接测 ingestStatus：本项目十八次"不可能
// 失败的测试"的主流形状，正是守卫被测到、却没有任何东西证明调用方还在调用它。
func TestIngestionStatusFollowsTheEvidence(t *testing.T) {
	window := testWindow()
	half := flow.Window{From: window.From, To: window.To.Add(-30 * time.Minute)}

	cases := []struct {
		name    string
		covered flow.Window
		want    snapshotstore.IngestStatus
		wantC   flow.Completeness
	}{
		// 覆盖窗口只有一半 —— 我们**知道**漏了。
		{"a half covered window", half, snapshotstore.IngestPartial, flow.CompletenessDegraded},
		// 覆盖满，但 Hubble 给不出采样率与丢弃数：跑完了，证明不了没漏。
		// 这是本来源的常态，把它记成 PARTIAL 会让真正跑漏的那些没人看。
		{"a fully covered window", window, snapshotstore.IngestOK, flow.CompletenessUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newCollectorStore()
			src := hubbleSource(hubbleResult(t, tc.covered, oneConnection()), nil)

			if err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testDynamic(), testFleet(),
				store, src, window, nil, nil, collectrun.ForeignPlanes{}, quietLogger()); err != nil {
				t.Fatalf("collectAndIngest() error = %v", err)
			}
			got := store.onlyIngest(t)
			if got.Status != tc.want {
				t.Errorf("stored ingest status = %q, want %q", got.Status, tc.want)
			}
			if _, c := got.Result.Connections(); c != tc.wantC {
				t.Errorf("stored completeness = %q, want %q — status and completeness are two fields, "+
					"and only the second one may say how much was missed", c, tc.wantC)
			}
		})
	}
}

// 摄入器拿到的必须是这次运行的集群与窗口。
//
// 集群 ID 传错的后果不是少一行：不同集群的 Pod CIDR 可能重叠，一批挂错集群的
// 连接会 join 到别的集群的 Pod 上，而且不报错（CLAUDE.md §4）。
func TestTheSourceIsAskedForThisClusterAndWindow(t *testing.T) {
	store := newCollectorStore()
	stub := &stubSource{result: hubbleResult(t, testWindow(), nil)}
	src := &flowSource{source: stub, kind: flow.SourceHubble}
	window := testWindow()

	if err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testDynamic(), testFleet(),
		store, src, window, nil, nil, collectrun.ForeignPlanes{}, quietLogger()); err != nil {
		t.Fatalf("collectAndIngest() error = %v", err)
	}
	if len(stub.clusterIDs) != 1 || stub.clusterIDs[0] != testClusterID {
		t.Errorf("source was asked for clusters %v, want exactly [%s]", stub.clusterIDs, testClusterID)
	}
	if len(stub.windows) != 1 || stub.windows[0] != window {
		t.Errorf("source was asked for windows %v, want exactly [%v]", stub.windows, window)
	}
}

// 没配置 relay 时不摄入，也不留下任何摄入记录。
//
// 一行"摄入了 0 条"与"我们没在摄入"是两件事：前者会被下游当成"这段时间没有
// 流量"，而那是 spec §2 那个单向错误的入口。
func TestNoRelayMeansNoIngestRecord(t *testing.T) {
	store := newCollectorStore()

	if err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testDynamic(), testFleet(),
		store, nil, testWindow(), nil, nil, collectrun.ForeignPlanes{}, quietLogger()); err != nil {
		t.Fatalf("collectAndIngest() error = %v", err)
	}
	store.only(t)
	if len(store.ingests) != 0 {
		t.Errorf("store received %d ingest runs without a configured relay, want 0", len(store.ingests))
	}
}

// 摄入落库失败必须变成错误，不得静默地当作摄过了。
func TestAFailedIngestStoreIsReported(t *testing.T) {
	want := errors.New("mysql is down")
	store := newCollectorStore()
	store.ingestErr = want
	src := hubbleSource(hubbleResult(t, testWindow(), oneConnection()), nil)

	err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testDynamic(), testFleet(),
		store, src, testWindow(), nil, nil, collectrun.ForeignPlanes{}, quietLogger())
	if !errors.Is(err, want) {
		t.Fatalf("collectAndIngest() error = %v, want it to wrap %v", err, want)
	}
}

// 摄入失败的原因是封闭枚举，且认不出的形态落 OTHER 而不是猜一个。
//
// 原因决定的是谁去查、往哪儿查：把一次超时说成 UNREACHABLE，会让操作者去
// 排查一条其实通着的链路。
func TestIngestErrorReasonsAreClosed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want snapshotstore.IngestErrorReason
	}{
		{"a deadline", context.DeadlineExceeded, snapshotstore.IngestErrorTimeout},
		{"an unreachable relay", hubble.ErrRelayUnavailable, snapshotstore.IngestErrorUnreachable},
		{"a wrapped deadline", errors.Join(errors.New("gave up"), context.DeadlineExceeded), snapshotstore.IngestErrorTimeout},
		{"an unrecognised failure", errors.New("something else"), snapshotstore.IngestErrorOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ingestErrorReason(tc.err); got != tc.want {
				t.Errorf("ingestErrorReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// 摄入选项自身写错时，这次运行必须起不来。
func TestIngestOptionsRejectAnUnusableWindow(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Minute, maxIngestWindow + time.Minute} {
		opts := ingestOptions{relayAddress: "hubble-relay.kube-system:4245", window: d}
		if err := opts.validate(); err == nil {
			t.Errorf("ingestOptions.validate() = nil for window %v, want a refusal", d)
		}
	}
}

// 没配置 relay 时，窗口是什么都不该拦下这次运行 —— 那是一个只采资产的部署。
func TestIngestOptionsAreOptional(t *testing.T) {
	if err := (ingestOptions{}).validate(); err != nil {
		t.Errorf("ingestOptions{}.validate() = %v, want nil", err)
	}
}
