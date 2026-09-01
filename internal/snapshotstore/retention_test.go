package snapshotstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// manyConnections 造 n 条互不相同的连接，用来验分批。
func manyConnections(n int) []flow.Connection {
	out := make([]flow.Connection, 0, n)
	for i := range n {
		out = append(out, connection(
			fmt.Sprintf("10.4.%d.%d", i/250%250, i%250),
			"10.4.200.1", int32(1024+i%40000)))
	}
	return out
}

// 从未清理过与"清理到了纪元 0"必须分得开：后者会让读取端把每一次查询
// 都判成落在保留期内。
func TestRetentionStartsUnset(t *testing.T) {
	s, _ := newTestStore(t)
	if _, ok, err := s.RetainedFrom(context.Background(), clusterA); err != nil || ok {
		t.Errorf("RetainedFrom() = ok %v err %v, want 未设置", ok, err)
	}
}

// **水位只前进，不后退。**
//
// 后退的水位会让一段已经删掉的时间重新被判成"还留着"，于是那次查询答出
// 零条而不是"已清理" —— 正是这套机制存在的理由。并发的两轮清理、或一次
// 回放旧配置，都可能带来一个更早的值。
func TestRetentionOnlyMovesForward(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	late := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	early := late.Add(-6 * time.Hour)

	if err := s.AdvanceRetainedFrom(ctx, clusterA, late); err != nil {
		t.Fatalf("AdvanceRetainedFrom() error = %v", err)
	}
	if err := s.AdvanceRetainedFrom(ctx, clusterA, early); err != nil {
		t.Fatalf("AdvanceRetainedFrom() error = %v", err)
	}
	got, ok, err := s.RetainedFrom(ctx, clusterA)
	if err != nil || !ok {
		t.Fatalf("RetainedFrom() = ok %v err %v", ok, err)
	}
	if !got.Equal(late) {
		t.Errorf("水位 = %s, want %s —— 一个后退的水位会把已删的时间说成还留着", got, late)
	}
}

// 分批删除：一次调用最多删一批，调用方据返回值决定要不要再来。
// 单条 DELETE 会在 2870 万行上长时间持锁，而这张表同时正在被摄入写入。
func TestPurgeDeletesInBoundedBatches(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	mustSaveIngest(t, s, ingestRun(clusterA, "purge-batch",
		snapshotstore.IngestOK, completeIngest(t, manyConnections(12000)...)))

	before := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	first, err := s.PurgeConnectionsBefore(ctx, clusterA, before)
	if err != nil {
		t.Fatalf("PurgeConnectionsBefore() error = %v", err)
	}
	if first == 0 {
		t.Fatal("一行都没删")
	}
	if first >= 12000 {
		t.Errorf("一次删了 %d 行 —— 分批的意义就是不要一次全删", first)
	}

	total := first
	for range 10 {
		n, err := s.PurgeConnectionsBefore(ctx, clusterA, before)
		if err != nil {
			t.Fatalf("PurgeConnectionsBefore() error = %v", err)
		}
		total += n
		if n == 0 {
			break
		}
	}
	var left int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM observed_connection WHERE cluster_id = ?`, clusterA).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("还剩 %d 行没删干净（已删 %d）", left, total)
	}
}

// 水位之后的连接一条都不能删：预览、流量列表与写回都还要读它们。
func TestPurgeKeepsWhatIsStillQueryable(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	mustSaveIngest(t, s, ingestRun(clusterA, "purge-keep",
		snapshotstore.IngestOK, completeIngest(t, manyConnections(10)...)))
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := s.PurgeConnectionsBefore(ctx, clusterA, past); err != nil || n != 0 {
		t.Fatalf("PurgeConnectionsBefore() = %d, %v, want 0 行 —— 水位之后的不得删", n, err)
	}
	var left int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM observed_connection WHERE cluster_id = ?`, clusterA).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 10 {
		t.Errorf("剩 %d 行，want 10 —— 水位之后的连接被删了", left)
	}
}
