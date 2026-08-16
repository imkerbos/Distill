package snapshot

import (
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
)

// Observation 是一次采集运行的全部产物。
//
// ClusterID / RunID / ObservedAt 挂在这一层而非每条记录上：它们描述的是
// 这次采集，不是被采集的对象。放进每行会让同一次运行的记录有机会带上
// 互不相同的时刻，而"这两条记录是不是同一次看到的"正是历史回放的判据。
type Observation struct {
	// ClusterID 是被采集的集群。
	ClusterID string
	// RunID 标识这一次采集运行，落库后用于把各类记录连回同一次运行。
	RunID string
	// ObservedAt 是这次采集的时刻，全部记录共用。
	ObservedAt time.Time

	// Namespaces 是命名空间快照。
	Namespaces []Namespace
	// Pods 是 Pod 快照。
	Pods []Pod
	// Nodes 是节点快照，集群真实网段的事实来源。
	Nodes []Node
	// Services 是 Service 快照。
	Services []Service
	// Endpoints 是 Service 后端快照，来自 EndpointSlice。
	Endpoints []Endpoints
	// Policies 是集群现存的 NetworkPolicy。
	Policies []NetworkPolicy
	// Gateways 是入口暴露对象快照，本轮只含 Kind=Ingress。
	Gateways []Gateway

	// Warnings 是采集当时发现的问题。
	//
	// 与 error 分开：这些不是采集失败，是采到的事实与登记不符。
	// 当作错误会让一次成功的采集被丢掉，忽略则会让"注册表填错了"
	// 一直等到求值阶段才以错误归属的形式表现出来。
	Warnings []Warning
}

// Namespace 是一个命名空间的观测快照。
type Namespace struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Name 是命名空间名。
	Name string
	// Labels 是命名空间标签，NetworkPolicy 的 namespaceSelector 依赖它。
	Labels map[string]string
	// InMesh 表示该命名空间配置了 sidecar 注入。
	//
	// 这是 namespace 的配置，不是其中每个 Pod 的实际状态 —— 后者见 Pod.InMesh。
	InMesh bool
	// MeshSource 是 InMesh 的依据类别。
	MeshSource cluster.MeshSource
	// MeshDetail 是依据的具体取值。
	MeshDetail string
}

// Pod 是一个 Pod 的观测快照。
type Pod struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Namespace 是所在命名空间。
	Namespace string
	// Name 是 Pod 名。
	Name string
	// UID 区分同名重建的 Pod。
	UID string
	// Phase 是 Pod 生命周期阶段，用于解释 IP 为空的记录。
	Phase string
	// IP 是 Pod IP。Pending 的 Pod 尚未分配，此处为空。
	IP string
	// IPScope 是 IP 在 Fleet 内的归属判定，空表示 IP 为空或无法解析。
	IPScope cluster.Scope
	// IPScopeReason 仅在 IPScope 为 UNKNOWN 时非空。
	IPScopeReason cluster.Reason
	// Labels 是 Pod 标签，用于恢复历史 selector 语义。
	Labels map[string]string
	// HostNetwork 表示该 Pod 使用宿主网络，不受 NetworkPolicy 管控。
	HostNetwork bool
	// NodeName 是所在节点。
	NodeName string
	// ServiceAccount 是绑定的服务账号。
	ServiceAccount string
	// OwnerKind 与 OwnerName 是直接控制器，通常是 ReplicaSet。
	OwnerKind string
	// OwnerName 是直接控制器名。
	OwnerName string
	// WorkloadKind 与 WorkloadName 是沿 ownerRef 链解到顶层的控制器。
	//
	// 与 Owner 分开保留：对账一律用 workload 而非 pod，而 ReplicaSet 每次
	// 发布都换名字，用它当主体会让同一个服务在发布前后变成两个东西。
	WorkloadKind string
	// WorkloadName 是顶层控制器名。
	WorkloadName string
	// InMesh 表示该 Pod 实际注入了 sidecar，其 L4 身份不可信。
	InMesh bool
	// MeshSource 是 InMesh 的依据类别。
	MeshSource cluster.MeshSource
	// MeshDetail 是依据的具体取值。
	MeshDetail string
}

// Node 是一个节点的观测快照。
//
// 采它是为了拿到集群真实网段：PodCIDRs 与 InternalIPs 是集群自己报的事实，
// 而注册表里的网段是人填的。两者不一致时，错的是注册表。
type Node struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Name 是节点名。
	Name string
	// PodCIDRs 是分配给该节点的 Pod 网段。
	PodCIDRs []string
	// InternalIPs 是节点内网地址。
	InternalIPs []string
}

// NetworkPolicy 是集群现存的一条 NetworkPolicy。
//
// 保留完整 manifest 而非解析后的结构：这是"集群当时是什么样"的证据，
// 解析可以重做，丢掉的字段找不回来。
type NetworkPolicy struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Namespace 是所在命名空间。
	Namespace string
	// Name 是策略名。
	Name string
	// UID 区分同名重建的策略。
	UID string
	// Manifest 是该策略的 YAML 原文。
	Manifest string
}

// WarningKind 是采集告警的类别，封闭枚举。
//
// 不用自由文本：告警条数与构成要进可见面与统计口径，
// 自由文本会让"有多少 Pod 的 IP 落在登记网段外"变成一次字符串匹配。
type WarningKind string

const (
	// WarningPodIPOutsideCluster 表示 Pod IP 落在它自己集群的登记网段之外。
	// 成因通常是注册表里的网段填错或集群扩了新网段。
	WarningPodIPOutsideCluster WarningKind = "POD_IP_OUTSIDE_CLUSTER"
	// WarningPodIPAmbiguous 表示 Pod IP 同时命中多个集群的登记网段。
	WarningPodIPAmbiguous WarningKind = "POD_IP_AMBIGUOUS"
	// WarningPodIPUnclassifiable 表示登记不足以判定该 Pod IP 的归属。
	WarningPodIPUnclassifiable WarningKind = "POD_IP_UNCLASSIFIABLE"
	// WarningPodIPUnparsable 表示 Pod IP 不是一个合法地址。
	WarningPodIPUnparsable WarningKind = "POD_IP_UNPARSABLE"
	// WarningServiceWithoutEndpoints 表示 Service 存在但没有后端地址。
	// 照它生成的放行规则指向空集：看起来齐备，实际什么都没放行。
	WarningServiceWithoutEndpoints WarningKind = "SERVICE_WITHOUT_ENDPOINTS"
	// WarningWorkloadUnresolved 表示 ownerRef 链没能解到顶层控制器。
	WarningWorkloadUnresolved WarningKind = "WORKLOAD_UNRESOLVED"
)

// Warning 是采集当时发现的一个问题。
type Warning struct {
	// Kind 是告警类别。
	Kind WarningKind
	// Subject 是出问题的对象，形如 namespace/name。
	Subject string
	// Detail 是补充说明，仅供操作者阅读，不参与统计。
	Detail string
}
