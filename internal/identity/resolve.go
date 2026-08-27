package identity

import "time"

// Outcome 是一次身份解析的结果，封闭枚举。
type Outcome string

const (
	// OutcomeResolved 表示恰好一个区间覆盖那一刻。
	OutcomeResolved Outcome = "RESOLVED"
	// OutcomeAmbiguous 表示多于一个区间覆盖那一刻，归属不唯一。
	OutcomeAmbiguous Outcome = "AMBIGUOUS"
	// OutcomeNotCovered 表示那一刻这个 IP 没有 Pod —— 可能是节点 IP、
	// 外部地址，或 Pod 尚未创建。这是一个结论，不是一次弃权。
	OutcomeNotCovered Outcome = "NOT_COVERED"
	// OutcomeNoData 表示平台那段时间没在看，无从判断。
	//
	// 与 NOT_COVERED 不得合并：合成一个会让"我们没数据"被读成"那时确实
	// 没有 Pod"，而后者是下游会当作事实使用的结论。
	OutcomeNoData Outcome = "NO_DATA"
	// OutcomeHostNetwork 表示覆盖那一刻的区间**全都是 hostNetwork Pod**。
	//
	// 它们共用节点 IP，因此"多个区间覆盖同一时刻"是常态而不是异常 ——
	// 一个节点上跑着十个 DaemonSet Pod 就有十个区间。按 AMBIGUOUS 处理会
	// 把这件事报成"归属不唯一，我不知道它是谁"，而真相是一个确定的结论：
	// **这一端不受 NetworkPolicy 管控**（它不在 Pod 网络里，策略选不中它）。
	//
	// 与 NOT_COVERED 同一条纪律 —— 那条的注释写着"这是一个结论，不是一次
	// 弃权"。把确定的事实计进「无法判定」，会让那个比例失去意义：实测这个
	// 集群 26.3% 的 UNKNOWN 里，绝大部分是这件事。
	//
	// **只在全都是 hostNetwork 时成立。** 混合（既有 hostNetwork 又有普通
	// Pod）仍然是真歧义：那时确实答不出这条连接的另一端是不是受管控的那个。
	OutcomeHostNetwork Outcome = "HOST_NETWORK"
)

// Valid 判断该结果是否已登记。
//
// 显式 switch 而非查表：新增一个取值却没在这里列出来，它就不是一个合法
// 结果，而这一行的缺失在评审里看得见。零值不合法 —— 一个没被赋过值的
// 结果不得被当成任何一种结论。
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeResolved, OutcomeAmbiguous, OutcomeNotCovered, OutcomeNoData, OutcomeHostNetwork:
		return true
	default:
		return false
	}
}

// Resolve 回答「在时刻 at，这个 IP 属于谁」。
//
// intervals 必须是同一个 (cluster_id, pod_ip) 的全部已知区间，由调用方按
// 主键前缀查出：本函数没有集群与地址参数，无从校验这一点，混进别的
// 集群的区间会静默解到错误的 Pod 上。
//
// 只读、无副作用。RESOLVED 之外一律返回零值 Identity —— 尤其是
// AMBIGUOUS：任选一个仍然能查出结果、仍然不报错，而错的那次没有任何症状。
func Resolve(intervals []Interval, at time.Time) (Identity, Outcome) {
	var (
		hit        Identity
		matches    int
		allHostNet = true
	)
	for _, iv := range intervals {
		if iv.Covers(at) {
			hit = iv.Identity
			matches++
			if !iv.Identity.HostNetwork {
				allHostNet = false
			}
		}
	}
	switch {
	case matches == 1 && hit.HostNetwork:
		// 单个 hostNetwork 区间也走这一支：解出来的那个 Pod 确实是它，
		// 但下游关心的不是"哪个 Pod"，而是"这一端受不受策略管控"，
		// 而答案与有几个区间无关。两处给出不同的 Outcome 会让同一个事实
		// 在一个节点上只跑一个 hostNetwork Pod 时表现成另一回事。
		return hit, OutcomeHostNetwork
	case matches == 1:
		return hit, OutcomeResolved
	case matches > 1 && allHostNet:
		// 全是 hostNetwork：它们共用节点 IP，这不是归属不唯一，而是
		// 一个确定的结论 —— 这一端不在 Pod 网络里。Identity 返回零值，
		// 与 AMBIGUOUS 同理：具体是哪一个 Pod 既答不出也不重要。
		return Identity{}, OutcomeHostNetwork
	case matches > 1:
		return Identity{}, OutcomeAmbiguous
	}

	// 没有区间覆盖时不看"最近的那个"：一个 5 分钟前结束的区间对当前时刻
	// 没有解释力，返回它等于用当前状态解释历史数据的镜像错误。只区分
	// "窗口内的空档"与"窗口外"。
	if !withinObservedWindow(intervals, at) {
		return Identity{}, OutcomeNoData
	}
	return Identity{}, OutcomeNotCovered
}

// withinObservedWindow 判断 at 是否落在这批区间给出的观测窗口内。
//
// 窗口下界是最早的 ValidFrom，上界是最晚的 ValidTo（存在开区间时为无穷）。
// 窗口之外一律算没数据：一个区间在 T 被一次可信运行关闭，只证明 T 那一
// 刻这个 IP 空着，不证明 T 之后我们还在看。同理，这个 IP 一个区间都没有
// 时，"从来没有过 Pod"与"那段时间没在采集"在本函数的入参里无法区分 ——
// 说成 NOT_COVERED 才是那个让人放心的方向，因此一律 NO_DATA。
func withinObservedWindow(intervals []Interval, at time.Time) bool {
	if len(intervals) == 0 {
		return false
	}
	var lo, hi time.Time
	open := false
	for i, iv := range intervals {
		if i == 0 || iv.ValidFrom.Before(lo) {
			lo = iv.ValidFrom
		}
		if iv.Open() {
			open = true
			continue
		}
		if iv.ValidTo.After(hi) {
			hi = iv.ValidTo
		}
	}
	if at.Before(lo) {
		return false
	}
	if open {
		return true
	}
	return !at.After(hi)
}
