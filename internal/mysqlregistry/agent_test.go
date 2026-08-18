package mysqlregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
)

// sampleAgent 返回一条挂在 sampleCluster() 上的 agent 记录。
func sampleAgent(agentID string) registry.ClusterAgent {
	return registry.ClusterAgent{
		ClusterID: "prod-asia-1",
		AgentID:   agentID,
		TokenHash: []byte("0123456789abcdef0123456789abcdef"), // 32 字节
		State:     registry.AgentActive,
		CreatedBy: "admin",
	}
}

// seedCluster 建出 agent 要挂靠的那个集群。
func seedCluster(t *testing.T, s *mysqlregistry.Store) {
	t.Helper()
	if err := s.CreateCluster(context.Background(),
		registry.Actor{Username: "admin"}, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
}

func TestIssueClusterAgentThenLookItUpByID(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)

	want := sampleAgent("0011223344556677")
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, want); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}

	got, ok, err := s.ClusterAgentByID(ctx, want.AgentID)
	if err != nil || !ok {
		t.Fatalf("ClusterAgentByID() = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	// 这一个字段是整条认证链的落点：摄入时的集群归属只来自它，取错等于
	// 把一个集群的 Pod 写进另一个集群的身份表（CLAUDE.md §4）。
	if got.ClusterID != want.ClusterID {
		t.Errorf("ClusterID = %q, want %q", got.ClusterID, want.ClusterID)
	}
	if string(got.TokenHash) != string(want.TokenHash) {
		t.Errorf("TokenHash round-tripped as %q, want %q — 哈希存取不一致会让"+
			"比对恒不相等，症状与「token 错了」一模一样", got.TokenHash, want.TokenHash)
	}
	if got.State != registry.AgentActive {
		t.Errorf("State = %q, want ACTIVE", got.State)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if !got.LastSeenAt.IsZero() || !got.RevokedAt.IsZero() {
		t.Errorf("LastSeenAt = %v, RevokedAt = %v; want both zero — 「还没发生过」"+
			"不该渲染成某个时刻", got.LastSeenAt, got.RevokedAt)
	}
}

func TestIssueClusterAgentRejectsAnInvalidRecord(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)

	bad := sampleAgent("0011223344556677")
	bad.TokenHash = []byte("short")
	err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, bad)
	if err == nil {
		t.Fatal("IssueClusterAgent(short hash) = nil — 校验必须在入库前拦住，" +
			"否则一条永远认不过的记录会静静躺在库里")
	}
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
}

func TestRevokedAgentStaysLookupable(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)
	actor := registry.Actor{Username: "admin"}

	a := sampleAgent("8877665544332211")
	if err := s.IssueClusterAgent(ctx, actor, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	if err := s.RevokeClusterAgent(ctx, actor, a.ClusterID, a.AgentID); err != nil {
		t.Fatalf("RevokeClusterAgent() error = %v", err)
	}

	// 吊销**不删行**：认证层要能分辨「这把 token 被吊销了」与「这把 token
	// 从来不存在」。前者意味着有人正在用一把该扔的凭据 —— 那是要被看见
	// 的信号；删了行就只剩「未知 token」，与打错字没有区别。
	got, ok, err := s.ClusterAgentByID(ctx, a.AgentID)
	if err != nil || !ok {
		t.Fatalf("ClusterAgentByID(revoked) = (_, %v, %v), want still found", ok, err)
	}
	if got.State != registry.AgentRevoked {
		t.Errorf("State = %q, want REVOKED", got.State)
	}
	if got.RevokedAt.IsZero() {
		t.Error("RevokedAt is zero — 吊销时刻是审计要回答的那个「什么时候」")
	}
}

func TestRevokeIsNotFoundWhenNothingWasRevoked(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)
	actor := registry.Actor{Username: "admin"}

	if err := s.RevokeClusterAgent(ctx, actor, "prod-asia-1", "ffffffffffffffff"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RevokeClusterAgent(unknown) = %v, want ErrNotFound", err)
	}

	a := sampleAgent("1122334455667788")
	if err := s.IssueClusterAgent(ctx, actor, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	if err := s.RevokeClusterAgent(ctx, actor, a.ClusterID, a.AgentID); err != nil {
		t.Fatalf("first revoke error = %v", err)
	}
	// 第二次吊销必须答「没有可吊销的」而不是静默成功：静默成功会让一次
	// 吊销错对象的操作看起来生效了。
	if err := s.RevokeClusterAgent(ctx, actor, a.ClusterID, a.AgentID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("second revoke = %v, want ErrNotFound", err)
	}
}

func TestRevokeWillNotCrossClusterBoundaries(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)
	actor := registry.Actor{Username: "admin"}

	a := sampleAgent("aabbccddeeff0011")
	if err := s.IssueClusterAgent(ctx, actor, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	// agent_id 全局唯一，但吊销仍然按 (cluster_id, agent_id) 定位：少了
	// cluster_id 那一半，一个管理员就能吊销别的集群的 agent，而界面上
	// 看起来他只是在操作自己那个集群。
	if err := s.RevokeClusterAgent(ctx, actor, "prod-eu-1", a.AgentID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("cross-cluster revoke = %v, want ErrNotFound", err)
	}
	got, _, _ := s.ClusterAgentByID(ctx, a.AgentID)
	if got.State != registry.AgentActive {
		t.Errorf("State = %q after a cross-cluster revoke, want it untouched", got.State)
	}
}

func TestClusterAgentsListsRevokedOnesToo(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)
	actor := registry.Actor{Username: "admin"}

	for _, id := range []string{"1111111111111111", "2222222222222222"} {
		if err := s.IssueClusterAgent(ctx, actor, sampleAgent(id)); err != nil {
			t.Fatalf("IssueClusterAgent(%s) error = %v", id, err)
		}
	}
	if err := s.RevokeClusterAgent(ctx, actor, "prod-asia-1", "1111111111111111"); err != nil {
		t.Fatalf("RevokeClusterAgent() error = %v", err)
	}

	// 含已吊销的：操作者要看得见「这个集群历史上签过几把」。只显示活的
	// 会让一次忘记吊销无从发现。
	agents, err := s.ClusterAgents(ctx, "prod-asia-1")
	if err != nil {
		t.Fatalf("ClusterAgents() error = %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("ClusterAgents() returned %d agents, want 2", len(agents))
	}
}

func TestClusterAgentsAreScopedToTheirCluster(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"},
		sampleAgent("3333333333333333")); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	agents, err := s.ClusterAgents(ctx, "prod-eu-1")
	if err != nil {
		t.Fatalf("ClusterAgents() error = %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("ClusterAgents(other cluster) returned %d agents, want 0", len(agents))
	}
}

func TestIssueAndRevokeWriteAuditInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	seedCluster(t, s)
	actor := registry.Actor{Username: "admin"}

	a := sampleAgent("4444444444444444")
	if err := s.IssueClusterAgent(ctx, actor, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	if err := s.RevokeClusterAgent(ctx, actor, a.ClusterID, a.AgentID); err != nil {
		t.Fatalf("RevokeClusterAgent() error = %v", err)
	}

	// 签发一把能往平台写数据的凭据，与吊销它，都是权限变更（规范 §28）。
	// 审计与业务写入同事务（V4 spec §9.9）：状态变了而审计缺失，正是事后
	// 最需要追溯的那一次。
	for _, action := range []string{"ISSUE_CLUSTER_AGENT", "REVOKE_CLUSTER_AGENT"} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM audit_log WHERE action = ? AND cluster_id = ?`,
			action, a.ClusterID).Scan(&n); err != nil {
			t.Fatalf("count audit for %s: %v", action, err)
		}
		if n != 1 {
			t.Errorf("audit rows for %s = %d, want 1", action, n)
		}
	}
}

func TestAuditNeverCarriesTheTokenHash(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	seedCluster(t, s)

	a := sampleAgent("5555555555555555")
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}

	// 审计记的是「谁给哪个集群签了哪个 agent_id」。agent_id 是公开段，
	// 哈希不是 —— 它是离线爆破的输入，而审计表是长期留存并会被导出的
	// （规范 §19、§21、§43）。
	rows, err := db.Query(`SELECT target, COALESCE(before_val, ''), COALESCE(after_val, '')
	                         FROM audit_log WHERE action = 'ISSUE_CLUSTER_AGENT'`)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var target, before, after string
		if err := rows.Scan(&target, &before, &after); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, field := range []string{target, before, after} {
			if len(field) > 0 && contains(field, string(a.TokenHash)) {
				t.Errorf("audit row carried the token hash: %q", field)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestTouchRecordsLastSeenWithoutChangingState(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)

	a := sampleAgent("6666666666666666")
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	at := time.Date(2026, 8, 18, 9, 30, 0, 0, time.UTC)
	if err := s.TouchClusterAgent(ctx, a.AgentID, at); err != nil {
		t.Fatalf("TouchClusterAgent() error = %v", err)
	}

	got, ok, err := s.ClusterAgentByID(ctx, a.AgentID)
	if err != nil || !ok {
		t.Fatalf("ClusterAgentByID() = (_, %v, %v)", ok, err)
	}
	if !got.LastSeenAt.Equal(at) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, at)
	}
	if got.State != registry.AgentActive {
		t.Errorf("State = %q after Touch, want it untouched", got.State)
	}
}

func TestTouchDoesNotWriteAudit(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	seedCluster(t, s)

	a := sampleAgent("7777777777777777")
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	if err := s.TouchClusterAgent(ctx, a.AgentID, time.Now().UTC()); err != nil {
		t.Fatalf("TouchClusterAgent() error = %v", err)
	}

	// 每一次成功的摄入都会 Touch 一次。给它写审计等于让审计表按摄入频率
	// 增长，把「谁做了什么」淹掉（规范 §43 要的是可复盘的链条，不是流水）。
	// 只数这个 agent 的审计行：建集群本身也写审计，把它算进来会让这条
	// 断言测的是别的东西。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE target = ?`,
		"cluster-agent/"+a.AgentID).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Errorf("audit rows for this agent = %d, want 1 (the issue only)", n)
	}
}

// 编译期确认新方法进了接口：漏加会让装配处在运行时才发现。
var _ interface {
	IssueClusterAgent(context.Context, registry.Actor, registry.ClusterAgent) error
	RevokeClusterAgent(context.Context, registry.Actor, string, string) error
	ClusterAgents(context.Context, string) ([]registry.ClusterAgent, error)
	ClusterAgentByID(context.Context, string) (registry.ClusterAgent, bool, error)
	TouchClusterAgent(context.Context, string, time.Time) error
} = (*mysqlregistry.Store)(nil)

var _ = sql.ErrNoRows
