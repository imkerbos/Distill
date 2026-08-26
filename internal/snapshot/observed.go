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
	// AdminPolicies 是集群现存的 ANP 与 BANP，**只存不解释**。
	AdminPolicies []AdminPolicy

	// ForeignScopes 是平台**不解释**的策略平面所覆盖的主体范围
	// （design doc 2026-08-25 §2）。
	//
	// 跟着这一次快照走，而不是记在集群登记上：CNP 的覆盖范围会变，
	// 今天那条选中 A、昨天那条可能选中 B。按"当前范围"去解释历史窗口，
	// B 那一批就不会被降级 —— 而那正是它需要被降级的时候
	// （CLAUDE.md §4：禁止用当前状态解释历史数据）。
	ForeignScopes []ForeignScope
	// ForeignScopesComplete 表示上面那份范围是**完整**的。
	//
	// **为 false 时判定必须整片降级**，不得只降 ForeignScopes 里那些：
	// 范围不完整意味着有主体被覆盖而我们不知道是哪些，漏掉一个就是把一条
	// 真的被管着的连接判成可信。
	//
	// 平面存在而范围不完整是常态，不是异常 —— 平台只解析它确定算得出来
	// 的那部分（matchExpressions、非 k8s 标签来源、AdminNetworkPolicy
	// 一族都算不出）。
	ForeignScopesComplete bool

	// Warnings 是采集当时发现的问题。
	//
	// 与 error 分开：这些不是采集失败，是采到的事实与登记不符。
	// 当作错误会让一次成功的采集被丢掉，忽略则会让"注册表填错了"
	// 一直等到求值阶段才以错误归属的形式表现出来。
	Warnings []Warning
}

// ForeignScope 是一条平台不解释的策略所覆盖的主体范围。
//
// 只带 namespace 与一组标签相等条件 —— 这是从 CiliumNetworkPolicy 的
// endpointSelector 里确定算得出来的部分。它**不描述那条策略放行了什么**，
// 那是第二套求值引擎的事。
type ForeignScope struct {
	// Namespace 为空表示集群级，跨全部 namespace 生效。
	Namespace string
	// MatchLabels 为空表示选中该范围内全部主体。
	MatchLabels map[string]string
}

// PodAddress 是 Pod 的一个地址及其归属判定。
//
// 归属跟着地址走，不跟着 Pod 走：双栈 Pod 的两个地址可能落在不同的登记网段
// 里（甚至一个在登记内、一个在登记外），共用一个 Scope 会让其中一个的结论
// 被另一个覆盖。
type PodAddress struct {
	IP    string
	Scope cluster.Scope
	// Reason 仅在 Scope 判不出来时非空，取值与主地址那一列同一套封闭枚举。
	Reason cluster.Reason
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
	// ExtraIPs 是这个 Pod 的**其余**地址，双栈 Pod 在此带上另一个协议族。
	//
	// Kubernetes 的 `status.podIPs` 是一个数组，`status.podIP` 等于它的第一项。
	// 只读后者的话，双栈 Pod 的第二个地址不进快照、也不进身份区间表 ——
	// 走那个地址的连接**解不出主体**、判 UNKNOWN，覆盖它的规则于是缺席，
	// 下发 default-deny 之后会被拦断。方向危险，因此必须带全。
	//
	// 主地址仍留在 IP 里、不并进来：现有的每一条按 IP 的查询与展示都指着它，
	// 而单栈集群（绝大多数）的这一项恒为空，形状完全不变。
	ExtraIPs []PodAddress
	// IPScope 是**主地址** IP 在 Fleet 内的归属判定，空表示 IP 为空或无法解析。
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
	// ScrapeAnnotations 是这个 Pod 自己声明的 metrics 抓取意愿。
	//
	// **只装白名单里的那几个键**（见 collect.ScrapeAnnotationKeys），不是
	// 整批 annotations：kubectl.kubernetes.io/last-applied-configuration 里
	// 是整份 manifest —— 体积上是 labels 的几十倍，内容上可能带着 env 里的
	// 口令与内网地址，而这个库会被导出到事实层长期留存（V4 spec §9.9）。
	//
	// 它是 METRICS_SCRAPE Baseline 的依据的一半：这一半说的是"谁愿意被抓"，
	// 另一半"谁来抓"由集群登记给出，两者都不许猜
	// （design doc 2026-08-18 §3）。
	ScrapeAnnotations map[string]string
}

// ScrapeAnnotationScrape 是被抓端表达意愿的注解键。
const ScrapeAnnotationScrape = "prometheus.io/scrape"

// DeclaresScrape 报告这个 Pod 是否声明了自己要被抓。
//
// **判据是取值等于 "true"，不是注解存在。** `prometheus.io/scrape: "false"`
// 是一句明确的"别抓我"，把它读成一次声明会让那个 namespace 的
// METRICS_SCRAPE 永远显得适用而又推不出规则 —— 一道永远在挡的门。
//
// 收成一个方法而不是让每个消费方各写一次比较：适用性判定与规则推导必须
// 用同一条判据，各写一份就会出现"这一类适用、却永远推不出规则"的死角。
func (p Pod) DeclaresScrape() bool {
	return p.ScrapeAnnotations[ScrapeAnnotationScrape] == "true"
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

// AdminPolicyKind 是管理面策略的种类，封闭枚举。
//
// 两者是同一个 API 组下的两种对象，**求值次序完全不同**：ANP 在标准
// NetworkPolicy 之前生效，BANP 在其之后兜底。把它们混作一类，会让一条
// 兜底规则被当成前置规则解释，方向恰好相反。
type AdminPolicyKind string

const (
	// AdminPolicyAdmin 是 AdminNetworkPolicy，带 priority，先于 NetworkPolicy 生效。
	AdminPolicyAdmin AdminPolicyKind = "ADMIN_NETWORK_POLICY"
	// AdminPolicyBaseline 是 BaselineAdminNetworkPolicy，无 priority，在 NetworkPolicy 之后兜底。
	AdminPolicyBaseline AdminPolicyKind = "BASELINE_ADMIN_NETWORK_POLICY"
)

// AdminPolicy 是集群里一条管理面策略的原文快照。
//
// 本轮**只存不解释**：落库让"这个集群上有哪些 ANP、当时长什么样"成为可以
// 回看的事实，求值仍然照旧整片降级。先存后解释而不是一步到位，是因为
// ANP 的求值是有序短路的（ANP → NP → BANP），而一个只对了一半的次序实现
// 会给出一个自信的错答案 —— 那比不解释更危险。
type AdminPolicy struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Kind 区分 ANP 与 BANP。
	Kind AdminPolicyKind
	// Name 是策略名。这两类都是集群级对象，没有 namespace。
	Name string
	// UID 区分同名重建的策略。
	UID string
	// Priority 是 ANP 的优先级，**数值小的先生效**。
	Priority int32
	// PriorityKnown 表示上面那个数是真的读到了。
	//
	// 与 Priority 分开而不是拿 0 表示"没有"：0 是一个合法的 ANP 优先级，
	// 而且是**最高**的那一个。合并之后，一条读不出优先级的策略会被当成
	// 优先级最高的策略排到最前面 —— 错得最狠的那个方向。BANP 没有优先级，
	// 这里恒为 false。
	PriorityKnown bool
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
