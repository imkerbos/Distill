package collectstore

import (
	"context"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

// EnsureRuleExists 校验一条即将落库的人工决定在当前候选集里仍然成立。
//
// **这里此前一律拒绝**（「尚未接到采集数据上」）。那个拒绝在写路径上是对的
// 失败方向，但它的代价是：一个真集群上看得见几百条推荐，却一条都确认不了 ——
// 而「人工逐条确认」正是这个平台的核心动作。接上之后拒绝仍然存在，只是它现在
// 拒绝的是真正该拒的那些（指纹对不上、想关掉一条 Baseline）。
//
// 校验逻辑与 store.FixtureReader 逐字同源，且**共用同一个 generate**：两个端点
// 都要回答「当前候选集长什么样」，各自拼装只要有一处漂移，就会对着不同的候选集
// 给出互相矛盾的答案（store/policy.go §candidateSet 的注释）。
//
// 指纹对不上时返回 registry.NewInvalidError：调用方拿着一个过期页面提交，写进去
// 的覆盖不会报错，只会永远待在「已失效」那一节，而它从来就没生效过。
//
// 指纹对上了、但目标是 BASELINE 来源且决定是 DISABLE 时返回
// policygen.ErrBaselineNotDisablable：policygen.Apply 面对同一种输入本就会把它
// 判成失效，这里只是把同一个必然结论挪到写库之前。
func (r *Reader) EnsureRuleExists(
	ctx context.Context, clusterID, namespace, workload, fingerprint string,
	decision policygen.OverrideDecision, window store.TimeWindow,
) error {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return err
	}
	for _, p := range cs.result.Policies {
		if p.Namespace != namespace || p.Workload != workload {
			continue
		}
		for _, rule := range p.Rules {
			if rule.Fingerprint != fingerprint {
				continue
			}
			if rule.Origin == policygen.OriginBaseline && decision == policygen.DecisionDisable {
				return policygen.ErrBaselineNotDisablable
			}
			return nil
		}
	}
	return registry.NewInvalidError("指纹与当前候选规则不匹配，页面可能已过期")
}
