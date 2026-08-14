package mysqlregistry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// 导出必须留痕（design doc 2026-08-14 §5，规范 §43「导出数据」）：流出去的
// 是这个集群完整的网络策略，"谁在什么时候把它拿走了"要答得出来。
//
// 断言的是那条审计行的内容，不是它的条数：一条只数行数的用例，对一次把
// 时间窗或命名空间记错的导出照样是绿的 —— 而事后正是靠这两个字段判断某份
// 流出去的文件对应哪一次判定。
func TestRecordPolicyExportWritesAnAuditRow(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}

	// 集群先注册：审计行的 cluster_id 指向它。
	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	e := registry.PolicyExport{
		ClusterID: "prod-asia-1", Namespace: "payment",
		From: from, To: to, Documents: 3, Rules: 11,
	}
	if err := s.RecordPolicyExport(ctx, actor, e); err != nil {
		t.Fatalf("RecordPolicyExport() error = %v", err)
	}

	var actorCol, target, clusterID string
	var before, after sql.NullString
	if err := db.QueryRow(
		`SELECT actor, target, cluster_id, before_val, after_val
		   FROM audit_log WHERE action = 'EXPORT_POLICY'`,
	).Scan(&actorCol, &target, &clusterID, &before, &after); err != nil {
		t.Fatalf("query EXPORT_POLICY audit row: %v", err)
	}
	if actorCol != "ops-admin" {
		t.Errorf("actor = %q, want ops-admin —— 审计要答得出「谁」", actorCol)
	}
	if clusterID != "prod-asia-1" {
		t.Errorf("cluster_id = %q, want prod-asia-1", clusterID)
	}
	if target != "policy_export/prod-asia-1/payment" {
		t.Errorf("target = %q, want policy_export/prod-asia-1/payment", target)
	}
	if before.Valid {
		t.Errorf("before_val = %v, want NULL —— 导出没有改变任何状态", before)
	}
	if !after.Valid {
		t.Fatal("after_val is NULL —— 范围（命名空间、时间窗、条数）必须记下来")
	}

	var got struct {
		Namespace string    `json:"namespace"`
		From      time.Time `json:"from"`
		To        time.Time `json:"to"`
		Documents int       `json:"documents"`
		Rules     int       `json:"rules"`
	}
	if err := json.Unmarshal([]byte(after.String), &got); err != nil {
		t.Fatalf("after_val is not JSON: %v (%s)", err, after.String)
	}
	if got.Namespace != "payment" || got.Documents != 3 || got.Rules != 11 {
		t.Errorf("after_val = %+v, want namespace=payment documents=3 rules=11", got)
	}
	if !got.From.Equal(from) || !got.To.Equal(to) {
		t.Errorf("audited window = %s ~ %s, want %s ~ %s —— 换一个窗口导出的是另一套规则",
			got.From, got.To, from, to)
	}
}

// 全集群导出与单命名空间导出必须能分开检索：前者是范围最大的那一次，
// 而一个留空的 target 段读起来像是漏填了。
func TestRecordPolicyExportMarksAWholeClusterExport(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.RecordPolicyExport(ctx, actor, registry.PolicyExport{
		ClusterID: "prod-asia-1", Documents: 9, Rules: 42,
		From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordPolicyExport() error = %v", err)
	}

	var target string
	if err := db.QueryRow(
		`SELECT target FROM audit_log WHERE action = 'EXPORT_POLICY'`).Scan(&target); err != nil {
		t.Fatalf("query EXPORT_POLICY audit row: %v", err)
	}
	if target != "policy_export/prod-asia-1/*" {
		t.Errorf("target = %q, want policy_export/prod-asia-1/* for a whole-cluster export", target)
	}
}

// 导出不改变平台里的任何状态：一条往业务表里写东西的实现会让"导出"
// 这个只读操作留下无人能解释的行。
func TestRecordPolicyExportWritesNoBusinessRow(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.RecordPolicyExport(ctx, actor, registry.PolicyExport{
		ClusterID: "prod-asia-1", Documents: 1, Rules: 1,
		From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordPolicyExport() error = %v", err)
	}

	for _, tbl := range []string{"rule_override", "policy_import"} {
		var n int
		//nolint:gosec // G202: tbl comes from the fixed literal slice above, not external input
		if err := db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s rows = %d, want 0 —— 导出是只读操作", tbl, n)
		}
	}
}
