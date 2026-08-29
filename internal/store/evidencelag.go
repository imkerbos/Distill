package store

import "time"

// EvidenceLag 说的是一件事：**这份预览里的证据计数，还在不在更新。**
//
// 2026-08-29 的事故形状：周期记账连续失败了 13 个小时，而失败只在日志里
// 留下一行 WARN。界面上唯一的症状是几个数字不再变大——而"不再变大"与
// "这段时间确实没有新观测"长得一模一样。没有任何东西把"这个集群的证据
// 已经停止更新"变成一个可见的事实，于是它一直没被发现，直到有人为了别的
// 事去查累积效果才撞见。
//
// 判据不新增任何表，用平台已经存着的两个事实相比：
//
//	AccountedTo   记账最远记到哪个窗口（rule_evidence.last_seen 的最大值）
//	IngestedTo    摄入最远收到哪个窗口（flow_ingest_run 的最新窗口末端）
//
// 摄入在往前走而记账没跟上，就是记账掉队了。这恰好是这次坏掉的那条不变量，
// 而且它自己不会静默失败——两个值都来自预览本来就要读的那批数据。
type EvidenceLag struct {
	// AccountedTo 是证据记到的最远窗口末端。零值表示一次都没记过。
	AccountedTo time.Time `json:"accountedTo"`
	// IngestedTo 是摄入到的最远窗口末端。零值表示这个集群还没有过流量摄入。
	IngestedTo time.Time `json:"ingestedTo"`
}

// staleAfter 是允许的落后幅度。
//
// 记账周期是 5 分钟，取 3 倍：一次失败、一次重试、再一次才算掉队，
// 而不是偶尔慢一拍就报警。**报警太灵敏的下场是被忽略**，而这条判据存在的
// 全部意义就是它响的时候有人看。
const staleAfter = 15 * time.Minute

// Behind 是记账落后于摄入多久。摄入还没开始时为 0。
func (l EvidenceLag) Behind() time.Duration {
	if l.IngestedTo.IsZero() {
		return 0
	}
	if l.AccountedTo.IsZero() {
		// 一次都没记过，而摄入已经在走：落后的就是摄入至今的全部。
		return l.IngestedTo.Sub(time.Time{})
	}
	if !l.IngestedTo.After(l.AccountedTo) {
		return 0
	}
	return l.IngestedTo.Sub(l.AccountedTo)
}

// Stale 报告证据是否已经停止跟进摄入。
//
// **摄入没开始时不算停摆**：那时证据不更新是因为没有东西可记，
// 与"记账坏了"是两件事，混成一个会让一个刚接入、还没有流量的集群
// 一上来就报一条不存在的故障。
func (l EvidenceLag) Stale() bool {
	if l.IngestedTo.IsZero() {
		return false
	}
	if l.AccountedTo.IsZero() {
		return true
	}
	return l.IngestedTo.Sub(l.AccountedTo) > staleAfter
}
