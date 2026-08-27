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

// 下线集群之后，这个集群的 agent 记录必须同时说出两件事：
// 凭据已吊销，且它所属的集群已经不在了。
//
// 两件事都要，而且要在**同一次查询**里拿到：认证路径只查一次库
// （ClusterAgentByID 的注释），为了问一句"这个集群还在吗"再查一次，
// 等于在最热的那条链上多一次往返。
//
// 用真库而不是内存假实现：这一位是 LEFT JOIN 算出来的，而 join 写错
// 在假实现上永远测不出来。
func TestRetiringAClusterRevokesItsAgentsAndMarksThemRetired(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)

	a := sampleAgent("aabbccddeeff0011")
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	before, ok, err := s.ClusterAgentByID(ctx, a.AgentID)
	if err != nil || !ok {
		t.Fatalf("ClusterAgentByID() = (_, %v, %v)", ok, err)
	}
	if before.ClusterRetired {
		t.Fatal("一个还在册的集群被报成已下线，这条用例的前提不成立")
	}

	if err := s.SoftDeleteCluster(ctx, registry.Actor{Username: "admin"}, a.ClusterID); err != nil {
		t.Fatalf("SoftDeleteCluster() error = %v", err)
	}

	got, ok, err := s.ClusterAgentByID(ctx, a.AgentID)
	if err != nil {
		t.Fatalf("ClusterAgentByID() after retiring = %v", err)
	}
	// 记录本身必须还查得到：查不到在认证层是「未知 agent」，与「集群已下线」
	// 是两条不同的日志，合并之后就再也分不出是哪一种。
	if !ok {
		t.Fatal("下线之后 agent 记录整个查不到了；认证层需要它才能分辨拒绝的成因")
	}
	if !got.ClusterRetired {
		t.Error("ClusterRetired = false，但它所属的集群已经下线 —— " +
			"认证层据这一位拒绝，它为假就等于这个集群还在收数据")
	}
	if got.State != registry.AgentRevoked {
		t.Errorf("State = %q, want %q —— 下线之后这个集群再也打不开 agent 面板，"+
			"留着 ACTIVE 的凭据等于留下一批谁也看不见、谁也吊销不了的 token",
			got.State, registry.AgentRevoked)
	}
	if got.RevokedAt.IsZero() {
		t.Error("吊销了却没有吊销时刻：事后答不出这批凭据是什么时候失效的")
	}
}

// 已经吊销过的 token 不该被下线再改一次时刻。
//
// 下线那条 UPDATE 带 state = ACTIVE 的条件，正是为此：不带的话，
// 一把三个月前就被吊销的 token 会在今天被重新盖上今天的吊销时刻，
// 而那个时刻是编出来的。
func TestRetiringAClusterLeavesAlreadyRevokedTokensAlone(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	seedCluster(t, s)

	a := sampleAgent("1122334455667788")
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	if err := s.RevokeClusterAgent(ctx, registry.Actor{Username: "admin"},
		a.ClusterID, a.AgentID); err != nil {
		t.Fatalf("RevokeClusterAgent() error = %v", err)
	}
	revoked, _, err := s.ClusterAgentByID(ctx, a.AgentID)
	if err != nil {
		t.Fatalf("ClusterAgentByID() = %v", err)
	}

	if err := s.SoftDeleteCluster(ctx, registry.Actor{Username: "admin"}, a.ClusterID); err != nil {
		t.Fatalf("SoftDeleteCluster() error = %v", err)
	}
	after, _, err := s.ClusterAgentByID(ctx, a.AgentID)
	if err != nil {
		t.Fatalf("ClusterAgentByID() after retiring = %v", err)
	}
	if !after.RevokedAt.Equal(revoked.RevokedAt) {
		t.Errorf("RevokedAt 被下线改写了：%v → %v。已经吊销过的凭据，"+
			"它失效的那一刻是一个事实，不该被后来的动作盖掉",
			revoked.RevokedAt, after.RevokedAt)
	}
}

// 集群行被硬删之后，agent 记录仍然要查得到，并且报「集群没了」。
//
// 这不是假想：迁移、清理脚本、以及任何绕过软删的运维动作都会留下这种孤儿
// 记录。它必须与「未知 agent」分得开 —— 认证层对外只回一句话，但日志里
// 那两条指向完全不同的处置：一条是有人拿着一把不存在的凭据在试，另一条是
// 我们自己的库里躺着一批该被清掉的记录。
//
// 这一条正是 ClusterAgentByID 用 LEFT JOIN 而不是 INNER 的理由：INNER 会
// 让这行整个查不到，于是它在认证层变成「未知 agent」。
func TestAnAgentSurvivesItsClusterRowBeingDeleted(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	seedCluster(t, s)

	a := sampleAgent("99887766554433aa")
	if err := s.IssueClusterAgent(ctx, registry.Actor{Username: "admin"}, a); err != nil {
		t.Fatalf("IssueClusterAgent() error = %v", err)
	}
	// 硬删集群行，绕开软删 —— 模拟一次清理之后留下的孤儿记录。
	//
	// 先删有外键指过来的子表。**cluster_agent 不在其中**：它没有指向
	// cluster 的外键，所以这种孤儿记录是数据库允许存在的，而不是一个
	// 只能靠想象构造出来的情形。
	// 表名写死在各自的语句里，不拼字符串：拼接是 SQL 注入的形状，
	// 即使这里的输入来自一个字面量数组。
	for _, stmt := range []string{
		`DELETE FROM cluster_apiserver WHERE cluster_id = ?`,
		`DELETE FROM cluster_health_check_source WHERE cluster_id = ?`,
	} {
		if _, err := db.ExecContext(ctx, stmt, a.ClusterID); err != nil {
			t.Fatalf("delete child rows: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM cluster WHERE cluster_id = ?`, a.ClusterID); err != nil {
		t.Fatalf("delete the cluster row: %v", err)
	}

	got, ok, err := s.ClusterAgentByID(ctx, a.AgentID)
	if err != nil {
		t.Fatalf("ClusterAgentByID() = %v", err)
	}
	if !ok {
		t.Fatal("集群行没了之后 agent 记录整个查不到；它在认证层会变成" +
			"「未知 agent」，而那是另一件事")
	}
	if !got.ClusterRetired {
		t.Error("ClusterRetired = false，但它所属的集群行根本不存在了")
	}
}
