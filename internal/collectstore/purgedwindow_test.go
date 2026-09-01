package collectstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/store"
)

// **被清理掉的窗口必须报「已清理」，不能答零条。**
//
// 零条与"那时确实没有流量"长得一模一样，而后者是下游会当作事实使用的
// 结论：拓扑图空、dry-run 报"不会拦断任何连接"、写回门禁据此放行 ——
// 全部建立在一段我们自己删掉的证据上。这是保留策略唯一可能制造的新错误，
// 也是这套水位存在的全部理由（design doc 2026-09-02 §2.2）。
func TestAPurgedWindowIsRefusedNotAnsweredEmpty(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	saveRun(t, s, "run-purged", firstRunAt, assetOnlyPods())

	// 水位推到未来：等价于"这个集群的连接全被清理了"。
	if err := s.AdvanceRetainedFrom(ctx, collectedID,
		time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AdvanceRetainedFrom() error = %v", err)
	}

	w := store.TimeWindow{From: firstRunAt, To: firstRunAt.Add(time.Minute)}
	_, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: w})
	if !errors.Is(err, collectstore.ErrWindowPurged) {
		t.Errorf("Flows() error = %v, want ErrWindowPurged —— "+
			"答零条会被读成「那时没有流量」，而事实是我们把证据删了", err)
	}
}

// 水位之后的窗口照常作答：拒绝的只有被清理掉的那一段。
func TestAWindowAfterTheWatermarkIsStillAnswered(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	saveRun(t, s, "run-kept", firstRunAt, assetOnlyPods())

	if err := s.AdvanceRetainedFrom(ctx, collectedID, firstRunAt.Add(-time.Hour)); err != nil {
		t.Fatalf("AdvanceRetainedFrom() error = %v", err)
	}
	w := store.TimeWindow{From: firstRunAt, To: firstRunAt.Add(time.Minute)}
	if _, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: w}); errors.Is(err, collectstore.ErrWindowPurged) {
		t.Error("水位之后的窗口被当成已清理拒绝了")
	}
}

// 从未清理过的集群一切照旧 —— 那是绝大多数集群的常态，这套机制不得
// 改变它们的任何行为。
func TestAClusterThatWasNeverPurgedIsUnaffected(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	saveRun(t, s, "run-never", firstRunAt, assetOnlyPods())

	w := store.TimeWindow{From: firstRunAt, To: firstRunAt.Add(time.Minute)}
	if _, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: w}); errors.Is(err, collectstore.ErrWindowPurged) {
		t.Error("从未清理过的集群被判成已清理")
	}
}
