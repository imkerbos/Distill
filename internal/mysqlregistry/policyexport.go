package mysqlregistry

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/imkerbos/Distill/internal/registry"
)

// actionPolicyExport 是策略导出的审计动作名。
const actionPolicyExport = "EXPORT_POLICY"

// RecordPolicyExport 为一次策略导出写一条审计行。
//
// 没有业务写入，因为导出不改变平台里的任何状态 —— 但它必须留痕：流出去的
// 是这个集群完整的网络策略（design doc 2026-08-14 §5，规范 §43）。仍然走
// mutate 而不是直接 INSERT 一条审计：审计行的事务、列与失败语义只有一处
// 定义，另开一条写路径就是另一份要跟着改的规则。
//
// 范围记进 after_val、before_val 留空：导出前后没有"前一个状态"这回事，
// 硬塞一个会让追溯的人以为这次操作改了什么。after 记的是"被拿走的是哪一
// 份"——集群、命名空间筛选、时间窗与条数，不含策略内容本身。
func (s *Store) RecordPolicyExport(
	ctx context.Context, actor registry.Actor, e registry.PolicyExport,
) error {
	// namespace 进 target：一次全集群导出与一次单命名空间导出是不同规模的
	// 事件，靠 target 就能分开检索，不必解 JSON。
	target := fmt.Sprintf("policy_export/%s/%s", e.ClusterID, namespaceTarget(e.Namespace))
	after := map[string]any{
		"namespace": e.Namespace,
		"from":      e.From,
		"to":        e.To,
		"documents": e.Documents,
		"rules":     e.Rules,
	}
	return s.mutate(ctx, actor, e.ClusterID, actionPolicyExport, target, nil, after,
		func(*sql.Tx) error { return nil })
}

// namespaceTarget 把命名空间筛选渲染进 target；空筛选是全集群。
//
// 用一个不可能是合法命名空间名的字面量（"*" 含非法字符）表示"全集群"，
// 而不是留空：留空的 target 读起来像是漏填了一段，而"全集群"恰恰是范围
// 最大的那一次导出。
func namespaceTarget(namespace string) string {
	if namespace == "" {
		return "*"
	}
	return namespace
}
