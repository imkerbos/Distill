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

// sampleGitRepo 是一个合法的策略仓库，只填操作者能提供的字段。
//
// 不填 VerifyResult / VerifiedAt：请求形状里没有它们（design doc
// 2026-08-13 §4），测试因此也从同一个形状出发。
func sampleGitRepo() registry.GitRepo {
	return registry.GitRepo{
		ID:     "repo-policies",
		URL:    "ssh://git@gitlab.example.com/net/policies.git",
		Branch: "main",
	}
}

// mustCreateGitRepo 登记 sampleGitRepo，供需要一个可绑定仓库的测试调用。
//
// 绑定不再自带仓库地址（design doc §3.2），因此每个绑定测试都要先有一个
// 仓库；这一步失败就没有必要继续，用 Fatalf 而不是返回 error。
func mustCreateGitRepo(t *testing.T, s *mysqlregistry.Store, actor registry.Actor) registry.GitRepo {
	t.Helper()
	r := sampleGitRepo()
	if err := s.CreateGitRepo(context.Background(), actor, r); err != nil {
		t.Fatalf("CreateGitRepo() error = %v", err)
	}
	return r
}

// 仓库是独立实体：登记之后读得回来，并且带着自己的审计动作与 target。
func TestCreateAndReadBackGitRepo(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "gitops-admin"}

	in := mustCreateGitRepo(t, s, actor)

	got, ok, err := s.GitRepo(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("GitRepo() = %+v, %v, %v", got, ok, err)
	}
	// 刚登记、还没跑过校验：零值 "" 不是登记过的枚举值，落库时被写成
	// NOT_VERIFIED，读回来因此是它而不是空串。
	want := in
	want.VerifyResult = registry.RepoVerifyNotVerified
	if got != want {
		t.Errorf("round-tripped repo = %+v, want %+v", got, want)
	}
	// VerifiedAt 为 NULL 必须读成 nil：零值 time.Time 是 1970 年的一个真实
	// 时间点，新鲜度检查会把它当成「校验过」放行。
	if got.VerifiedAt != nil {
		t.Errorf("VerifiedAt = %v, want nil for a never-verified repo", got.VerifiedAt)
	}

	list, err := s.GitRepos(ctx)
	if err != nil {
		t.Fatalf("GitRepos() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != in.ID {
		t.Errorf("GitRepos() = %+v, want exactly the one registered repo", list)
	}

	var n int
	var target, clusterID sql.NullString
	if err := db.QueryRow(
		`SELECT COUNT(*), MIN(target), MIN(cluster_id) FROM audit_log WHERE action = 'CREATE_GIT_REPO'`,
	).Scan(&n, &target, &clusterID); err != nil {
		t.Fatalf("count CREATE_GIT_REPO: %v", err)
	}
	if n != 1 {
		t.Errorf("CREATE_GIT_REPO audit rows = %d, want exactly 1", n)
	}
	if target.String != "git-repo/"+in.ID {
		t.Errorf("CREATE_GIT_REPO target = %q, want %q", target.String, "git-repo/"+in.ID)
	}
	// 仓库不属于任何集群：审计行的 cluster_id 必须是那个撞不上真实集群的
	// 空串，否则它会与某个集群的审计混在 idx_cluster_time 上读不开。
	if clusterID.String != "" {
		t.Errorf("CREATE_GIT_REPO cluster_id = %q, want empty —— 仓库不属于任何集群",
			clusterID.String)
	}
}

// UNIQUE 保证 1:1（design doc §3.2）。不加它，模型默默允许 N:1，而 N:1 会
// 让「每绑定一把凭据」失去隔离意义 —— 同一个仓库上 N 把 deploy key，泄漏
// 任何一把的爆炸半径都是整个仓库。
//
// 判定必须由约束给出，不由一句应用层检查给出：应用层的检查在两条并发
// 请求之间不成立，而这条测试要守住的正是「库本身不允许」。
func TestOneRepoCannotBeBoundToTwoClusters(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "gitops-admin"}

	first := sampleCluster()
	if err := s.CreateCluster(ctx, actor, first); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	second := sampleCluster()
	second.ID = "prod-eu-1"
	second.DisplayName = "EU Prod"
	if err := s.CreateCluster(ctx, actor, second); err != nil {
		t.Fatalf("second CreateCluster() error = %v", err)
	}
	repo := mustCreateGitRepo(t, s, actor)

	if err := s.BindGitRepo(ctx, actor, first.ID, sampleGitBinding()); err != nil {
		t.Fatalf("first BindGitRepo() error = %v", err)
	}

	poach := sampleGitBinding()
	poach.PolicyPath = "clusters/prod-eu-1"
	err := s.BindGitRepo(ctx, actor, second.ID, poach)
	// 选错仓库是操作者的输入问题，不是服务故障：返回 500 会让这次手滑
	// 计进服务可用性，而页面只会说「服务器错误」。
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("BindGitRepo() of an already-bound repo err = %v, want ErrInvalid —— "+
			"仓库与集群是 1:1，第二个集群绑上同一个仓库必须失败", err)
	}

	// 第一个集群的绑定必须原封不动。这一行守的是另一件事：
	// INSERT ... ON DUPLICATE KEY UPDATE 撞上 repo_id 的唯一键时，改的是
	// **第一个集群**那一行而不是报错 —— 那样上面的断言会红，但如果哪天
	// 有人把错误吞掉，这里能说出真正发生了什么。
	row := rowValues(t, db, `SELECT * FROM cluster_git_binding WHERE cluster_id = ?`, first.ID)
	if row["policy_path"] != sampleGitBinding().PolicyPath {
		t.Errorf("cluster_git_binding.policy_path for %s = %q, want it untouched at %q —— "+
			"一次失败的绑定改写了另一个集群的绑定", first.ID, row["policy_path"],
			sampleGitBinding().PolicyPath)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM cluster_git_binding WHERE repo_id = ?`, repo.ID).Scan(&n); err != nil {
		t.Fatalf("count bindings on the repo: %v", err)
	}
	if n != 1 {
		t.Errorf("bindings on repo %s = %d, want exactly 1", repo.ID, n)
	}
}

// 仍被绑定的仓库不得删除：级联会让一次仓库清理静默解除某个集群的
// 策略下发路径 —— 没有报错、没有人做过「这个集群不再下发策略」的决定，
// 下一次推荐照常产出，只是再也没有地方接收它（design doc §4）。
func TestDeletingABoundRepoIsRefused(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "gitops-admin"}

	c := sampleCluster()
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	repo := mustCreateGitRepo(t, s, actor)
	if err := s.BindGitRepo(ctx, actor, c.ID, sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}

	if err := s.SoftDeleteGitRepo(ctx, actor, repo.ID); !errors.Is(err, registry.ErrRepoInUse) {
		t.Fatalf("SoftDeleteGitRepo() on a bound repo err = %v, want ErrRepoInUse", err)
	}

	// 拒绝之后两边都必须还在。少了这两条，把实现改成「删仓库时顺手解绑」
	// 再返回 ErrRepoInUse，上面那行照样绿。
	var bindings int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM cluster_git_binding WHERE repo_id = ?`, repo.ID).Scan(&bindings); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if bindings != 1 {
		t.Errorf("bindings on the refused repo = %d, want 1 —— "+
			"删除仓库静默解除了这个集群的策略下发路径", bindings)
	}
	if _, ok, err := s.GitRepo(ctx, repo.ID); err != nil || !ok {
		t.Errorf("GitRepo() after a refused delete = %v, %v; want the repo still live", ok, err)
	}
	// 被拒绝的删除不得留下审计行：审计里的一条 DELETE_GIT_REPO 会说
	// 一件没发生过的事。
	var audits int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'DELETE_GIT_REPO'`).Scan(&audits); err != nil {
		t.Fatalf("count DELETE_GIT_REPO: %v", err)
	}
	if audits != 0 {
		t.Errorf("DELETE_GIT_REPO audit rows after a refused delete = %d, want 0", audits)
	}

	// 解绑之后才删得掉：拒绝的是「仍被绑定」，不是「删除」本身。
	if err := s.UnbindGitRepo(ctx, actor, c.ID); err != nil {
		t.Fatalf("UnbindGitRepo() error = %v", err)
	}
	if err := s.SoftDeleteGitRepo(ctx, actor, repo.ID); err != nil {
		t.Fatalf("SoftDeleteGitRepo() on an unbound repo error = %v", err)
	}
	if _, ok, err := s.GitRepo(ctx, repo.ID); err != nil || ok {
		t.Errorf("GitRepo() after delete = %v, %v; want it gone", ok, err)
	}
}

// 软删除留着行，外键因此照样满足 —— 挡不住把集群绑到一个已经删掉的
// 仓库上，而它的地址与凭据从此没人维护。判定归 requireLiveRepo。
func TestSoftDeletedRepoCannotBeBound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "gitops-admin"}

	c := sampleCluster()
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	repo := mustCreateGitRepo(t, s, actor)
	if err := s.SoftDeleteGitRepo(ctx, actor, repo.ID); err != nil {
		t.Fatalf("SoftDeleteGitRepo() error = %v", err)
	}

	if err := s.BindGitRepo(ctx, actor, c.ID, sampleGitBinding()); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("BindGitRepo() to a soft-deleted repo err = %v, want ErrNotFound", err)
	}
}

// 仓库级校验的结论与时间往返，且写的是仓库自己的审计动作 ——
// 一次仓库级校验不该冒充一次配置变更，也不该冒充路径级校验。
func TestSetGitRepoVerifyResultRoundTripsAndAuditsItself(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "gitops-admin"}

	repo := mustCreateGitRepo(t, s, actor)
	at := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if err := s.SetGitRepoVerifyResult(ctx, actor, repo.ID, registry.RepoVerifyAuthFailed, at); err != nil {
		t.Fatalf("SetGitRepoVerifyResult() error = %v", err)
	}

	got, ok, err := s.GitRepo(ctx, repo.ID)
	if err != nil || !ok {
		t.Fatalf("GitRepo() = %+v, %v, %v", got, ok, err)
	}
	if got.VerifyResult != registry.RepoVerifyAuthFailed {
		t.Errorf("VerifyResult = %q, want %q", got.VerifyResult, registry.RepoVerifyAuthFailed)
	}
	if got.VerifiedAt == nil || !got.VerifiedAt.Equal(at) {
		t.Errorf("VerifiedAt = %v, want %v", got.VerifiedAt, at)
	}
	// 跑一次校验不是一次配置变更：地址与分支不该被改写。
	if got.URL != repo.URL || got.Branch != repo.Branch {
		t.Errorf("repo after verify = %+v, want url/branch untouched at %q/%q",
			got, repo.URL, repo.Branch)
	}

	verifyBefore, verifyAfter := auditPayload(t, db, "VERIFY_GIT_REPO")
	if verifyBefore["verifyResult"] != string(registry.RepoVerifyNotVerified) {
		t.Errorf("VERIFY_GIT_REPO before_val verifyResult = %v, want %q",
			verifyBefore["verifyResult"], registry.RepoVerifyNotVerified)
	}
	if verifyAfter["verifyResult"] != string(registry.RepoVerifyAuthFailed) {
		t.Errorf("VERIFY_GIT_REPO after_val verifyResult = %v, want %q",
			verifyAfter["verifyResult"], registry.RepoVerifyAuthFailed)
	}
	// 前后值只该有结论与时间：带上整个仓库，追溯的人又要在一个更大的
	// 对象里认「哪几个字段真的变了」。
	for _, leaked := range []string{"repoUrl", "branch", "credentialRef"} {
		if _, ok := verifyAfter[leaked]; ok {
			t.Errorf("VERIFY_GIT_REPO after_val carries %q; 这次写入只动了结论与时间", leaked)
		}
	}

	// 封闭枚举：自由文本落进库里，统计口径当场失去意义。
	if err := s.SetGitRepoVerifyResult(ctx, actor, repo.ID,
		registry.RepoVerifyResult("LOOKS_FINE"), at); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("SetGitRepoVerifyResult() with an unregistered result err = %v, want ErrInvalid", err)
	}
	// 路径级的取值不属于这一层，类型挡得住写法，运行期的字符串挡不住。
	if err := s.SetGitRepoVerifyResult(ctx, actor, repo.ID,
		registry.RepoVerifyResult("PATH_MISSING"), at); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("SetGitRepoVerifyResult() with a path-level verdict err = %v, want ErrInvalid", err)
	}
	// 仓库不在：要写结论的那个东西不存在。
	if err := s.SetGitRepoVerifyResult(ctx, actor, "repo-nope",
		registry.RepoVerifyOK, at); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("SetGitRepoVerifyResult() on a missing repo err = %v, want ErrNotFound", err)
	}
}

// 改仓库会清掉上一次的校验结论：换了地址之后，旧的 OK 描述的是另一个
// 仓库，留着它就是拿一句关于别处的判断给新地址背书。
func TestUpdateGitRepoClearsTheStaleVerdict(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "gitops-admin"}

	repo := mustCreateGitRepo(t, s, actor)
	at := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if err := s.SetGitRepoVerifyResult(ctx, actor, repo.ID, registry.RepoVerifyOK, at); err != nil {
		t.Fatalf("SetGitRepoVerifyResult() error = %v", err)
	}

	moved := repo
	moved.URL = "ssh://git@gitlab.example.com/net/policies-v2.git"
	if err := s.UpdateGitRepo(ctx, actor, moved); err != nil {
		t.Fatalf("UpdateGitRepo() error = %v", err)
	}

	got, ok, err := s.GitRepo(ctx, repo.ID)
	if err != nil || !ok {
		t.Fatalf("GitRepo() = %+v, %v, %v", got, ok, err)
	}
	if got.URL != moved.URL {
		t.Errorf("URL = %q, want %q", got.URL, moved.URL)
	}
	if got.VerifyResult != registry.RepoVerifyNotVerified || got.VerifiedAt != nil {
		t.Errorf("verdict after a repo edit = %q/%v, want NOT_VERIFIED/nil —— "+
			"旧结论描述的是改之前那个地址", got.VerifyResult, got.VerifiedAt)
	}

	if err := s.UpdateGitRepo(ctx, actor, registry.GitRepo{
		ID: "repo-nope", URL: repo.URL, Branch: repo.Branch,
	}); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("UpdateGitRepo() on a missing repo err = %v, want ErrNotFound", err)
	}
}
