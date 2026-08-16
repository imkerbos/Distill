package snapshot

import "time"

// Run 是一次采集运行的完整结果。
//
// Observation 与 Failures 并列而非二选一：部分资源失败时，采到的那部分
// 仍然有价值，但它必须带着"缺了什么"一起交出去。
//
// 定义在纯类型层而非采集层：落库只需要这些字段，让它为了这个类型去
// import 采集包，等于把 client-go 拖进存储层的依赖图。
type Run struct {
	// Observation 是采到的资产。
	Observation Observation
	// Failures 是各类资源的失败记录。
	Failures []Failure
	// Status 是本次运行的结果判定。
	Status RunStatus
	// ErrorReason 仅在这一轮**根本没能开始采集**时非空。
	//
	// 它非空时 Observation 里不会有任何资产，Failures 也是空的 ——
	// 因为一个资源都没被尝试过。这与"尝试了、全都失败"是两回事。
	ErrorReason RunErrorReason
	// StartedAt 与 FinishedAt 界定这次运行占用的时间。
	StartedAt time.Time
	// FinishedAt 是运行结束时刻。
	FinishedAt time.Time
}

// RunErrorReason 是一次运行**根本没能开始采集**的原因，封闭枚举。
//
// 与 Failure.Reason 是两件事，不得合并：后者说的是"某一类资源没采到"，
// 前者说的是"这一轮从来没读到过集群"。合并会让"NetworkPolicy 被拒"
// 与"采集器连不上这个集群"落进同一个统计口径。
//
// 它存在的理由是这条端点最容易骗人的一种状态：采集器被拉起、在读集群
// 之前就失败退出，于是一行 collection_run 也没有，界面显示"这个集群还
// 没有过任何一次资产采集"—— 与一个刚注册、采集器压根没跑过的集群一模
// 一样。操作者会去等一次永远不会成功的采集。
type RunErrorReason string

const (
	// RunErrorNone 表示这一轮真的读到了集群，成败见 RunStatus。
	RunErrorNone RunErrorReason = ""
	// RunErrorCredentialUnavailable 表示凭据引用缺失、非法或解析不出来。
	//
	//nolint:gosec // G101 false positive: 这是一个落库的原因枚举值，不是凭据。
	RunErrorCredentialUnavailable RunErrorReason = "CREDENTIAL_UNAVAILABLE"
	// RunErrorClientUnavailable 表示凭据拿到了，但构造不出可用的客户端 ——
	// 包括 apiserver 地址被出站守卫拒绝。
	RunErrorClientUnavailable RunErrorReason = "CLIENT_UNAVAILABLE"
	// RunErrorReadOnlyUnproven 表示这次运行没能证明自己对目标集群只读，
	// 因而一个资源都没有读。
	RunErrorReadOnlyUnproven RunErrorReason = "READ_ONLY_UNPROVEN"
)

// ResourceKind 是一类被采集的 Kubernetes 资源，封闭枚举。
type ResourceKind string

const (
	// ResourceNamespace 是 Namespace。
	ResourceNamespace ResourceKind = "NAMESPACE"
	// ResourcePod 是 Pod。
	ResourcePod ResourceKind = "POD"
	// ResourceNode 是 Node。
	ResourceNode ResourceKind = "NODE"
	// ResourceReplicaSet 是 ReplicaSet，只为解 ownerRef 链而采。
	ResourceReplicaSet ResourceKind = "REPLICASET"
	// ResourceService 是 Service。
	ResourceService ResourceKind = "SERVICE"
	// ResourceEndpointSlice 是 EndpointSlice。
	ResourceEndpointSlice ResourceKind = "ENDPOINTSLICE"
	// ResourceNetworkPolicy 是 NetworkPolicy。
	ResourceNetworkPolicy ResourceKind = "NETWORKPOLICY"
	// ResourceIngress 是 Ingress。
	ResourceIngress ResourceKind = "INGRESS"
)

// FailureReason 是一类资源采集失败的原因，封闭枚举。
//
// FORBIDDEN 单独成项而不并进 OTHER：权限不足是唯一一种"集群是好的、
// 是我们没被授权"的失败，处置方式（改 RBAC）与其余几种完全不同，
// 而它恰恰是最容易被当成偶发错误重试掉的那种。
type FailureReason string

const (
	// FailureForbidden 表示采集身份没有该资源的读权限。
	FailureForbidden FailureReason = "FORBIDDEN"
	// FailureNotFound 表示该资源类型在集群里不存在，通常是 CRD 未安装。
	FailureNotFound FailureReason = "NOT_FOUND"
	// FailureTimeout 表示请求超时。
	FailureTimeout FailureReason = "TIMEOUT"
	// FailureUnavailable 表示 apiserver 不可达或拒绝服务。
	FailureUnavailable FailureReason = "UNAVAILABLE"
	// FailureOther 是上述之外的失败。
	FailureOther FailureReason = "OTHER"
)

// Failure 是一类资源的采集失败记录。
type Failure struct {
	// Resource 是失败的资源类型。
	Resource ResourceKind
	// Reason 是失败原因。
	Reason FailureReason
	// Detail 是补充说明，仅供操作者阅读，不参与统计。
	Detail string
}

// RunStatus 是一次采集运行的结果，封闭枚举。
type RunStatus string

const (
	// RunOK 表示全部资源都采到了。
	RunOK RunStatus = "OK"
	// RunPartial 表示部分资源采到了，其余失败。
	//
	// 必须与 OK 区分：采到 Pod 但 NetworkPolicy 权限不足时，得到的是一份
	// 缺了策略的快照。报成 OK 会让下游把它当作完整事实使用，
	// 于是"这个集群没有任何策略"与"我们没被授权看策略"变得无法区分。
	RunPartial RunStatus = "PARTIAL"
	// RunFailed 表示没有任何资源采到。
	RunFailed RunStatus = "FAILED"
)

// DeriveRunStatus 由尝试过的资源类型与失败记录判定本次运行的结果。
//
// attempted 为空时返回 FAILED：一次什么都没尝试的运行不是成功，
// 而"循环体从没执行过"正是这种缺陷最常见的成因。
func DeriveRunStatus(attempted []ResourceKind, failures []Failure) RunStatus {
	switch {
	case len(attempted) == 0:
		return RunFailed
	case len(failures) == 0:
		return RunOK
	case len(failures) >= len(attempted):
		return RunFailed
	default:
		return RunPartial
	}
}

// Counts 返回本次观测中各类资源的记录条数。
//
// 返回 map 而非固定字段的结构体：落库时一类资源一行，
// 新增一种资源不需要改表结构，也就不会出现"表里少一列、
// 而少的那一列恰好是这次没采到的那类"。
func (o Observation) Counts() map[ResourceKind]int {
	return map[ResourceKind]int{
		ResourceNamespace:     len(o.Namespaces),
		ResourcePod:           len(o.Pods),
		ResourceNode:          len(o.Nodes),
		ResourceService:       len(o.Services),
		ResourceEndpointSlice: len(o.Endpoints),
		ResourceNetworkPolicy: len(o.Policies),
		ResourceIngress:       len(o.Gateways),
	}
}
