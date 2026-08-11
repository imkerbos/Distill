package mysqlregistry_test

import (
	"context"
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
