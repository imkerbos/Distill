package mysqlregistry_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
)

func sampleOverride() registry.RuleOverride {
	return registry.RuleOverride{
		ClusterID: "prod-asia-1", Namespace: "batch", Workload: "worker",
		Fingerprint: strings.Repeat("a", 64),
		Decision:    policygen.DecisionEnable,
		Reason:      "已确认是对账任务",
		DecidedBy:   "admin", DecidedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestCreateAndListRuleOverrides(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.CreateRuleOverride(ctx, actor, sampleOverride()); err != nil {
		t.Fatalf("CreateRuleOverride() error = %v", err)
	}

	list, err := s.RuleOverrides(ctx, "prod-asia-1")
	if err != nil {
		t.Fatalf("RuleOverrides() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("RuleOverrides() = %d entries, want 1", len(list))
	}
	if list[0].Decision != policygen.DecisionEnable || list[0].Reason != "已确认是对账任务" {
		t.Errorf("round trip lost data: %+v", list[0])
	}
	if list[0].MergedCommitSHA != "" {
		t.Errorf("MergedCommitSHA = %q, want empty in this round", list[0].MergedCommitSHA)
	}
}

// 这是本包存在的理由：审计与业务写入同生共死。
func TestOverrideAuditRollsBackWithTheWrite(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	// 不建集群，外键必然失败。
	if err := s.CreateRuleOverride(ctx, actor, sampleOverride()); err == nil {
		t.Fatal("CreateRuleOverride() succeeded without its cluster, want an error")
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'CREATE_RULE_OVERRIDE'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("audit rows = %d, want 0 — a failed write must leave no audit row", n)
	}
}

// 人改主意是正常的；两条互相矛盾的决定并存会让「这条规则到底开不开」
// 没有答案。重复决定覆盖旧值，旧值进审计。
func TestRepeatedDecisionOverwritesAndAudits(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	first := sampleOverride()
	if err := s.CreateRuleOverride(ctx, actor, first); err != nil {
		t.Fatalf("first CreateRuleOverride() error = %v", err)
	}
	second := first
	second.Decision = policygen.DecisionDisable
	second.Reason = "改主意了：这条实际上不该放行"
	if err := s.CreateRuleOverride(ctx, actor, second); err != nil {
		t.Fatalf("second CreateRuleOverride() error = %v", err)
	}

	list, _ := s.RuleOverrides(ctx, "prod-asia-1")
	if len(list) != 1 {
		t.Fatalf("RuleOverrides() = %d entries, want 1 after the overwrite", len(list))
	}
	if list[0].Decision != policygen.DecisionDisable {
		t.Errorf("Decision = %q, want the second decision", list[0].Decision)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'CREATE_RULE_OVERRIDE'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("audit rows = %d, want 2 — both decisions must be on the record", n)
	}
}

func TestSoftDeletedOverrideDisappearsButAuditRemains(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	o := sampleOverride()
	if err := s.CreateRuleOverride(ctx, actor, o); err != nil {
		t.Fatalf("CreateRuleOverride() error = %v", err)
	}
	if err := s.SoftDeleteRuleOverride(
		ctx, actor, o.ClusterID, o.Namespace, o.Workload, o.Fingerprint); err != nil {
		t.Fatalf("SoftDeleteRuleOverride() error = %v", err)
	}

	list, _ := s.RuleOverrides(ctx, "prod-asia-1")
	if len(list) != 0 {
		t.Errorf("RuleOverrides() = %d entries after delete, want 0", len(list))
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'DELETE_RULE_OVERRIDE'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("DELETE_RULE_OVERRIDE audit rows = %d, want 1", n)
	}
}

func TestSoftDeleteMissingOverrideIsNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	err := s.SoftDeleteRuleOverride(
		ctx, actor, "prod-asia-1", "batch", "worker", strings.Repeat("b", 64))
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
