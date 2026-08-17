// Package identity 把离散的 Pod 观测收敛成可按时刻查询的身份区间，
// 并回答「在时刻 T，这个 IP 是谁」。
//
// 纯逻辑：零 I/O、零框架依赖，只依赖标准库。Pod IP 会被回收，拿当前快照
// 解释过去的流量会把一条本该归给 A 的连接算到 B 头上 —— 而且不报错。
// 因此本包的规则与 internal/cluster 同源，两条都是"宁可不答"：
// 重叠区间返回 AMBIGUOUS 而不任选其一；没有区间覆盖那一刻时区分
// "那时这个 IP 没有 Pod"与"平台那段时间没在看"，绝不回退到最近的区间。
//
// 还有一条比上面两条更靠前：一次没采到 Pod 的运行不得关闭任何区间。
// 把 FORBIDDEN 读成"Pod 没了"，会让平台认为一批工作负载已经下线，
// 于是它们的策略被判成"无流量、可收紧"—— 一次权限故障变成一次生产
// 阻断建议。这是"结果比现实好看"的方向，本平台必须往反方向错。
package identity

import (
	"cmp"
	"slices"
	"time"
)

// RunStatus 是一次采集运行的结果，封闭枚举。
//
// 取值与 internal/snapshot.RunStatus 逐字相同，但在这里重新声明：本包
// 必须保持只直接依赖标准库，而这几个取值是它判断"能不能关区间"的唯一
// 依据。边缘层负责把落库的状态映射进来。
type RunStatus string

const (
	// RunOK 表示全部资源都采到了。
	RunOK RunStatus = "OK"
	// RunPartial 表示部分资源采到了，其余失败。
	RunPartial RunStatus = "PARTIAL"
	// RunFailed 表示没有任何资源采到。
	RunFailed RunStatus = "FAILED"
)

// ClosedReason 是一个区间被关闭的原因，封闭枚举。
//
// 不用自由文本：关闭动作是"这个 Pod 不在了"这一结论的落点，事后要能按
// 原因聚合追溯。新增取值要同步更新统计口径。
type ClosedReason string

const (
	// ClosedNone 表示区间尚未关闭。
	ClosedNone ClosedReason = ""
	// ClosedAbsentInLaterRun 表示一次可信运行没有再看到这个 Pod。
	//
	// IP 被另一个 Pod 接手也走这一条：那个 Pod 同样没有出现在这次观测里。
	// 交接本身由新区间的起点记录，不需要第二个原因。
	ClosedAbsentInLaterRun ClosedReason = "ABSENT_IN_LATER_RUN"
)

// Identity 是一个 IP 在某段时间背后的通信主体。
//
// 带上 WorkloadKind / WorkloadName 而不只是 Pod：对账一律用 workload 而非
// pod，ReplicaSet 每次发布都换名字，用它当主体会让同一个服务在发布前后
// 变成两个东西。
type Identity struct {
	// Namespace 是 Pod 所在命名空间。
	Namespace string
	// PodName 是 Pod 名。
	PodName string
	// PodUID 区分同名重建的 Pod。
	PodUID string
	// WorkloadKind 是沿 ownerRef 链解到顶层的控制器类型。
	WorkloadKind string
	// WorkloadName 是顶层控制器名。
	WorkloadName string
	// HostNetwork 表示该 Pod 使用宿主网络，不受 NetworkPolicy 管控。
	HostNetwork bool
	// InMesh 表示该 Pod 注入了 sidecar，其 L4 身份不可信。
	InMesh bool
}

// ObservedPod 是一次采集运行里看到的一个 Pod。
//
// 不带 ClusterID：一次运行只采一个集群，集群挂在 RunFacts 上。放进每条
// 记录会让同一次运行的观测有机会带上互不相同的集群。
type ObservedPod struct {
	// PodIP 是观测到的 Pod IP，空表示尚未分配（Pending）。
	PodIP string
	// Identity 是这一刻这个 IP 背后的主体。
	Identity Identity
}

// Interval 是同一个 IP 归属同一个主体的一段时间。
//
// 半开区间 [ValidFrom, ValidTo)：一个 IP 被回收时，旧区间的关闭时刻与新
// 区间的起始时刻是同一个观测点，闭区间会让每一次交接都在那一刻重叠成
// AMBIGUOUS —— 把一次正常的回收报成数据问题。
type Interval struct {
	// ClusterID 是所属集群。不同集群 Pod CIDR 可能重叠，缺它会解到别的集群。
	ClusterID string
	// PodIP 是这段时间里指向该主体的地址。
	PodIP string
	// ValidFrom 是首次观测到这个归属的时刻。
	ValidFrom time.Time
	// ValidTo 是关闭时刻，零值表示至今未关闭。
	ValidTo time.Time
	// Identity 是这段时间里该 IP 背后的主体。
	Identity Identity
	// FirstRunID 是开启这个区间的那次运行。
	FirstRunID string
	// LastRunID 是最后一次触及这个区间的运行 —— 看见它，或者关闭它。
	//
	// 关闭必须记下是哪一次运行关的：一个区间被关掉意味着平台认定这个
	// 工作负载下线了，事后要能追到那个结论是谁下的。
	LastRunID string
	// ClosedReason 是关闭原因，未关闭时为空。
	ClosedReason ClosedReason
}

// Open 判断该区间是否尚未关闭。
func (iv Interval) Open() bool {
	return iv.ValidTo.IsZero()
}

// Covers 判断该区间是否覆盖时刻 at，按半开区间 [ValidFrom, ValidTo) 判定。
func (iv Interval) Covers(at time.Time) bool {
	if at.Before(iv.ValidFrom) {
		return false
	}
	if iv.Open() {
		return true
	}
	return at.Before(iv.ValidTo)
}

// RunFacts 是推导需要知道的、关于一次采集运行的全部事实。
//
// Status 与 PodsCollected 分开传而不是由调用方合成一个布尔："这次运行可
// 不可信"这个判断必须落在本包，否则守卫会被测到，却没有任何东西证明调
// 用方还在调用它。
type RunFacts struct {
	// ClusterID 是这次运行采集的集群。
	ClusterID string
	// RunID 标识这一次采集运行。
	RunID string
	// ObservedAt 是这次采集的时刻，本次开启与关闭的区间共用这个端点。
	ObservedAt time.Time
	// Status 是这次运行的整体判定。
	Status RunStatus
	// PodsCollected 表示 Pod 这一类资源本次采集成功。
	//
	// 与 Status 分开：Pod 采集失败而其余成功时 Status 是 PARTIAL，但
	// "计数为 0"与"压根没采到"必须一直分到这一层。
	PodsCollected bool
}

// closesIntervals 判断这次运行是否有资格关闭区间。
//
// 显式 switch 而非查表：新增一个状态却没在这里列出来，它就不是一个可信
// 状态，而这一行的缺失在评审里看得见。未登记的取值（含零值）一律不可
// 信 —— 一个状态没被赋过值的运行必须什么区间都关不了。
func (r RunFacts) closesIntervals() bool {
	switch r.Status {
	case RunOK:
		return r.PodsCollected
	case RunPartial, RunFailed:
		return false
	default:
		return false
	}
}

// intervalKey 是"同一个 IP 上的同一个主体"，推导用它判断区间是否延续。
type intervalKey struct {
	podIP    string
	identity Identity
}

// Derive 由已有区间、一次运行的 Pod 观测与这次运行的事实，推导出新的区间集合。
//
// 幂等：重试、补跑与并发都会让同一批输入被推导多次，把上一次的产物再喂
// 一遍必须得到逐字节相同的结果。因此已有开区间在本次仍被观测到时原样保
// 留，而不是再开一个。
//
// prev 里不属于本次运行集群的区间原样带出：Pod CIDR 跨集群可能重叠，
// 让一个集群的运行去关另一个集群的区间，正是"join 到错误的 Pod 上且不
// 报错"的那种错法。
func Derive(prev []Interval, obs []ObservedPod, run RunFacts) []Interval {
	observed := make(map[intervalKey]struct{}, len(obs))
	for _, o := range obs {
		// IP 为空的 Pod（Pending）没有可归属的地址，不产生区间。
		if o.PodIP == "" {
			continue
		}
		observed[intervalKey{podIP: o.PodIP, identity: o.Identity}] = struct{}{}
	}

	closes := run.closesIntervals()
	out := make([]Interval, 0, len(prev)+len(obs))
	// covered 记下已经有开区间的 (IP, 主体)，它同时挡掉两件事：为一个已在
	// 延续的区间再开一个，以及同一次观测里的重复条目各开一个。
	covered := make(map[intervalKey]struct{}, len(prev)+len(obs))

	for _, iv := range prev {
		if iv.ClusterID != run.ClusterID || !iv.Open() {
			out = append(out, iv)
			continue
		}
		k := intervalKey{podIP: iv.PodIP, identity: iv.Identity}
		if _, seen := observed[k]; seen {
			covered[k] = struct{}{}
			iv.LastRunID = run.RunID
			out = append(out, iv)
			continue
		}
		// 不可信的运行一个区间都不关。这条排在最前面：它比下面那条
		// 时序守卫更重要，也更容易被"顺手放宽"掉。
		if !closes {
			out = append(out, iv)
			continue
		}
		// 起点不早于本次观测的区间，本次运行没有资格关：它要么比这个区间
		// 还老（乱序或并发落库），要么正好开在这一刻。关掉会得到一个
		// ValidTo 不晚于 ValidFrom 的区间 —— 一段谁也解释不了的时间。
		if !iv.ValidFrom.Before(run.ObservedAt) {
			out = append(out, iv)
			continue
		}
		iv.ValidTo = run.ObservedAt
		iv.LastRunID = run.RunID
		iv.ClosedReason = ClosedAbsentInLaterRun
		out = append(out, iv)
	}

	for _, o := range obs {
		if o.PodIP == "" {
			continue
		}
		k := intervalKey{podIP: o.PodIP, identity: o.Identity}
		if _, done := covered[k]; done {
			continue
		}
		covered[k] = struct{}{}
		out = append(out, Interval{
			ClusterID:  run.ClusterID,
			PodIP:      o.PodIP,
			ValidFrom:  run.ObservedAt,
			Identity:   o.Identity,
			FirstRunID: run.RunID,
			LastRunID:  run.RunID,
		})
	}

	slices.SortStableFunc(out, compareIntervals)
	return out
}

// compareIntervals 给区间一个全序，让推导的产物与入参顺序无关。
//
// 顺序只为可复现，不表达语义：PodIP 按字符串比，不是按地址大小。落库行
// 的顺序若随查询返回顺序变化，"同一批快照重跑结果逐字节相同"就无法验证。
func compareIntervals(a, b Interval) int {
	if c := cmp.Compare(a.ClusterID, b.ClusterID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.PodIP, b.PodIP); c != 0 {
		return c
	}
	if c := a.ValidFrom.Compare(b.ValidFrom); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Identity.Namespace, b.Identity.Namespace); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Identity.PodName, b.Identity.PodName); c != 0 {
		return c
	}
	return cmp.Compare(a.Identity.PodUID, b.Identity.PodUID)
}
