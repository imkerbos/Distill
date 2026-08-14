package mysqlregistry

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/registry"
)

// 本文件是白盒测试：要造出「升级前」的形态，就必须能停在某一个中间版本上，
// 而这件事只有 migrator 给得出 —— 导出的 Migrate / Rollback 是全上全下。
// 与 cluster_internal_test.go 同一惯例，因此放在 package mysqlregistry。

// 升级前留在绑定上的仓库级结论，必须被迁移收窄，否则那个集群再也校验不了。
//
// 这一条不是在测「UPDATE 语句会不会执行」，而是在测那个**被堵死的功能**：
// ValidateGitBinding 对未登记取值返回 InvalidError，而 handleVerifyGitBinding
// 是拿库里读出来的绑定去调它的 —— 于是一条 AUTH_FAILED 的历史行会让「重新
// 校验」这个按钮此后每一次都返回同一句拒绝，操作者永远拿不到新结论
// （final review I2）。dev 库恰好只有 NOT_VERIFIED，所以它一直没暴露。
//
// 两个方向一起断言：越界的必须被收窄，**已经合法的必须原样留着**。
// 少了后一条，一句无条件的 UPDATE 也能通过 —— 那会把路径级真正校验出来的
// 结论一起抹掉，也就是用一次数据丢失去换一次数据修正。
func TestMigrationNarrowsPreSplitBindingVerdicts(t *testing.T) {
	cfg := config.DatabaseConfig{DSN: internalTestDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	m, err := migrator(cfg, "../../migrations")
	if err != nil {
		t.Fatalf("migrator() error = %v", err)
	}
	defer func() { _, _ = m.Close() }()

	// 先全下再迁到 000006：升级前的形态只有停在那一版才造得出来，
	// 而全下是这里唯一能给出确定起点的做法（同 newSettingStore 的取舍）。
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Down() error = %v", err)
	}
	if err := m.Migrate(6); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("Migrate(6) error = %v", err)
	}

	// 两条绑定：一条带着拆分前才可能出现的仓库级结论，一条带着拆分后
	// 合法的路径级结论。集群行来自 000002 的种子。
	seedPreSplitBinding(t, db, "prod-asia-1", "repo-pre-split",
		string(registry.RepoVerifyAuthFailed))
	seedPreSplitBinding(t, db, "prod-eu-1", "repo-already-narrow",
		string(registry.BindingVerifyOK))

	if err := m.Migrate(7); err != nil {
		t.Fatalf("Migrate(7) error = %v", err)
	}

	got, at := bindingVerdict(t, db, "prod-asia-1")
	if registry.BindingVerifyResult(got) != registry.BindingVerifyNotVerified {
		t.Errorf("binding verdict after the migration = %q, want %q: "+
			"an out-of-enum verdict permanently blocks re-verification for that cluster",
			got, registry.BindingVerifyNotVerified)
	}
	// 结论没了，那个时刻就不再是任何东西的时刻 —— 带时间戳的 NOT_VERIFIED
	// 会在界面上显示成一次从未发生过的校验。
	if at.Valid {
		t.Errorf("binding kept verifiedAt %v alongside NOT_VERIFIED", at.Time)
	}

	if kept, keptAt := bindingVerdict(t, db, "prod-eu-1"); registry.BindingVerifyResult(kept) != registry.BindingVerifyOK || !keptAt.Valid {
		t.Errorf("binding verdict %q/%v was rewritten, want the legal path-level verdict left untouched",
			kept, keptAt.Valid)
	}

	// 收尾断言：库里剩下的每一个取值都在路径级封闭枚举内。
	rows, err := db.Query(`SELECT verify_result FROM cluster_git_binding`)
	if err != nil {
		t.Fatalf("query verdicts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan verdict: %v", err)
		}
		if !registry.BindingVerifyResult(v).Valid() {
			t.Errorf("binding table still holds verdict %q, which is not in the path-level enum", v)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate verdicts: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cluster_git_binding`)
		_, _ = db.Exec(`DELETE FROM git_repo`)
	})
}

// seedPreSplitBinding 造一条绑定与它指向的仓库，结论按 verdict 落库。
//
// 直接写 SQL 而不是走 Store：Store 会拒绝越界取值，而这条用例要造的正是
// 一条 Store 写不出来、却真实存在于升级前的库里的行。
func seedPreSplitBinding(t *testing.T, db *sql.DB, clusterID, repoID, verdict string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO git_repo
		   (repo_id, repo_url, branch, credential_ref, verify_result, verified_at,
		    created_at, updated_at, deleted_at)
		 VALUES (?, 'ssh://git@git.example.com/net/policies.git', 'main', '',
		         'NOT_VERIFIED', NULL, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), NULL)`,
		repoID); err != nil {
		t.Fatalf("insert git_repo %s: %v", repoID, err)
	}
	if _, err := db.Exec(
		`INSERT INTO cluster_git_binding
		   (cluster_id, repo_id, policy_path, verified_at, verify_result)
		 VALUES (?, ?, 'policies/prod', UTC_TIMESTAMP(6), ?)`,
		clusterID, repoID, verdict); err != nil {
		t.Fatalf("insert cluster_git_binding %s: %v", clusterID, err)
	}
}

// bindingVerdict 读一条绑定的结论与校验时刻。
func bindingVerdict(t *testing.T, db *sql.DB, clusterID string) (string, sql.NullTime) {
	t.Helper()
	var (
		verdict string
		at      sql.NullTime
	)
	if err := db.QueryRow(
		`SELECT verify_result, verified_at FROM cluster_git_binding WHERE cluster_id = ?`,
		clusterID).Scan(&verdict, &at); err != nil {
		t.Fatalf("query binding %s: %v", clusterID, err)
	}
	return verdict, at
}
