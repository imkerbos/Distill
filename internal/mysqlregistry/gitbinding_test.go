package mysqlregistry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
)

// sampleGitBinding 是一个合法的绑定，只填操作者能提供的字段。
//
// 不填 VerifyResult / VerifiedAt：请求形状里没有它们（design doc
// 2026-08-13 §5），测试因此也从同一个形状出发，否则测的是一条
// 生产上不存在的输入。
//
// 仓库地址与分支不在这里：它们属于 GitRepo（design doc §3.2）。要绑定
// 先得有仓库，调用方用 mustCreateGitRepo 建出 sampleGitRepo。
func sampleGitBinding() registry.GitBinding {
	return registry.GitBinding{
		RepoID:     sampleGitRepo().ID,
		PolicyPath: "clusters/prod-asia-1",
	}
}

// rowValues 把一行读成 列名 → 文本值 的映射。
//
// 查 SELECT * 而不是写死列清单：这个助手服务的问题是「这次写入有没有
// 碰它不该碰的列」，写死清单会让将来新增的列自动逃出断言范围 —— 而
// 那正是「校验一次绑定重写了整行集群」这类回归重新溜进来的入口。
func rowValues(t *testing.T, db *sql.DB, query string, args ...any) map[string]string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %q: %v", query, err)
	}
	if !rows.Next() {
		t.Fatalf("query %q returned no row", query)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan %q: %v", query, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", query, err)
	}
	out := make(map[string]string, len(cols))
	for i, c := range cols {
		// 驱动把字符串类的列交回成 []byte，%v 会把它印成一串十进制数字；
		// 断言失败时那串数字读不出是哪个值，先转成文本。
		if b, isBytes := vals[i].([]byte); isBytes {
			out[c] = string(b)
			continue
		}
		out[c] = fmt.Sprintf("%v", vals[i])
	}
	return out
}

// auditPayload 取一条审计行的前后值，第二个返回值报告 after_val 是否为 NULL。
func auditPayload(t *testing.T, db *sql.DB, action string) (before, after map[string]any) {
	t.Helper()
	var beforeVal, afterVal sql.NullString
	if err := db.QueryRow(
		`SELECT before_val, after_val FROM audit_log WHERE action = ?`, action,
	).Scan(&beforeVal, &afterVal); err != nil {
		t.Fatalf("query audit row for %s: %v", action, err)
	}
	decode := func(v sql.NullString) map[string]any {
		if !v.Valid {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(v.String), &m); err != nil {
			t.Fatalf("decode %s payload %q: %v", action, v.String, err)
		}
		return m
	}
	return decode(beforeVal), decode(afterVal)
}

// 绑定级写入不得改动集群自身的任何字段。这是拆分的核心承诺：
// 校验一次绑定，不该重写整行集群。
func TestSetGitVerifyResultLeavesTheClusterUntouched(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	c := sampleCluster()
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	mustCreateGitRepo(t, s, actor)
	if err := s.BindGitRepo(ctx, actor, c.ID, sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}

	clusterBefore := rowValues(t, db, `SELECT * FROM cluster WHERE cluster_id = ?`, c.ID)
	apiBefore := rowValues(t, db,
		`SELECT * FROM cluster_apiserver WHERE cluster_id = ? ORDER BY host`, c.ID)
	hcBefore := rowValues(t, db,
		`SELECT * FROM cluster_health_check_source WHERE cluster_id = ? ORDER BY cidr`, c.ID)
	bindingBefore := rowValues(t, db,
		`SELECT * FROM cluster_git_binding WHERE cluster_id = ?`, c.ID)

	at := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if err := s.SetGitVerifyResult(ctx, actor, c.ID, registry.BindingVerifyOK, at); err != nil {
		t.Fatalf("SetGitVerifyResult() error = %v", err)
	}

	if got := rowValues(t, db, `SELECT * FROM cluster WHERE cluster_id = ?`, c.ID); !reflect.DeepEqual(got, clusterBefore) {
		t.Errorf("cluster row after SetGitVerifyResult =\n%v\nwant it byte-for-byte unchanged\n%v\n"+
			"—— 跑一次校验重写了集群行，拆分就没有发生", got, clusterBefore)
	}
	if got := rowValues(t, db,
		`SELECT * FROM cluster_apiserver WHERE cluster_id = ? ORDER BY host`, c.ID); !reflect.DeepEqual(got, apiBefore) {
		t.Errorf("cluster_apiserver row = %v, want unchanged %v", got, apiBefore)
	}
	if got := rowValues(t, db,
		`SELECT * FROM cluster_health_check_source WHERE cluster_id = ? ORDER BY cidr`, c.ID); !reflect.DeepEqual(got, hcBefore) {
		t.Errorf("cluster_health_check_source row = %v, want unchanged %v", got, hcBefore)
	}

	// 绑定行上也只有这两列该动：仓库地址、分支、路径都是操作者提供的值，
	// 平台跑一次校验没有理由改写它们。
	bindingAfter := rowValues(t, db, `SELECT * FROM cluster_git_binding WHERE cluster_id = ?`, c.ID)
	for col, want := range bindingBefore {
		if col == "verify_result" || col == "verified_at" {
			continue
		}
		if bindingAfter[col] != want {
			t.Errorf("cluster_git_binding.%s = %q, want it untouched at %q", col, bindingAfter[col], want)
		}
	}
	if bindingAfter["verify_result"] != string(registry.BindingVerifyOK) {
		t.Errorf("verify_result = %q, want %q", bindingAfter["verify_result"], registry.BindingVerifyOK)
	}
	if bindingAfter["verified_at"] == bindingBefore["verified_at"] {
		t.Errorf("verified_at = %q, unchanged from %q —— 结论根本没落库",
			bindingAfter["verified_at"], bindingBefore["verified_at"])
	}
}

// 审计必须说出实际发生的事。原先重新校验写出的是 UPDATE_CLUSTER，
// 前后值是整个集群，事后追溯要从整行 diff 里认。
func TestBindingWritesCarryTheirOwnAuditAction(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	// actor 来自会话，不来自请求体；审计行必须记下的是这个人。
	actor := registry.Actor{Username: "gitops-admin"}

	c := sampleCluster()
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	mustCreateGitRepo(t, s, actor)
	if err := s.BindGitRepo(ctx, actor, c.ID, sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}
	at := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if err := s.SetGitVerifyResult(ctx, actor, c.ID, registry.BindingVerifyOK, at); err != nil {
		t.Fatalf("SetGitVerifyResult() error = %v", err)
	}
	if err := s.UnbindGitRepo(ctx, actor, c.ID); err != nil {
		t.Fatalf("UnbindGitRepo() error = %v", err)
	}

	wantTarget := "git-binding/" + c.ID
	for _, action := range []string{"BIND_GIT_REPO", "VERIFY_GIT_BINDING", "UNBIND_GIT_REPO"} {
		var n int
		var target, who sql.NullString
		if err := db.QueryRow(
			`SELECT COUNT(*), MIN(target), MIN(actor) FROM audit_log WHERE action = ?`, action,
		).Scan(&n, &target, &who); err != nil {
			t.Fatalf("count %s: %v", action, err)
		}
		// 恰好一条：多出来的行意味着某条写路径重复记账，而一次被记了
		// 两次的操作与两次真实操作在复盘时无法区分。
		if n != 1 {
			t.Errorf("%s audit rows = %d, want exactly 1", action, n)
			continue
		}
		if target.String != wantTarget {
			t.Errorf("%s target = %q, want %q", action, target.String, wantTarget)
		}
		if who.String != actor.Username {
			t.Errorf("%s actor = %q, want %q", action, who.String, actor.Username)
		}
	}

	// 三次绑定级写入没有一次冒充集群更新。这一行是本测试的要害：
	// 只数三个新动作各一条，把它们同时也写成 UPDATE_CLUSTER 照样绿。
	var updates int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'UPDATE_CLUSTER'`).Scan(&updates); err != nil {
		t.Fatalf("count UPDATE_CLUSTER: %v", err)
	}
	if updates != 0 {
		t.Errorf("UPDATE_CLUSTER audit rows = %d, want 0 —— "+
			"绑定级写入被记成了一次集群更新，追溯又要回到整行 diff", updates)
	}

	// 绑定这次写的是操作者提供的配置，前值为空（此前未绑定）、后值是新绑定。
	bindBefore, bindAfter := auditPayload(t, db, "BIND_GIT_REPO")
	if bindBefore != nil {
		t.Errorf("BIND_GIT_REPO before_val = %v, want NULL for a first-time binding", bindBefore)
	}
	if bindAfter["repoId"] != sampleGitBinding().RepoID {
		t.Errorf("BIND_GIT_REPO after_val repoId = %v, want %q",
			bindAfter["repoId"], sampleGitBinding().RepoID)
	}

	// 校验这次写的是平台自己的判断，前后值只该有结论与时间 ——
	// 带上整个集群，就等于把「谁改了仓库地址」和「平台跑了一次校验」
	// 重新混回同一种 diff 里。
	verifyBefore, verifyAfter := auditPayload(t, db, "VERIFY_GIT_BINDING")
	if verifyBefore["verifyResult"] != string(registry.BindingVerifyNotVerified) {
		t.Errorf("VERIFY_GIT_BINDING before_val verifyResult = %v, want %q",
			verifyBefore["verifyResult"], registry.BindingVerifyNotVerified)
	}
	if verifyAfter["verifyResult"] != string(registry.BindingVerifyOK) {
		t.Errorf("VERIFY_GIT_BINDING after_val verifyResult = %v, want %q",
			verifyAfter["verifyResult"], registry.BindingVerifyOK)
	}
	for _, leaked := range []string{"podCidr", "displayName", "apiServers", "repoId", "policyPath"} {
		if _, ok := verifyAfter[leaked]; ok {
			t.Errorf("VERIFY_GIT_BINDING after_val carries %q; "+
				"这次写入只动了结论与时间，前后值不该是一个更大的对象", leaked)
		}
	}
}

// 解绑后审计行仍在：那些行记录着这个集群的策略曾经指向哪里。
func TestUnbindKeepsTheAuditTrail(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "gitops-admin"}

	c := sampleCluster()
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	mustCreateGitRepo(t, s, actor)
	if err := s.BindGitRepo(ctx, actor, c.ID, sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}
	if err := s.UnbindGitRepo(ctx, actor, c.ID); err != nil {
		t.Fatalf("UnbindGitRepo() error = %v", err)
	}

	got, ok, err := s.Cluster(ctx, c.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}
	if got.Git != nil {
		t.Errorf("Git = %+v after unbind, want nil", got.Git)
	}

	// 绑定行没了，但「曾经指向哪里」这个问题仍然答得出来。
	_, bindAfter := auditPayload(t, db, "BIND_GIT_REPO")
	if bindAfter["repoId"] != sampleGitBinding().RepoID {
		t.Errorf("BIND_GIT_REPO after_val repoId = %v, want %q —— "+
			"解绑抹掉了这个集群的策略曾经指向哪里", bindAfter["repoId"], sampleGitBinding().RepoID)
	}
	unbindBefore, unbindAfter := auditPayload(t, db, "UNBIND_GIT_REPO")
	if unbindBefore["repoId"] != sampleGitBinding().RepoID {
		t.Errorf("UNBIND_GIT_REPO before_val repoId = %v, want %q",
			unbindBefore["repoId"], sampleGitBinding().RepoID)
	}
	if unbindAfter != nil {
		t.Errorf("UNBIND_GIT_REPO after_val = %v, want NULL", unbindAfter)
	}

	// 再解一次：要解的那个东西不在了。
	if err := s.UnbindGitRepo(ctx, actor, c.ID); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("second UnbindGitRepo() err = %v, want ErrNotFound", err)
	}
}

// 集群写入不再碰绑定行。没有这条，把 insertChildren 里那段绑定 INSERT
// 与 UpdateCluster 里那句 DELETE 放回去，不会有任何测试变红 —— 而放回去
// 意味着一次改网段又会顺手重写绑定，连同它上面的校验结论。
func TestClusterWritesDoNotTouchTheGitBinding(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	c := sampleCluster()
	// 即便调用方在集群对象上带了绑定，集群写入也不该替它落库：
	// 绑定有自己的写入与自己的授权含义。
	c.Git = &registry.GitBinding{RepoID: "repo-wrong", PolicyPath: "wrong"}
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM cluster_git_binding WHERE cluster_id = ?`, c.ID).Scan(&n); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if n != 0 {
		t.Errorf("cluster_git_binding rows after CreateCluster = %d, want 0", n)
	}

	mustCreateGitRepo(t, s, actor)
	if err := s.BindGitRepo(ctx, actor, c.ID, sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}
	at := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if err := s.SetGitVerifyResult(ctx, actor, c.ID, registry.BindingVerifyOK, at); err != nil {
		t.Fatalf("SetGitVerifyResult() error = %v", err)
	}

	u := sampleCluster() // Git 为 nil：请求形状里已经没有这个字段
	u.DisplayName = "Asia Production"
	if err := s.UpdateCluster(ctx, actor, u); err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}

	got, ok, err := s.Cluster(ctx, c.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}
	if got.Git == nil {
		t.Fatal("UpdateCluster() 删掉了绑定：改一次网段不该顺手解绑")
	}
	if got.Git.RepoID != sampleGitBinding().RepoID {
		t.Errorf("repoId = %q, want %q", got.Git.RepoID, sampleGitBinding().RepoID)
	}
	if got.Git.VerifyResult != registry.BindingVerifyOK {
		t.Errorf("VerifyResult = %q, want %q —— 一次集群更新清掉了校验结论",
			got.Git.VerifyResult, registry.BindingVerifyOK)
	}
}

// 绑定到一个不存在的集群是调用方的问题，不是服务故障；
// 而一个填一半的绑定在轮 4 会变成一次指向不存在路径的写入尝试。
func TestBindGitRepoRejectsMissingClusterAndInvalidBinding(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	err := s.BindGitRepo(ctx, actor, "no-such-cluster", sampleGitBinding())
	if !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("BindGitRepo() on a missing cluster err = %v, want ErrNotFound", err)
	}

	c := sampleCluster()
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	mustCreateGitRepo(t, s, actor)
	half := sampleGitBinding()
	half.PolicyPath = ""
	if err := s.BindGitRepo(ctx, actor, c.ID, half); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("BindGitRepo() with a half-filled binding err = %v, want ErrInvalid", err)
	}

	// 绑到一个没登记过的仓库：外键之外还有软删除，判定由 requireLiveRepo
	// 给出，因此这里问的是 ErrNotFound 而不是一条驱动错误。
	ghost := sampleGitBinding()
	ghost.RepoID = "repo-never-registered"
	if err := s.BindGitRepo(ctx, actor, c.ID, ghost); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("BindGitRepo() to an unregistered repo err = %v, want ErrNotFound", err)
	}

	// 结论是封闭枚举：自由文本落进库里，统计口径就此失去意义。
	if err := s.SetGitVerifyResult(ctx, actor, c.ID, registry.BindingVerifyResult("LOOKS_FINE"),
		time.Now().UTC()); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("SetGitVerifyResult() with an unregistered result err = %v, want ErrInvalid", err)
	}

	// 没有绑定却要写结论：要写的那个东西不在。
	if err := s.SetGitVerifyResult(ctx, actor, c.ID, registry.BindingVerifyOK,
		time.Now().UTC()); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("SetGitVerifyResult() without a binding err = %v, want ErrNotFound", err)
	}
}
