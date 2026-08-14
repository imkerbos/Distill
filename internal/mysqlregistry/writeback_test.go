package mysqlregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
)

// pushedCommit 是一个完整的 commit SHA：漂移检测拿它去仓库里定位一次提交，
// 缩写会歧义（registry.ValidateCommitSHA）。
const pushedCommit = "0123456789abcdef0123456789abcdef01234567"

// sampleWriteback 是一条写回审计记录，四类计数齐全。
func sampleWriteback() registry.Writeback {
	return registry.Writeback{
		Branch: "distill/prod-asia-1-20260814T101500Z",
		Files:  2,
		Counts: map[predict.ChangeKind]int{
			predict.ChangeWouldBreak: 0,
			predict.ChangeWouldOpen:  2,
			predict.ChangeUnchanged:  41,
			predict.ChangeUnknown:    3,
		},
	}
}

// mustBind 建集群与仓库并绑定，供写回测试拿到一条可写的绑定行。
func mustBind(t *testing.T, s *mysqlregistry.Store, actor registry.Actor) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	mustCreateGitRepo(t, s, actor)
	if err := s.BindGitRepo(ctx, actor, "prod-asia-1", sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}
}

// 出计划必须留痕（design doc 2026-08-14 §9）：那是平台宣告"我打算往策略
// 仓库里写这些"的时刻。断言的是审计行的内容而不是条数 —— 一条只数行数的
// 用例，对一次把分支或计数记错的计划照样是绿的。
func TestRecordWritebackPlanWritesAnAuditRow(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}
	mustBind(t, s, actor)

	if err := s.RecordWritebackPlan(ctx, actor, "prod-asia-1", sampleWriteback()); err != nil {
		t.Fatalf("RecordWritebackPlan() error = %v", err)
	}

	var actorCol, target, clusterID string
	if err := db.QueryRow(
		`SELECT actor, target, cluster_id
		   FROM audit_log WHERE action = 'PLAN_POLICY_WRITEBACK'`,
	).Scan(&actorCol, &target, &clusterID); err != nil {
		t.Fatalf("query PLAN_POLICY_WRITEBACK audit row: %v", err)
	}
	if actorCol != "ops-admin" || clusterID != "prod-asia-1" {
		t.Errorf("actor/cluster = %q/%q, want ops-admin/prod-asia-1", actorCol, clusterID)
	}
	if target != "policy_writeback/prod-asia-1" {
		t.Errorf("target = %q, want policy_writeback/prod-asia-1", target)
	}
	before, got := auditPayload(t, db, "PLAN_POLICY_WRITEBACK")
	if before != nil {
		t.Errorf("before_val = %v, want NULL —— 出计划没有改变任何状态", before)
	}
	if got["branch"] != "distill/prod-asia-1-20260814T101500Z" {
		t.Errorf("after.branch = %v, want 目标分支", got["branch"])
	}
	if got["files"] != float64(2) {
		t.Errorf("after.files = %v, want 2", got["files"])
	}
	counts, ok := got["counts"].(map[string]any)
	if !ok || counts[string(predict.ChangeWouldOpen)] != float64(2) {
		t.Errorf("after.counts = %v, want 四类计数", got["counts"])
	}
	if _, leaked := got["content"]; leaked {
		t.Error("after_val 带上了文件内容 —— 审计只记范围，不记内容")
	}
	// 计划阶段没有 commit：一个凭空出现的 SHA 会让读的人以为这次计划
	// 已经推出去了。
	if _, has := got["lastWrittenCommit"]; has {
		t.Errorf("after_val 带了 lastWrittenCommit = %v，出计划阶段还没有 commit",
			got["lastWrittenCommit"])
	}
}

// 推送成功后写入 last_written_commit，与审计行同事务（design doc §8）。
//
// 同时断言这次写入**只**动了这一列：一次推送不是一次配置变更，把仓库、
// 路径或校验结论一并重写，等于让一次写回悄悄改掉下发配置。
func TestSetLastWrittenCommitUpdatesOnlyThatColumnAndAudits(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}
	mustBind(t, s, actor)

	beforeRow := rowValues(t, db,
		`SELECT * FROM cluster_git_binding WHERE cluster_id = ?`, "prod-asia-1")

	if err := s.SetLastWrittenCommit(
		ctx, actor, "prod-asia-1", pushedCommit, sampleWriteback()); err != nil {
		t.Fatalf("SetLastWrittenCommit() error = %v", err)
	}

	afterRow := rowValues(t, db,
		`SELECT * FROM cluster_git_binding WHERE cluster_id = ?`, "prod-asia-1")
	if afterRow["last_written_commit"] != pushedCommit {
		t.Errorf("last_written_commit = %q, want %q",
			afterRow["last_written_commit"], pushedCommit)
	}
	for col, want := range beforeRow {
		if col == "last_written_commit" {
			continue
		}
		if afterRow[col] != want {
			t.Errorf("一次推送改动了 %s：%q -> %q —— 它只该写 last_written_commit",
				col, want, afterRow[col])
		}
	}

	var target string
	if err := db.QueryRow(
		`SELECT target FROM audit_log WHERE action = 'PUSH_POLICY_WRITEBACK'`,
	).Scan(&target); err != nil {
		t.Fatalf("query PUSH_POLICY_WRITEBACK audit row: %v", err)
	}
	if target != "policy_writeback/prod-asia-1" {
		t.Errorf("target = %q, want policy_writeback/prod-asia-1", target)
	}
	beforeVal, afterVal := auditPayload(t, db, "PUSH_POLICY_WRITEBACK")
	if beforeVal["lastWrittenCommit"] != "" {
		t.Errorf("before.lastWrittenCommit = %v, want 空串（平台此前从未写过）",
			beforeVal["lastWrittenCommit"])
	}
	if afterVal["lastWrittenCommit"] != pushedCommit {
		t.Errorf("after.lastWrittenCommit = %v, want %s", afterVal["lastWrittenCommit"], pushedCommit)
	}
	if afterVal["branch"] != "distill/prod-asia-1-20260814T101500Z" {
		t.Errorf("after.branch = %v —— 审计要答得出推到了哪条分支", afterVal["branch"])
	}
	if _, ok := afterVal["counts"].(map[string]any); !ok {
		t.Errorf("after.counts = %v, want 四类计数", afterVal["counts"])
	}
}

// 绑定不存在时既不落 commit 也不落审计：一条留下来的审计行会记录一次
// 从未发生过的推送，而复盘时没有比这更糟的输入。
func TestSetLastWrittenCommitOnAMissingBindingWritesNothing(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}
	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}

	err := s.SetLastWrittenCommit(ctx, actor, "prod-asia-1", pushedCommit, sampleWriteback())
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("SetLastWrittenCommit() error = %v, want ErrNotFound", err)
	}
	assertNoWritebackAudit(t, db)
}

// 缩写的 commit SHA 一律拒绝：一个「平台最后交出去的是这个」的标记指向
// 两个提交，比没有标记更糟。拒绝时同样不得留下审计行。
func TestSetLastWrittenCommitRefusesAnAbbreviatedCommit(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}
	mustBind(t, s, actor)
	const bindingRow = `SELECT * FROM cluster_git_binding WHERE cluster_id = ?`
	beforeRow := rowValues(t, db, bindingRow, "prod-asia-1")

	err := s.SetLastWrittenCommit(ctx, actor, "prod-asia-1", "0123456", sampleWriteback())
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("SetLastWrittenCommit(abbreviated) error = %v, want ErrInvalid", err)
	}
	assertNoWritebackAudit(t, db)

	afterRow := rowValues(t, db, bindingRow, "prod-asia-1")
	if !reflect.DeepEqual(beforeRow, afterRow) {
		t.Errorf("被拒的推送改动了绑定行：%v -> %v", beforeRow, afterRow)
	}
}

// 自由文本的计数键不得进审计：统计口径一旦掺进枚举外的取值就失去意义
// （CLAUDE.md §3）。两个方法都要挡，只挡其中一个等于没挡。
func TestWritebackRefusesCountsOutsideTheClosedEnum(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "ops-admin"}
	mustBind(t, s, actor)

	bad := sampleWriteback()
	bad.Counts["WOULD_PROBABLY_BREAK"] = 1
	if err := s.RecordWritebackPlan(ctx, actor, "prod-asia-1", bad); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("RecordWritebackPlan(unknown count kind) error = %v, want ErrInvalid", err)
	}
	if err := s.SetLastWrittenCommit(
		ctx, actor, "prod-asia-1", pushedCommit, bad); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("SetLastWrittenCommit(unknown count kind) error = %v, want ErrInvalid", err)
	}
	assertNoWritebackAudit(t, db)
}

// assertNoWritebackAudit 断言两个写回动作都没有留下审计行。
func assertNoWritebackAudit(t *testing.T, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log
		  WHERE action IN ('PLAN_POLICY_WRITEBACK', 'PUSH_POLICY_WRITEBACK')`,
	).Scan(&n); err != nil {
		t.Fatalf("count writeback audit rows: %v", err)
	}
	if n != 0 {
		t.Errorf("写回审计行数 = %d, want 0 —— 失败的写回不得留痕", n)
	}
}
