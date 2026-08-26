package snapshotstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// 落库与读回逐字段一致，且趋势按窗口倒序 —— 这个指标唯一有行动含义的读法
// 是「在变好还是变坏」（design doc 2026-08-25 §3）。
func TestReconciliationRoundTripAndTrend(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	save := func(runID string, at time.Time, agree, under int) {
		t.Helper()
		err := s.SaveReconciliation(ctx, snapshotstore.ReconciliationRun{
			ClusterID: "prod-asia-1", RunID: runID,
			WindowFrom: at, WindowTo: at.Add(time.Minute), ComputedAt: at.Add(time.Minute),
			SourceReports: true,
			Report: reconcile.Report{
				Total:   agree + under,
				Overall: reconcile.Counts{reconcile.ClassAgree: agree, reconcile.ClassUnderPermissive: under},
				BySubject: []reconcile.SubjectCounts{{
					Subject: reconcile.Subject{Namespace: "payment", Workload: "api"},
					Counts:  reconcile.Counts{reconcile.ClassAgree: agree, reconcile.ClassUnderPermissive: under},
				}},
			},
		})
		if err != nil {
			t.Fatalf("SaveReconciliation(%s) = %v", runID, err)
		}
	}

	save("11111111111111111111111111111111", base, 100, 0)
	save("22222222222222222222222222222222", base.Add(time.Hour), 97, 3)

	trend, err := s.ReconciliationTrend(ctx, "prod-asia-1", 10)
	if err != nil {
		t.Fatalf("ReconciliationTrend() = %v", err)
	}
	if len(trend) != 2 {
		t.Fatalf("趋势里有 %d 条, want 2", len(trend))
	}
	// 最近的在前：一屏只看得下几行时，要先看到最新那次。
	if trend[0].RunID != "22222222222222222222222222222222" {
		t.Errorf("趋势不是按窗口倒序：第一条是 %s", trend[0].RunID)
	}
	first, ok := trend[0].Report.Overall.AgreementRate()
	if !ok || first >= 1 {
		t.Errorf("最近一次一致率 = %v (%v), 期望低于 1 —— 它正是「变坏了」那个信号", first, ok)
	}
	if second, _ := trend[1].Report.Overall.AgreementRate(); second != 1 {
		t.Errorf("上一次一致率 = %v, want 1", second)
	}
}

// 主体数超过上限时整次拒绝，不截断。
//
// 一份被截掉一半的分歧清单，在界面上读起来是"只有这几个 workload 有问题"，
// 而那句话没有人算过。
func TestAnOversizeReconciliationIsRefusedNotTruncated(t *testing.T) {
	s, _ := newTestStore(t)

	subjects := make([]reconcile.SubjectCounts, 5001)
	for i := range subjects {
		subjects[i] = reconcile.SubjectCounts{
			Subject: reconcile.Subject{Namespace: "ns", Workload: string(rune('a' + i%26))},
			Counts:  reconcile.Counts{reconcile.ClassAgree: 1},
		}
	}
	err := s.SaveReconciliation(context.Background(), snapshotstore.ReconciliationRun{
		ClusterID: "prod-asia-1", RunID: "33333333333333333333333333333333",
		WindowFrom: time.Now().UTC(), WindowTo: time.Now().UTC(),
		Report: reconcile.Report{BySubject: subjects},
	})
	if err == nil {
		t.Error("超过上限的对账被落库了 —— 截断后的分歧清单会被读成一份完整清单")
	}
}

// saveIngestWindow 落一次指定窗口起点与状态的摄入。
func saveIngestWindow(
	t *testing.T, s *snapshotstore.Store, runID string,
	from time.Time, status snapshotstore.IngestStatus,
) {
	t.Helper()
	win := flow.Window{From: from, To: from.Add(time.Minute)}
	res, err := flow.NewIngestResult(flow.SourceHubble, win, win, nil)
	if err != nil {
		t.Fatalf("NewIngestResult() = %v", err)
	}
	if err := s.SaveIngest(t.Context(), snapshotstore.IngestRun{
		ClusterID: clusterA, RunID: runID,
		StartedAt: from, FinishedAt: from.Add(time.Minute),
		Status: status, Result: res.WithSampleRate(1).WithDropped(0),
		// 失败的摄入必须带原因：落库层拒绝一次没有原因的失败 —— 那种行
		// 与"这个窗口本来就没有流量"分不开。
		ErrorReason: failureReasonFor(status),
	}); err != nil {
		t.Fatalf("SaveIngest(%s) = %v", runID, err)
	}
}

// failureReasonFor 给失败的摄入配一个封闭枚举的原因。
func failureReasonFor(status snapshotstore.IngestStatus) snapshotstore.IngestErrorReason {
	if status == snapshotstore.IngestFailed {
		return snapshotstore.IngestErrorUnreachable
	}
	return ""
}

// 观测覆盖不等于首末之间的墙钟跨度。
//
// 一个集群 90 天前摄入过一次、之后采集器坏了 89 天、今天恢复：首末跨度是
// 90 天，而真正被观测到的只有两分钟。写回门禁拿跨度与业务周期比，会把这个
// 集群放行 —— 而它恰恰是最不该放行的那一类：一份基于两分钟观测的
// default-deny，下发的是"这两分钟之外的一切都拦掉"。
//
// 这与 P0-a（把 PARTIAL 采集当成事实）是同一类错误：用一个看起来够大的
// 数字，掩盖掉它背后根本没有的证据。
func TestObservedCoverageIsNotTheWallClockSpan(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, _, ok, err := s.ObservedCoverage(ctx, clusterA); err != nil || ok {
		t.Fatalf("一次摄入都没有时 ObservedCoverage() = %v, %v, want false, nil", ok, err)
	}

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	saveIngestWindow(t, s, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1", base, snapshotstore.IngestOK)
	saveIngestWindow(t, s, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2",
		base.Add(90*24*time.Hour), snapshotstore.IngestOK)

	span, covered, ok, err := s.ObservedCoverage(ctx, clusterA)
	if err != nil || !ok {
		t.Fatalf("ObservedCoverage() = %v, %v, %v, %v", span, covered, ok, err)
	}
	if span != 90*24*time.Hour+time.Minute {
		t.Errorf("span = %v, want 90 天零 1 分钟", span)
	}
	if covered != 2*time.Minute {
		t.Errorf("covered = %v, want 2 分钟 —— 中间那 89 天没有任何摄入，"+
			"不能算作被观测过", covered)
	}
}

// 重叠与相邻的窗口只算一次，不叠加。
//
// 重跑一段历史、或者两条采集链路窗口交叠，都会让同一段时间被记两次。
// 直接把窗口时长求和会让覆盖凭空变长 —— 而这个数字是用来决定"能不能下发"的。
func TestObservedCoverageMergesOverlappingWindows(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// 三个窗口，各 1 分钟：前两个完全重叠，第三个紧邻前一个的结束时刻。
	saveIngestWindow(t, s, "ccccccccccccccccccccccccccccccc1", base, snapshotstore.IngestOK)
	saveIngestWindow(t, s, "ccccccccccccccccccccccccccccccc2", base, snapshotstore.IngestOK)
	saveIngestWindow(t, s, "ccccccccccccccccccccccccccccccc3",
		base.Add(time.Minute), snapshotstore.IngestOK)

	_, covered, ok, err := s.ObservedCoverage(ctx, clusterA)
	if err != nil || !ok {
		t.Fatalf("ObservedCoverage() = %v, %v, %v", covered, ok, err)
	}
	if covered != 2*time.Minute {
		t.Errorf("covered = %v, want 2 分钟 —— 重叠的那一分钟被算了两次", covered)
	}
}

// 失败的摄入不进覆盖：它证明不了那段时间被观测过。
func TestObservedCoverageIgnoresFailedIngests(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	saveIngestWindow(t, s, "ddddddddddddddddddddddddddddddd1", base, snapshotstore.IngestOK)
	saveIngestWindow(t, s, "ddddddddddddddddddddddddddddddd2",
		base.Add(time.Hour), snapshotstore.IngestFailed)

	_, covered, ok, err := s.ObservedCoverage(ctx, clusterA)
	if err != nil || !ok {
		t.Fatalf("ObservedCoverage() = %v, %v, %v", covered, ok, err)
	}
	if covered != time.Minute {
		t.Errorf("covered = %v, want 1 分钟 —— 失败的那次被算进了覆盖", covered)
	}
}
