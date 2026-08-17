package collectstore

import (
	"context"
	"fmt"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/store"
)

// 本文件是分阶段接入的中间态（design doc §7）：EnsureRuleExists 还没有接到
// 采集数据上。
//
// 它**明确拒绝**，而不是返回空结果，也不借道 fixture：
//
//   - 空结果在界面上与"这个集群没有会被拦断的连接"完全一样，而那是一份
//     关于生产集群的结论；
//   - 回退到合成数据是本轮第一顺位要挡住的那件事（design doc §2）。
//
// 拒绝时带上集群 ID：一次拒绝要能说出自己拒绝的是谁，否则运维只能从
// 页面上猜是哪个集群还没接。错误文本里不含 SQL、路径与内部地址
// （安全规范 §22）。

// EnsureRuleExists 尚未接到采集数据上，一律拒绝。
//
// 它是写路径的前置校验，而校验依赖 PolicyPreview 那套候选集。前置校验
// 答不出来时必须拒绝写入，不能放行 —— **失败方向朝关**（安全规范 §49）。
func (r *Reader) EnsureRuleExists(
	_ context.Context, clusterID, namespace, workload, _ string,
	_ policygen.OverrideDecision, _ store.TimeWindow,
) error {
	return fmt.Errorf("%w: rule of %s/%s/%s", ErrReadNotCollectedYet, clusterID, namespace, workload)
}
