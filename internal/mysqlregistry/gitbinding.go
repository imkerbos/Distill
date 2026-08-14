package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// gitBindingTarget 是绑定级审计行的 target（design doc 2026-08-13 §4）。
//
// 绑定有自己的 target 而不是沿用 cluster/<id>：「谁动了这个集群的绑定」
// 与「谁改了集群本身」是两个问题，共用一个 target 就等于要求追溯的人
// 先从前后值里认出这次写的到底是哪一部分。
func gitBindingTarget(clusterID string) string {
	return "git-binding/" + clusterID
}

// gitVerifyRecord 是 VERIFY_GIT_BINDING 审计行的前后值。
//
// 只放结论与时间，不放整个绑定：这次写入动的就是这两列。把仓库地址
// 一并塞进前后值，追溯的人又要在一个更大的对象里认「哪几个字段真的
// 变了」—— 那正是本次拆分要消灭的东西。
type gitVerifyRecord struct {
	VerifyResult registry.VerifyResult `json:"verifyResult"`
	VerifiedAt   *time.Time            `json:"verifiedAt,omitempty"`
}

// gitBinding 读一个集群当前的绑定，供绑定级写入取审计前值。
//
// 与 loadChildren 里那段读取并存而不是抽出来共用：读模型继续内嵌绑定
// （design doc 2026-08-13 §2），loadChildren 的形状与生命周期跟着集群走；
// 这里要的是绑定自己这一行，跟着绑定走。两者将来会因为不同的原因变化。
func (s *Store) gitBinding(
	ctx context.Context, clusterID string,
) (registry.GitBinding, bool, error) {
	var g registry.GitBinding
	var credRef, lastCommit, verifyResult sql.NullString
	var verifiedAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT repo_url, branch, policy_path, credential_ref, last_written_commit,
		        verified_at, verify_result
		   FROM cluster_git_binding WHERE cluster_id = ?`, clusterID).
		Scan(&g.RepoURL, &g.Branch, &g.PolicyPath, &credRef, &lastCommit,
			&verifiedAt, &verifyResult)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.GitBinding{}, false, nil
	}
	if err != nil {
		return registry.GitBinding{}, false, fmt.Errorf("query git binding: %w", err)
	}
	g.CredentialRef = credRef.String
	g.LastWrittenCommit = lastCommit.String
	// 与 loadChildren 同一个理由：NULL 落成 nil 而不是零值 time.Time，
	// 否则「从未校验」会被新鲜度检查当成 1970 年校验过而放行。
	if verifiedAt.Valid {
		t := verifiedAt.Time
		g.VerifiedAt = &t
	}
	g.VerifyResult = registry.VerifyResult(verifyResult.String)
	return g, true, nil
}

// requireLiveCluster 在事务内确认集群仍然存在且未下线。
//
// 外键挡得住「集群从未注册」，挡不住「集群已软删除」—— 软删除留着
// cluster 行，外键照样满足。而事务外的存在性检查挡不住并发下线：加锁读
// 让这一行在本事务提交前不能被改成已删除，这次绑定写入因此落在一个
// 此刻仍然存活的集群上，审计里也就不会出现一条给已下线集群改绑的记录。
func requireLiveCluster(ctx context.Context, tx *sql.Tx, clusterID string) error {
	var alive int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM cluster WHERE cluster_id = ? AND deleted_at IS NULL FOR UPDATE`,
		clusterID).Scan(&alive)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: cluster %s", ErrNotFound, clusterID)
	}
	if err != nil {
		return fmt.Errorf("lock cluster: %w", err)
	}
	return nil
}

// BindGitRepo 写入或替换一个集群的 Git 绑定，同事务写审计。
// 集群不存在时返回 ErrNotFound。
//
// 整体替换，不做部分更新（design doc 2026-08-13 §5）：因此改绑会把上一次
// 的校验结论一并清掉 —— 换了仓库地址之后，旧的 OK 描述的是另一个仓库，
// 留着它就是拿一句关于别处的判断给新地址背书。
func (s *Store) BindGitRepo(
	ctx context.Context, actor registry.Actor, clusterID string, b registry.GitBinding,
) error {
	if err := registry.ValidateGitBinding(b); err != nil {
		return err
	}
	// 空值按 NOT_VERIFIED 落库，不落空串：空串不是登记过的枚举值，
	// 会让「从未校验」在库里以一个不合法的值存在。
	if b.VerifyResult == "" {
		b.VerifyResult = registry.VerifyNotVerified
	}
	before, bound, err := s.gitBinding(ctx, clusterID)
	if err != nil {
		return err
	}
	// 首次绑定的前值是 NULL 而不是一个空绑定：空对象读起来像
	// 「原先绑过一个各字段都为空的仓库」，那是一件没发生过的事。
	var beforeVal any
	if bound {
		beforeVal = before
	}
	return s.mutate(ctx, actor, clusterID, "BIND_GIT_REPO", gitBindingTarget(clusterID),
		beforeVal, b, func(tx *sql.Tx) error {
			if err := requireLiveCluster(ctx, tx, clusterID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO cluster_git_binding
				   (cluster_id, repo_url, branch, policy_path, credential_ref,
				    last_written_commit, verified_at, verify_result)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				 ON DUPLICATE KEY UPDATE
				   repo_url = VALUES(repo_url), branch = VALUES(branch),
				   policy_path = VALUES(policy_path),
				   credential_ref = VALUES(credential_ref),
				   last_written_commit = VALUES(last_written_commit),
				   verified_at = VALUES(verified_at), verify_result = VALUES(verify_result)`,
				clusterID, b.RepoURL, b.Branch, b.PolicyPath, nullIfEmpty(b.CredentialRef),
				nullIfEmpty(b.LastWrittenCommit), nullTimeIfNil(b.VerifiedAt),
				string(b.VerifyResult),
			); err != nil {
				return fmt.Errorf("upsert git binding: %w", err)
			}
			return nil
		})
}

// UnbindGitRepo 解除一个集群的 Git 绑定，同事务写审计。
// 绑定不存在时返回 ErrNotFound。
//
// 绑定行物理删除，不软删除：这张表上没有 deleted_at，而「这个集群的策略
// 曾经指向哪里」由审计行承载 —— UNBIND_GIT_REPO 的前值就是那份记录。
// 加一列 deleted_at 会让读路径与唯一键都要处理「解绑过又绑回来」的历史行，
// 换不到审计已经给出的东西。
func (s *Store) UnbindGitRepo(
	ctx context.Context, actor registry.Actor, clusterID string,
) error {
	before, bound, err := s.gitBinding(ctx, clusterID)
	if err != nil {
		return err
	}
	if !bound {
		return fmt.Errorf("%w: git binding %s", ErrNotFound, clusterID)
	}
	return s.mutate(ctx, actor, clusterID, "UNBIND_GIT_REPO", gitBindingTarget(clusterID),
		before, nil, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`DELETE FROM cluster_git_binding WHERE cluster_id = ?`, clusterID)
			if err != nil {
				return fmt.Errorf("delete git binding: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected: %w", err)
			}
			if n == 0 {
				// 事务外的存在性检查挡不住并发解绑：只有事务内的行数能
				// 证明这次删除真的落在了一行上。返回 ErrNotFound 让 mutate
				// 连审计一起回滚，否则审计里会留下一条描述从未发生过的
				// 解绑的记录。
				return fmt.Errorf("%w: git binding %s", ErrNotFound, clusterID)
			}
			return nil
		})
}

// SetGitVerifyResult 只写一次只读校验的结论与时间，同事务写审计。
// 绑定不存在时返回 ErrNotFound。
//
// 只 UPDATE verify_result 与 verified_at 两列：跑一次校验不是一次配置
// 变更，它没有理由改写仓库地址、分支、路径，更没有理由重写集群行
// （design doc 2026-08-13 §1 说的正是嵌在集群写模型里时付出的这笔代价）。
func (s *Store) SetGitVerifyResult(
	ctx context.Context, actor registry.Actor, clusterID string,
	result registry.VerifyResult, at time.Time,
) error {
	if !result.Valid() {
		// 封闭枚举：自由文本落进库里，统计口径当场失去意义，
		// 而前端只会把它渲染成一个没人认识的字符串。
		return registry.NewInvalidError(
			fmt.Sprintf("verifyResult %q 不在已登记的取值范围内", result))
	}
	before, bound, err := s.gitBinding(ctx, clusterID)
	if err != nil {
		return err
	}
	if !bound {
		return fmt.Errorf("%w: git binding %s", ErrNotFound, clusterID)
	}
	return s.mutate(ctx, actor, clusterID, "VERIFY_GIT_BINDING", gitBindingTarget(clusterID),
		gitVerifyRecord{VerifyResult: before.VerifyResult, VerifiedAt: before.VerifiedAt},
		gitVerifyRecord{VerifyResult: result, VerifiedAt: &at},
		func(tx *sql.Tx) error {
			// 存在性由事务内的加锁读断言，不由 RowsAffected 断言：运行时
			// 连接池没开 clientFoundRows，重复写入同一个结论与同一个时间戳
			// 时 MySQL 报的受影响行数是 0，与「绑定已被并发解绑」无法区分。
			// 拿它当判据，一次正常的重复校验会被报成 ErrNotFound。
			var bound int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM cluster_git_binding WHERE cluster_id = ? FOR UPDATE`,
				clusterID).Scan(&bound)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: git binding %s", ErrNotFound, clusterID)
			}
			if err != nil {
				return fmt.Errorf("lock git binding: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE cluster_git_binding SET verify_result = ?, verified_at = ?
				  WHERE cluster_id = ?`,
				string(result), at, clusterID,
			); err != nil {
				return fmt.Errorf("update git verify result: %w", err)
			}
			return nil
		})
}
