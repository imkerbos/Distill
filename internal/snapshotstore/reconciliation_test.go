package snapshotstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/replay"
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

// 分歧样本与计数落在同一个事务里，且读得回来。
//
// 一份有比率却没有证据的记录，正是样本表要消除的那种状态：门禁按比率拦人，
// 而操作者拿着一个比率无从判断平台漏了什么。
func TestReconciliationSamplesArePersistedWithTheCounts(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	subject := reconcile.Subject{Namespace: "payment", Workload: "api"}

	run := snapshotstore.ReconciliationRun{
		ClusterID: clusterA, RunID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeee1",
		WindowFrom: at, WindowTo: at.Add(time.Minute), ComputedAt: at,
		SourceReports: true,
		Report: reconcile.Report{
			Total:   2,
			Overall: reconcile.Counts{reconcile.ClassUnderPermissive: 2},
			BySubject: []reconcile.SubjectCounts{{
				Subject: subject,
				Counts:  reconcile.Counts{reconcile.ClassUnderPermissive: 2},
			}},
			Samples: []reconcile.Sample{
				{Subject: subject, Class: reconcile.ClassUnderPermissive, Flow: replay.Flow{
					Source: replay.Endpoint{IP: "10.0.0.1"}, Dest: replay.Endpoint{IP: "10.0.0.9"},
					Protocol: replay.ProtocolTCP, Port: 5432, Timestamp: at,
				}},
				// 同一条连接在一个窗口里出现两次是正常的，而"同一个端口反复
				// 出现"恰恰是最要紧的信号 —— 两条都必须留下来，不能被折叠。
				{Subject: subject, Class: reconcile.ClassUnderPermissive, Flow: replay.Flow{
					Source: replay.Endpoint{IP: "10.0.0.1"}, Dest: replay.Endpoint{IP: "10.0.0.9"},
					Protocol: replay.ProtocolTCP, Port: 5432, Timestamp: at,
				}},
			},
		},
	}
	if err := s.SaveReconciliation(ctx, run); err != nil {
		t.Fatalf("SaveReconciliation() = %v", err)
	}

	got, err := s.ReconciliationSamples(ctx, clusterA, run.RunID)
	if err != nil {
		t.Fatalf("ReconciliationSamples() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("拿到 %d 条样本, want 2 —— 同一条连接的两次出现被折叠掉了: %+v", len(got), got)
	}
	if got[0].Subject != subject || got[0].Class != reconcile.ClassUnderPermissive {
		t.Errorf("样本挂错了主体或类别：%+v", got[0])
	}
	if got[0].Flow.Port != 5432 || got[0].Flow.Dest.IP != "10.0.0.9" {
		t.Errorf("样本没带回可下钻的内容：%+v", got[0].Flow)
	}
	if !got[0].Flow.Timestamp.Equal(at) {
		t.Errorf("Timestamp = %v, want %v —— 存的必须是连接发生的时刻，"+
			"不是记录写入的时刻，否则下钻会对齐到错误的那份 Pod 名册",
			got[0].Flow.Timestamp, at)
	}
}

// LastReconciliationWindowEnd 报告这个集群的对账记到了哪个窗口末端。
//
// 推送式接入靠它避免把同一个窗口对两次账：记账周期与 agent 推送周期是两条
// 独立的节奏，而趋势里同一个窗口出现两次会被读成"这段时间波动很大"。
func TestLastReconciliationWindowEndReportsTheFurthestWindow(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	// 一次都没对过账时必须说"没记过"，而不是答一个零值时刻 —— 零值早于任何
	// 真实窗口，会让第一个窗口被当成"已经对过"而永远跳过。
	if _, ok, err := s.LastReconciliationWindowEnd(ctx, clusterA); err != nil {
		t.Fatalf("LastReconciliationWindowEnd() = %v", err)
	} else if ok {
		t.Error("一次都没对过账，却报告说对过了")
	}

	save := func(runID string, at time.Time) {
		t.Helper()
		if err := s.SaveReconciliation(ctx, snapshotstore.ReconciliationRun{
			ClusterID: clusterA, RunID: runID,
			WindowFrom: at, WindowTo: at.Add(time.Minute), ComputedAt: at.Add(time.Minute),
			SourceReports: false,
			Report:        reconcile.Report{},
		}); err != nil {
			t.Fatalf("SaveReconciliation(%s) = %v", runID, err)
		}
	}
	// 先晚后早：取的必须是最远的那一端，不是最后写进去的那一条。
	save("33333333333333333333333333333333", base.Add(2*time.Hour))
	save("44444444444444444444444444444444", base)

	got, ok, err := s.LastReconciliationWindowEnd(ctx, clusterA)
	if err != nil {
		t.Fatalf("LastReconciliationWindowEnd() = %v", err)
	}
	if !ok {
		t.Fatal("对过两个窗口，却报告说没对过")
	}
	want := base.Add(2*time.Hour + time.Minute)
	if !got.Equal(want) {
		t.Errorf("LastReconciliationWindowEnd() = %v, want %v", got, want)
	}

	// 集群之间不得串。
	if _, ok, err := s.LastReconciliationWindowEnd(ctx, clusterB); err != nil {
		t.Fatalf("LastReconciliationWindowEnd(clusterB) = %v", err)
	} else if ok {
		t.Error("另一个集群的对账被当成了这个集群的")
	}
}

// 来源不报判定时，一份没有逐主体明细的对账照样落得下去。
//
// conntrack 接入下每个主体都是 SOURCE_SILENT，几百行长得一模一样、零信息量，
// 记账因此丢掉它们；但这一轮本身必须落得下来，否则趋势永远是空的，而空趋势
// 读起来是"这个集群还没对过账"。
func TestAReconciliationWithoutSubjectsIsStored(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	if err := s.SaveReconciliation(ctx, snapshotstore.ReconciliationRun{
		ClusterID: clusterA, RunID: "55555555555555555555555555555555",
		WindowFrom: at, WindowTo: at.Add(time.Minute), ComputedAt: at.Add(time.Minute),
		SourceReports: false,
		Report: reconcile.Report{
			Total:   42,
			Overall: reconcile.Counts{reconcile.ClassSourceSilent: 42},
		},
	}); err != nil {
		t.Fatalf("SaveReconciliation() = %v", err)
	}

	trend, err := s.ReconciliationTrend(ctx, clusterA, 10)
	if err != nil {
		t.Fatalf("ReconciliationTrend() = %v", err)
	}
	if len(trend) != 1 {
		t.Fatalf("趋势里有 %d 个点，want 1 —— 空趋势会被读成「没问题」", len(trend))
	}
	if trend[0].SourceReports {
		t.Error("落的是「来源不报判定」，读回来却说报")
	}
	if got := trend[0].Report.Overall[reconcile.ClassSourceSilent]; got != 42 {
		t.Errorf("SOURCE_SILENT = %d, want 42", got)
	}
}
