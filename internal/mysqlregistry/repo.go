package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// gitRepoTarget 是仓库级审计行的 target（design doc 2026-08-13 §4）。
//
// 与 git-binding/<clusterID> 分开：「谁改了这个仓库的地址或凭据」与
// 「谁把某个集群改绑到了别处」是两个问题，共用一个 target 就等于要求
// 追溯的人先从前后值里认出这次写的到底是哪一边。
func gitRepoTarget(repoID string) string {
	return "git-repo/" + repoID
}

// gitRepoClusterID 是仓库级审计行的 cluster_id。
//
// 仓库不属于任何集群，理由与取值同 settingClusterID —— 那里写了为什么
// 只有空串撞不上真实集群，这里不再重复一遍。
const gitRepoClusterID = ""

// gitRepoVerifyRecord 是 VERIFY_GIT_REPO 审计行的前后值。
//
// 只放结论与时间，不放整个仓库：这次写入动的就是这两列。理由同
// gitVerifyRecord。
type gitRepoVerifyRecord struct {
	VerifyResult registry.RepoVerifyResult `json:"verifyResult"`
	VerifiedAt   *time.Time                `json:"verifiedAt,omitempty"`
}

// scanGitRepo 把一行 git_repo 读进 registry.GitRepo。
//
// 由 GitRepos 与 GitRepo 共用：两处的列清单必须一致，抄两份的那一天
// 只会是其中一处新增了列而另一处没有。
func scanGitRepo(scan func(...any) error) (registry.GitRepo, error) {
	var (
		r            registry.GitRepo
		verifiedAt   sql.NullTime
		verifyResult string
	)
	if err := scan(&r.ID, &r.URL, &r.Branch, &r.CredentialRef,
		&verifiedAt, &verifyResult); err != nil {
		return registry.GitRepo{}, err
	}
	// NULL 必须落成 nil，不能落成零值 time.Time：零值是一个真实存在的
	// 过去时间点（1970 年），任何新鲜度检查都会把它当成「校验过」而非
	// 「从未校验」放行。
	if verifiedAt.Valid {
		t := verifiedAt.Time
		r.VerifiedAt = &t
	}
	r.VerifyResult = registry.RepoVerifyResult(verifyResult)
	return r, nil
}

// GitRepos 返回全部未删除的策略仓库，按 ID 升序。
func (s *Store) GitRepos(ctx context.Context) ([]registry.GitRepo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo_id, repo_url, branch, credential_ref, verified_at, verify_result
		   FROM git_repo WHERE deleted_at IS NULL ORDER BY repo_id`)
	if err != nil {
		return nil, fmt.Errorf("query git repos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []registry.GitRepo
	for rows.Next() {
		r, err := scanGitRepo(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan git repo: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate git repos: %w", err)
	}
	return out, nil
}

// GitRepo 按 ID 查一个未删除的仓库。
func (s *Store) GitRepo(ctx context.Context, repoID string) (registry.GitRepo, bool, error) {
	r, err := scanGitRepo(s.db.QueryRowContext(ctx,
		`SELECT repo_id, repo_url, branch, credential_ref, verified_at, verify_result
		   FROM git_repo WHERE repo_id = ? AND deleted_at IS NULL`, repoID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.GitRepo{}, false, nil
	}
	if err != nil {
		return registry.GitRepo{}, false, fmt.Errorf("query git repo: %w", err)
	}
	return r, true, nil
}

// CreateGitRepo 登记一个仓库，同事务写审计。
func (s *Store) CreateGitRepo(ctx context.Context, actor registry.Actor, r registry.GitRepo) error {
	if err := registry.ValidateGitRepo(r); err != nil {
		return err
	}
	// 空值按 NOT_VERIFIED 落库，不落空串：空串不是登记过的枚举值，
	// 会让「从未校验」在库里以一个不合法的值存在。
	if r.VerifyResult == "" {
		r.VerifyResult = registry.RepoVerifyNotVerified
	}
	return s.mutate(ctx, actor, gitRepoClusterID, "CREATE_GIT_REPO", gitRepoTarget(r.ID),
		nil, r, func(tx *sql.Tx) error {
			now := s.now()
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO git_repo
				   (repo_id, repo_url, branch, credential_ref, verified_at,
				    verify_result, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				r.ID, r.URL, r.Branch, r.CredentialRef, nullTimeIfNil(r.VerifiedAt),
				string(r.VerifyResult), now, now,
			); err != nil {
				// 软删除留着行，因此一个删掉的 ID 不能被重新登记 —— 与集群
				// 同一处置。文案说出这件事，否则操作者会以为是自己看花了眼。
				return writeFailure("insert git repo",
					fmt.Sprintf("仓库 ID %q 已被登记（含已删除的），请换一个", r.ID), "", err)
			}
			return nil
		})
}

// UpdateGitRepo 修改一个仓库，同事务写审计。仓库不存在时返回 ErrNotFound。
//
// 整体替换，不做部分更新：因此改地址、分支或凭据会把上一次的校验结论
// 一并清掉 —— 换了地址之后，旧的 OK 描述的是另一个仓库，留着它就是拿
// 一句关于别处的判断给新地址背书（同 BindGitRepo）。请求形状里没有
// verifyResult / verifiedAt，它们到这里恒为零值，落库即 NOT_VERIFIED。
func (s *Store) UpdateGitRepo(ctx context.Context, actor registry.Actor, r registry.GitRepo) error {
	if err := registry.ValidateGitRepo(r); err != nil {
		return err
	}
	if r.VerifyResult == "" {
		r.VerifyResult = registry.RepoVerifyNotVerified
	}
	before, ok, err := s.GitRepo(ctx, r.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: git repo %s", ErrNotFound, r.ID)
	}
	return s.mutate(ctx, actor, gitRepoClusterID, "UPDATE_GIT_REPO", gitRepoTarget(r.ID),
		before, r, func(tx *sql.Tx) error {
			if err := requireLiveRepo(ctx, tx, r.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE git_repo
				    SET repo_url = ?, branch = ?, credential_ref = ?,
				        verified_at = ?, verify_result = ?, updated_at = ?
				  WHERE repo_id = ?`,
				r.URL, r.Branch, r.CredentialRef, nullTimeIfNil(r.VerifiedAt),
				string(r.VerifyResult), s.now(), r.ID,
			); err != nil {
				return fmt.Errorf("update git repo: %w", err)
			}
			return nil
		})
}

// SoftDeleteGitRepo 下线一个仓库，同事务写审计。
// 仓库不存在时返回 ErrNotFound；仍被集群绑定时返回 registry.ErrRepoInUse。
//
// 软删除而非物理删除：绑定表上的外键指向这一行，物理删除要么被外键挡住、
// 要么得级联 —— 而级联正是这里要避免的东西。
//
// 「仍被绑定就拒绝」在事务内判定，不在调用方判定（design doc §4）：放在
// 事务外，「先查绑定、再删仓库」之间的窗口里刚好新建的绑定会指向一个已经
// 删掉的仓库，而那正是级联要造成的后果 —— 一个集群的策略下发路径被静默
// 解除，没有报错，也没有人做过这个决定。加锁读同时挡住这个窗口。
func (s *Store) SoftDeleteGitRepo(ctx context.Context, actor registry.Actor, repoID string) error {
	before, ok, err := s.GitRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: git repo %s", ErrNotFound, repoID)
	}
	return s.mutate(ctx, actor, gitRepoClusterID, "DELETE_GIT_REPO", gitRepoTarget(repoID),
		before, nil, func(tx *sql.Tx) error {
			if err := requireLiveRepo(ctx, tx, repoID); err != nil {
				return err
			}
			// 唯一索引上的 FOR UPDATE：命中时锁住那一行，未命中时锁住那段
			// 间隙，因而本事务提交前没有别的事务能新建一条指向这个仓库的
			// 绑定。少了这把锁，check → use 之间就有一条并发绑定挤得进来。
			var boundTo string
			err := tx.QueryRowContext(ctx,
				`SELECT cluster_id FROM cluster_git_binding WHERE repo_id = ? FOR UPDATE`,
				repoID).Scan(&boundTo)
			switch {
			case err == nil:
				return fmt.Errorf("%w: 仓库 %s 仍被集群 %s 绑定",
					registry.ErrRepoInUse, repoID, boundTo)
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("lock git binding by repo: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE git_repo SET deleted_at = ?, updated_at = ? WHERE repo_id = ?`,
				s.now(), s.now(), repoID,
			); err != nil {
				return fmt.Errorf("soft delete git repo: %w", err)
			}
			return nil
		})
}

// SetGitRepoVerifyResult 只写一次仓库级只读校验的结论与时间，同事务写审计。
// 仓库不存在时返回 ErrNotFound。
//
// 只 UPDATE verify_result 与 verified_at 两列：跑一次校验不是一次配置变更，
// 它没有理由改写地址、分支或凭据（同 SetGitVerifyResult）。
func (s *Store) SetGitRepoVerifyResult(
	ctx context.Context, actor registry.Actor, repoID string,
	result registry.RepoVerifyResult, at time.Time,
) error {
	if !result.Valid() {
		// 封闭枚举：自由文本落进库里，统计口径当场失去意义，
		// 而前端只会把它渲染成一个没人认识的字符串。
		return registry.NewInvalidError(
			fmt.Sprintf("verifyResult %q 不在已登记的取值范围内", result))
	}
	before, ok, err := s.GitRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: git repo %s", ErrNotFound, repoID)
	}
	return s.mutate(ctx, actor, gitRepoClusterID, "VERIFY_GIT_REPO", gitRepoTarget(repoID),
		gitRepoVerifyRecord{VerifyResult: before.VerifyResult, VerifiedAt: before.VerifiedAt},
		gitRepoVerifyRecord{VerifyResult: result, VerifiedAt: &at},
		func(tx *sql.Tx) error {
			// 存在性由事务内的加锁读断言，不由 RowsAffected 断言：运行时
			// 连接池没开 clientFoundRows，重复写入同一个结论与同一个时间戳
			// 时 MySQL 报的受影响行数是 0，与「仓库已被并发删除」无法区分
			// （同 SetGitVerifyResult）。
			if err := requireLiveRepo(ctx, tx, repoID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE git_repo SET verify_result = ?, verified_at = ?, updated_at = ?
				  WHERE repo_id = ?`,
				string(result), at, s.now(), repoID,
			); err != nil {
				return fmt.Errorf("update git repo verify result: %w", err)
			}
			return nil
		})
}
