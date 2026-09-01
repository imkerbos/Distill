package bookkeeping

import "time"

// RetentionWatermark 算出一个集群的流量清理水位：这个时刻之前的连接可以删。
//
// **取记账水位与可查询期的较小者，两个都要。** 少任一个都会删掉不该删的：
//
//   - 只看时钟：周期记账停摆时它照旧前进，于是把还没汇成 rule_evidence 的
//     连接删掉。那些证据永久没了，候选策略里对应的放行再也产生不出来，
//     而症状是策略集少一条规则，不报任何错。2026-08-29 记账停过 13 小时，
//     这不是一个假想的形态。
//   - 只看记账：记账一路跟上时水位就等于"现在"，于是刚摄入的连接立刻被删，
//     而预览、流量列表与写回都还要读它们。
//
// accounted 为零值（一次都没记过）时不删任何东西：那时"记账记到哪"这个问题
// 还没有答案，而没有答案不等于答案是"记到现在"。
//
// retention 非正时同样不删：那是一个说不出可查询期的配置，据此删除等于
// 让一次配置遗漏变成一次数据销毁。
func RetentionWatermark(now, accounted time.Time, retention time.Duration) (time.Time, bool) {
	if accounted.IsZero() || retention <= 0 {
		return time.Time{}, false
	}
	queryable := now.Add(-retention)
	mark := accounted
	if queryable.Before(mark) {
		mark = queryable
	}
	// 水位落在纪元之前说明可查询期比集群的存在时间还长，此时无事可做。
	if mark.IsZero() || mark.Before(time.Unix(0, 0)) {
		return time.Time{}, false
	}
	return mark, true
}
