package collectstore

import (
	"context"
	"fmt"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/store"
)

// 本文件是分阶段接入的中间态（design doc §7）：Flows / Flow / Security /
// PolicyPreview 还没有接到采集数据上。
//
// 它们**明确拒绝**，而不是返回空结果，也不借道 fixture：
//
//   - 空结果在界面上与"这个集群没有流量、没有风险、没有会被拦断的连接"
//     完全一样，而那是一份关于生产集群的结论；
//   - 回退到合成数据是本轮第一顺位要挡住的那件事（design doc §2）。
//
// 每一个都带上集群 ID：一次拒绝要能说出自己拒绝的是谁，否则运维只能从
// 页面上猜是哪个集群还没接。错误文本里不含 SQL、路径与内部地址
// （安全规范 §22）。

// Flows 尚未接到采集数据上，一律拒绝。
func (r *Reader) Flows(_ context.Context, filter store.FlowFilter) (store.FlowPage, error) {
	return store.FlowPage{}, fmt.Errorf("%w: flows of cluster %s", ErrReadNotCollectedYet, filter.Cluster)
}

// Flow 尚未接到采集数据上，一律拒绝。
//
// 不返回 (Decision{}, false, nil)：那个 false 的意思是"这条流量不存在"，
// 而事实是我们还没有把这条读路径接上 —— 又一次把"没做"说成"没有"。
func (r *Reader) Flow(_ context.Context, id string) (store.Decision, bool, error) {
	return store.Decision{}, false, fmt.Errorf("%w: flow %s", ErrReadNotCollectedYet, id)
}

// Security 尚未接到采集数据上，一律拒绝。
func (r *Reader) Security(
	_ context.Context, clusterID string, _ store.TimeWindow,
) (store.SecurityReport, error) {
	return store.SecurityReport{}, fmt.Errorf("%w: security of cluster %s", ErrReadNotCollectedYet, clusterID)
}

// PolicyPreview 尚未接到采集数据上，一律拒绝。
//
// 它排在接入顺序的最后（design doc §7）：这是唯一一个错了会直接导致生产
// 阻断建议的读方法。
func (r *Reader) PolicyPreview(
	_ context.Context, clusterID, namespace string, _ store.TimeWindow,
) (store.PolicyPreview, error) {
	return store.PolicyPreview{}, fmt.Errorf(
		"%w: policy preview of %s/%s", ErrReadNotCollectedYet, clusterID, namespace)
}

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
