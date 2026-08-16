package snapshotstore_test

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// 一次「还没开始采集就失败」的运行必须留下痕迹，且不得带任何资源计数。
//
// 两半都是必需的：
//
//   - 留下痕迹。不留的话 Latest 返回 ErrNoRun，界面显示"这个集群还没有过
//     任何一次资产采集"—— 与一个刚注册、采集器压根没被拉起来过的集群
//     一模一样，操作者会去等一次因为凭据配错而永远不会成功的采集。
//   - 不带计数。复用 Save 会给**每一类**资源写一行 0，而那些 0 全是假话：
//     一个资源都没被尝试过。落成一排 0 会让这一屏显示"采到了零个 Pod、
//     零个 Service"，读起来像一个空集群，而事实是我们根本没看过它。
func TestAnAbortedRunIsRecordedWithoutInventingCounts(t *testing.T) {
	store, _ := newTestStore(t)
	at := time.Date(2026, 8, 17, 4, 0, 0, 0, time.UTC)

	if err := store.SaveAbortedRun(t.Context(), clusterA, "run-aborted",
		at, at.Add(2*time.Second), snapshot.RunErrorReadOnlyUnproven); err != nil {
		t.Fatalf("SaveAbortedRun() error = %v", err)
	}

	got, err := store.Latest(t.Context(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v, want the aborted run to be readable", err)
	}
	if got.Status != string(snapshot.RunFailed) {
		t.Errorf("status = %q, want %q", got.Status, snapshot.RunFailed)
	}
	if got.ErrorReason != string(snapshot.RunErrorReadOnlyUnproven) {
		t.Errorf("errorReason = %q, want %q", got.ErrorReason, snapshot.RunErrorReadOnlyUnproven)
	}
	if len(got.Resources) != 0 {
		t.Errorf("aborted run carries %d resource outcomes, want 0: "+
			"a run that read nothing must not report counts", len(got.Resources))
	}
	if got.WarningTotal != 0 || len(got.Warnings) != 0 {
		t.Errorf("aborted run carries warnings (%d): nothing was observed, so nothing can disagree "+
			"with the registration", got.WarningTotal)
	}
}

// 没有原因的中止运行必须被拒绝。
//
// 一次没有原因的失败运行，在界面上与一次采到零资源的成功运行无法区分 ——
// 而把两者分开正是这个方法存在的全部理由。允许空原因等于给这条路径留一个
// 默认就会说错话的入口。
func TestAnAbortedRunWithoutAReasonIsRefused(t *testing.T) {
	store, db := newTestStore(t)
	at := time.Date(2026, 8, 17, 4, 0, 0, 0, time.UTC)

	if err := store.SaveAbortedRun(t.Context(), clusterA, "run-no-reason",
		at, at, snapshot.RunErrorNone); err == nil {
		t.Fatal("SaveAbortedRun() = nil error for an empty reason, want it refused")
	}

	// 拒绝必须发生在写库之前：只返回错误却仍然插了一行，界面照样会读到
	// 一条没有原因的 FAILED 运行。
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM collection_run WHERE cluster_id = ? AND run_id = ?`,
		clusterA, "run-no-reason").Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 0 {
		t.Errorf("a reasonless aborted run was written anyway (%d rows)", n)
	}
}

// 正常采集的一轮，error_reason 必须是空的。
//
// 这是上面那条的反面。少了它，一个"每一轮都写 READ_ONLY_UNPROVEN"的实现
// 同样能让前面几条通过，而界面会在每一次成功采集上顶一句"这一轮没有开始"。
func TestANormalRunCarriesNoErrorReason(t *testing.T) {
	store, _ := newTestStore(t)

	run := snapshot.Run{
		Observation: snapshot.Observation{
			ClusterID:  clusterA,
			RunID:      "run-normal",
			ObservedAt: time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC),
		},
		Status:     snapshot.RunOK,
		StartedAt:  time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 8, 17, 5, 0, 3, 0, time.UTC),
	}
	if err := store.Save(t.Context(), run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Latest(t.Context(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.ErrorReason != "" {
		t.Errorf("errorReason = %q, want empty: this run did read the cluster", got.ErrorReason)
	}
	// 采到零个资源仍然要报计数 —— 那些 0 是真话，与中止运行的缺席不同。
	if len(got.Resources) == 0 {
		t.Error("a run that did read the cluster reports no resource outcomes; " +
			"a real zero must stay a zero")
	}
}

// 中止的一轮不得盖住此前一次真的采到东西的运行。
//
// Latest 按 observed_at 取最新，因此一次失败的重试会成为界面上唯一可见的
// 那一轮。这是对的 —— 但前提是它确实更晚。这条钉住排序键，防止某次改动
// 把它换成一个与时间无关的顺序。
func TestTheLatestRunIsTheMostRecentOne(t *testing.T) {
	store, _ := newTestStore(t)
	earlier := time.Date(2026, 8, 17, 6, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)

	if err := store.Save(t.Context(), snapshot.Run{
		Observation: snapshot.Observation{ClusterID: clusterA, RunID: "run-ok", ObservedAt: earlier},
		Status:      snapshot.RunOK,
		StartedAt:   earlier,
		FinishedAt:  earlier,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveAbortedRun(t.Context(), clusterA, "run-aborted",
		later, later, snapshot.RunErrorCredentialUnavailable); err != nil {
		t.Fatalf("SaveAbortedRun() error = %v", err)
	}

	got, err := store.Latest(t.Context(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.RunID != "run-aborted" {
		t.Errorf("Latest() = %q, want the later run %q", got.RunID, "run-aborted")
	}
	if got.ErrorReason != string(snapshot.RunErrorCredentialUnavailable) {
		t.Errorf("errorReason = %q, want %q", got.ErrorReason, snapshot.RunErrorCredentialUnavailable)
	}
}

// 中止运行的原因必须原样落库、原样读回。
//
// 三个取值各测一遍而不是只测一个：它们在库里是同一列 VARCHAR，
// 一个"把所有原因都写成同一个常量"的实现只测一个是发现不了的。
func TestEveryAbortReasonRoundTrips(t *testing.T) {
	reasons := []snapshot.RunErrorReason{
		snapshot.RunErrorCredentialUnavailable,
		snapshot.RunErrorClientUnavailable,
		snapshot.RunErrorReadOnlyUnproven,
	}

	for i, reason := range reasons {
		t.Run(string(reason), func(t *testing.T) {
			store, _ := newTestStore(t)
			at := time.Date(2026, 8, 17, 7, i, 0, 0, time.UTC)

			if err := store.SaveAbortedRun(t.Context(), clusterA, "run-"+string(reason),
				at, at, reason); err != nil {
				t.Fatalf("SaveAbortedRun() error = %v", err)
			}
			got, err := store.Latest(t.Context(), clusterA)
			if err != nil {
				t.Fatalf("Latest() error = %v", err)
			}
			if got.ErrorReason != string(reason) {
				t.Errorf("errorReason = %q, want %q", got.ErrorReason, reason)
			}
		})
	}
}
