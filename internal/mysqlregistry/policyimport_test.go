package mysqlregistry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

func sampleImport() registry.PolicyImport {
	return registry.PolicyImport{
		ClusterID: "prod-asia-1", ImportID: "imp-1", Plane: "networkpolicy",
		Role: registry.RoleBaselineCurrent, Source: registry.SourcePaste,
		Namespace: "payment", Name: "allow-gateway",
		YAML: "kind: NetworkPolicy\n", SpecHash: "abc",
		ImportedBy: "admin", ImportedAt: time.Now().UTC(),
	}
}

func TestCreateAndListPolicyImports(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.CreatePolicyImport(ctx, actor, sampleImport()); err != nil {
		t.Fatalf("CreatePolicyImport() error = %v", err)
	}

	list, err := s.PolicyImports(ctx, "prod-asia-1")
	if err != nil {
		t.Fatalf("PolicyImports() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("PolicyImports() = %d entries, want 1", len(list))
	}
	if list[0].Role != registry.RoleBaselineCurrent {
		t.Errorf("Role = %q, want BASELINE_CURRENT", list[0].Role)
	}
	// PASTE 来源没有 commit，必须能被识别为未经 Git 核对。
	if list[0].VerifiedAgainstGit() {
		t.Error("a PASTE import reports itself as verified against Git")
	}
}

func TestGitSourcedImportIsVerified(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	p := sampleImport()
	p.ImportID = "imp-git"
	p.Source = registry.SourceGit
	p.GitCommitSHA = "0123456789abcdef0123456789abcdef01234567"
	if err := s.CreatePolicyImport(ctx, actor, p); err != nil {
		t.Fatalf("CreatePolicyImport() error = %v", err)
	}

	list, _ := s.PolicyImports(ctx, "prod-asia-1")
	if len(list) != 1 || !list[0].VerifiedAgainstGit() {
		t.Errorf("git-sourced import not reported as verified: %+v", list)
	}
}

func TestSoftDeletedImportDisappearsButAuditRemains(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.CreatePolicyImport(ctx, actor, sampleImport()); err != nil {
		t.Fatalf("CreatePolicyImport() error = %v", err)
	}
	if err := s.SoftDeletePolicyImport(ctx, actor, "prod-asia-1", "imp-1"); err != nil {
		t.Fatalf("SoftDeletePolicyImport() error = %v", err)
	}

	list, _ := s.PolicyImports(ctx, "prod-asia-1")
	if len(list) != 0 {
		t.Errorf("PolicyImports() = %d entries after delete, want 0", len(list))
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'DELETE_POLICY_IMPORT'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("DELETE_POLICY_IMPORT audit rows = %d, want 1", n)
	}
}

// 往一个未注册的集群里导入，是调用方指错了目标，不是服务故障。
//
// 外键的 1452 在翻译之前会冒泡成 HTTP 500；而这条路径的入口正是运维
// 注册生产集群的那一屏，一次打错集群名不该显示「服务器错误」。
func TestImportIntoUnknownClusterIsNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	p := sampleImport()
	p.ClusterID = "no-such-cluster"
	err := s.CreatePolicyImport(context.Background(), registry.Actor{Username: "admin"}, p)
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound — the cluster does not exist", err)
	}
}

// 来源为 GIT 却没有 commit：入库前拒绝，不落库（spec §4）。
func TestCreatePolicyImportRejectsGitWithoutCommit(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	p := sampleImport()
	p.Source = registry.SourceGit
	if err := s.CreatePolicyImport(ctx, actor, p); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	list, _ := s.PolicyImports(ctx, "prod-asia-1")
	if len(list) != 0 {
		t.Errorf("PolicyImports() = %d entries, want 0 — the rejected import was stored", len(list))
	}
}

// 删除的审计行必须说清删掉的是什么。
//
// before 为 NULL 的审计只能证明「有人删过东西」，而复盘时要回答的是
// 「删掉的是哪一条策略、内容是什么」—— 与 DELETE_CLUSTER 同一条纪律。
func TestDeleteImportAuditRecordsWhatWasDeleted(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.CreatePolicyImport(ctx, actor, sampleImport()); err != nil {
		t.Fatalf("CreatePolicyImport() error = %v", err)
	}
	if err := s.SoftDeletePolicyImport(ctx, actor, "prod-asia-1", "imp-1"); err != nil {
		t.Fatalf("SoftDeletePolicyImport() error = %v", err)
	}

	var before sql.NullString
	if err := db.QueryRow(
		`SELECT before_val FROM audit_log WHERE action = 'DELETE_POLICY_IMPORT'`).Scan(&before); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if !before.Valid {
		t.Fatal("before_val is NULL; the audit row says nothing about what was deleted")
	}
	var got registry.PolicyImport
	if err := json.Unmarshal([]byte(before.String), &got); err != nil {
		t.Fatalf("decode before_val: %v", err)
	}
	if got.ImportID != "imp-1" || got.Namespace != "payment" || got.Name != "allow-gateway" {
		t.Errorf("before_val = %+v, want the deleted import's identity", got)
	}
	if got.SpecHash != "abc" {
		t.Errorf("before_val spec hash = %q, want the deleted content to be identifiable", got.SpecHash)
	}
}

// 删除一条不存在的导入必须报 ErrNotFound，且不留审计行 ——
// 一条描述从未发生过的操作的审计记录，是复盘时最坏的输入。
func TestDeleteMissingImportIsNotFoundAndWritesNoAudit(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	err := s.SoftDeletePolicyImport(ctx, actor, "prod-asia-1", "no-such-import")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'DELETE_POLICY_IMPORT'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("DELETE_POLICY_IMPORT audit rows = %d, want 0", n)
	}
}
