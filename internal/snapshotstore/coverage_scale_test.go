package snapshotstore_test

import (
	"context"
	"testing"
	"time"
)

// **窗口数量不该有上限。**
//
// 覆盖是按 window_start 有序的一次线性区间合并，边扫边合就只需要 O(1) 内存；
// 上限之所以存在，只是因为实现先把每一行攒进切片。而它挡住的是真实规模：
// 推送式接入下每个节点各推各的窗口，15 个节点每分钟一次 = 21600 个/天，
// 十万行是 4.6 天，不是常量注释里写的"约三年"（那按一个集群 15 分钟一个
// 窗口算，而那不是推送模式的形态）。
//
// UAT 实测：flow_ingest_run 涨到 18 万行之后，写回计划直接 500，
// 报"refusing to answer coverage from a truncated list" —— 平台再也出不了规则。
func TestCoverageHandlesMoreWindowsThanTheOldCap(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	// 造 100001 个连续窗口：正好越过旧上限。每个 1 分钟，首尾相接，
	// 因此合并后的覆盖必然等于窗口数分钟。
	const n = 100001
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO flow_ingest_run
		   (cluster_id, run_id, source_kind, window_start, window_end,
		    started_at, finished_at, status, error_reason, sample_rate, dropped)
		 VALUES (?, ?, 'CONNTRACK', ?, ?, ?, ?, 'OK', '', 1, 0)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := range n {
		from := base.Add(time.Duration(i) * time.Minute)
		to := from.Add(time.Minute)
		if _, err := stmt.ExecContext(ctx, clusterA,
			"scale-"+itoa(i), from, to, from, to); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, covered, ok, err := s.ObservedCoverage(ctx, clusterA)
	if err != nil {
		t.Fatalf("ObservedCoverage() error = %v —— 窗口多了就答不出，"+
			"平台从此出不了写回计划", err)
	}
	if !ok {
		t.Fatal("ObservedCoverage() ok = false")
	}
	if want := time.Duration(n) * time.Minute; covered != want {
		t.Errorf("covered = %s, want %s", covered, want)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
