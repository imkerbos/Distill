package bookkeeping

import (
	"context"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// Purger 删掉一个集群保留水位之前的连接，并记下水位。
//
// 三个方法分开，因为顺序是这套机制的一部分：先删干净、再推进水位。
// 反过来在中途失败时会留下一个说谎的水位 —— 读取端据此报「已清理」，
// 而那段数据其实还在，于是一次可以正常回答的查询被拒绝。而清理是分批
// 跑的，中途停下（进程重启、记账停摆、磁盘告急）是常态不是异常。
type Purger interface {
	PurgeConnectionsBefore(ctx context.Context, clusterID string, before time.Time) (int, error)
	AdvanceRetainedFrom(ctx context.Context, clusterID string, at time.Time) error
}

// maxPurgeBatchesPerRound 是一轮记账里最多删几批。
//
// 有上限而不是删到干净为止：一个积压了很久的集群会让这一轮跑很长，
// 而记账循环里还有别的集群等着。删不完下一轮接着删 —— 水位只前进，
// 因此分多轮与一轮删完的最终状态一样。
const maxPurgeBatchesPerRound = 20

// purgeCluster 删掉一个集群水位之前的连接。
//
// **水位在删干净之后才推进。** 见 Purger 的说明。删到一批为 0 表示水位
// 之前已经没有剩余，这时才把水位记下来；没删完就不记 —— 下一轮从同一个
// 位置接着删，而读取端在此期间照旧如实回答那段时间。
func (a Accountant) purgeCluster(
	ctx context.Context, clusterID string, accounted time.Time, retention time.Duration,
) {
	if a.purger == nil {
		return
	}
	mark, ok := RetentionWatermark(a.now(), accounted, retention)
	if !ok {
		return
	}
	total := 0
	for range maxPurgeBatchesPerRound {
		if err := ctx.Err(); err != nil {
			return
		}
		n, err := a.purger.PurgeConnectionsBefore(ctx, clusterID, mark)
		if err != nil {
			// 删除失败不推进水位：那段数据还在，说它没了就是在编造事实。
			a.logger.Warn("cannot purge flow connections; the retention watermark stays put",
				"cluster", clusterID, "before", mark, "purged", total, "err", err)
			return
		}
		total += n
		if n == 0 {
			if err := a.purger.AdvanceRetainedFrom(ctx, clusterID, mark); err != nil {
				a.logger.Warn("purged connections but could not record the watermark",
					"cluster", clusterID, "before", mark, "purged", total, "err", err)
				return
			}
			if total > 0 {
				a.logger.Info("purged flow connections",
					"cluster", clusterID, "before", mark, "rows", total)
			}
			return
		}
	}
	// 这一轮没删完：不推进水位，下一轮接着删。
	a.logger.Info("flow purge did not finish this round; it resumes next round",
		"cluster", clusterID, "before", mark, "rows", total)
}

// purgeAfterAccounting 在这一轮记账之后清理这个集群。
//
// **排在记账之后**：水位取的正是记账刚推进到的位置，先删的话这一轮新记的
// 那一段还进不了水位，白等一轮。
func (a Accountant) purgeAfterAccounting(ctx context.Context, c registry.Cluster) {
	if a.purger == nil || a.retentionOf == nil {
		return
	}
	accounted, ok, err := a.accountedFloor(ctx, c.ID)
	if err != nil || !ok {
		// 读不到记账进度就不删。**不当成"记到现在"**：那会把还没汇总的
		// 证据删掉，而那是这套水位存在的头一条理由。
		return
	}
	a.purgeCluster(ctx, c.ID, accounted, a.retentionOf(c))
}

// accountedFloor 取全部记账里最落后的那一个。
//
// **取最小值，不取任意一个。** 每件记账各有各的进度；按其中跑得快的那件
// 删，跑得慢的那件就再也读不到它还没消化的连接了。
func (a Accountant) accountedFloor(ctx context.Context, clusterID string) (time.Time, bool, error) {
	var floor time.Time
	for _, task := range a.tasks {
		at, ok, err := task.Accounted.LastAccountedWindowEnd(ctx, clusterID)
		if err != nil {
			return time.Time{}, false, err
		}
		if !ok {
			// 有一件记账一次都没跑过：它需要的连接一条都不能删。
			return time.Time{}, false, nil
		}
		if floor.IsZero() || at.Before(floor) {
			floor = at
		}
	}
	return floor, !floor.IsZero(), nil
}
