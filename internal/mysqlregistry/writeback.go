package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
)

const (
	// actionPlanWriteback 是「出了一份写回计划」的审计动作名。
	actionPlanWriteback = "PLAN_POLICY_WRITEBACK"
	// actionPushWriteback 是「真的推了」的审计动作名。
	//
	// 与出计划分成两个动作（design doc 2026-08-14 §9）：出计划是常规动作，
	// 推送是一次仓库变更，混成一条会让"谁真的改了仓库"淹没在计划请求里。
	actionPushWriteback = "PUSH_POLICY_WRITEBACK"
)

// writebackTarget 是写回审计行的 target。
//
// 与 gitBindingTarget 分开：绑定的 target 回答"谁改了这个集群的下发配置"，
// 而写回回答"谁把策略推了出去"。共用一个 target，追溯的人就得先从前后值里
// 认出这次到底是配置变更还是一次推送。
func writebackTarget(clusterID string) string {
	return "policy_writeback/" + clusterID
}

// writebackRecord 是写回审计行的 after 值。
//
// 集群、目标分支、commit SHA、文件数与四类计数，不含文件内容
// （design doc §9）：内容体积不可控，且策略本身已经在 Git 里。
type writebackRecord struct {
	Branch string                     `json:"branch"`
	Files  int                        `json:"files"`
	Counts map[predict.ChangeKind]int `json:"counts"`
	// LastWrittenCommit 只在推送时有值：出计划阶段还没有 commit，
	// 而一个凭空出现的 SHA 会让读的人以为那次计划已经推出去了。
	LastWrittenCommit string `json:"lastWrittenCommit,omitempty"`
}

// writebackCommitRecord 是 PUSH_POLICY_WRITEBACK 的 before 值：这次业务写入
// 动的就是 last_written_commit 这一列。
//
// 与 after 用不同的形状，是因为两者本就是不同的东西：before 描述被改的那
// 一列的旧值，after 还要带上这次事件的属性（分支、文件数、计数）。硬套同
// 一个结构，before 会出现一串空壳字段，读的人分不清"没变"与"没有"。
//
// 平台从未写过时留空串而不是 NULL：NULL 的 before_val 在本库里表示"这个
// 动作不改变任何状态"（见 RecordPolicyExport），而推送恰恰改了状态。
type writebackCommitRecord struct {
	LastWrittenCommit string `json:"lastWrittenCommit"`
}

// RecordWritebackPlan 为一次出计划写一条审计行，动作 PLAN_POLICY_WRITEBACK。
//
// 没有业务写入，因为出计划不改变平台里的任何状态 —— 但它必须留痕：一份
// 计划描述的是平台打算往策略仓库里写什么（规范 §43）。仍然走 mutate 而不是
// 直接 INSERT 一条审计：审计行的事务、列与失败语义只有一处定义。
//
// before 留空、范围进 after：出计划之前没有"前一个状态"这回事，
// 硬塞一个会让追溯的人以为这次操作改了什么。
func (s *Store) RecordWritebackPlan(
	ctx context.Context, actor registry.Actor, clusterID string, w registry.Writeback,
) error {
	if err := registry.ValidateWriteback(w); err != nil {
		return err
	}
	after := writebackRecord{Branch: w.Branch, Files: w.Files, Counts: w.Counts}
	return s.mutate(ctx, actor, clusterID, actionPlanWriteback, writebackTarget(clusterID),
		nil, after, func(*sql.Tx) error { return nil })
}

// SetLastWrittenCommit 写入绑定的 last_written_commit，同事务写审计。
// 绑定不存在时返回 ErrNotFound。
//
// 只 UPDATE 这一列：一次推送不是一次配置变更，它没有理由改写绑定指向的
// 仓库、路径或校验结论（与 SetGitVerifyResult 同一个理由）。
//
// commit 与审计同事务（design doc §8）：分开写的话，进程在两次写入之间挂掉
// 会留下一个没人推过的 commit，或者一次没落到基准上的推送 —— 而漂移检测
// 拿这一列比对的正是"平台最后交出去的是什么"。
func (s *Store) SetLastWrittenCommit(
	ctx context.Context, actor registry.Actor, clusterID, commitSHA string, w registry.Writeback,
) error {
	if err := registry.ValidateCommitSHA(commitSHA); err != nil {
		return err
	}
	if err := registry.ValidateWriteback(w); err != nil {
		return err
	}
	before, bound, err := s.gitBinding(ctx, clusterID)
	if err != nil {
		return err
	}
	if !bound {
		return fmt.Errorf("%w: git binding %s", ErrNotFound, clusterID)
	}
	after := writebackRecord{
		Branch: w.Branch, Files: w.Files, Counts: w.Counts, LastWrittenCommit: commitSHA,
	}
	return s.mutate(ctx, actor, clusterID, actionPushWriteback, writebackTarget(clusterID),
		writebackCommitRecord{LastWrittenCommit: before.LastWrittenCommit}, after,
		func(tx *sql.Tx) error {
			// 存在性由事务内的加锁读断言，不由 RowsAffected 断言：运行时
			// 连接池没开 clientFoundRows，重复推同一个 commit 时 MySQL 报的
			// 受影响行数是 0，与"绑定已被并发解绑"无法区分。
			var locked int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM cluster_git_binding WHERE cluster_id = ? FOR UPDATE`,
				clusterID).Scan(&locked)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: git binding %s", ErrNotFound, clusterID)
			}
			if err != nil {
				return fmt.Errorf("lock git binding: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE cluster_git_binding SET last_written_commit = ? WHERE cluster_id = ?`,
				commitSHA, clusterID,
			); err != nil {
				return fmt.Errorf("update last written commit: %w", err)
			}
			return nil
		})
}

// 编译期确认 Store 满足写回所需的接口。放在这里而非测试里：
// 接口漏实现应当在构建时失败，而不是等到装配时才发现。
var _ registry.WritebackStore = (*Store)(nil)
