// Package snapshot 定义资产快照的纯类型。
//
// 与 replay 的 PodRef / NamespaceRef 分开：那两个是求值所需的最小视图，
// 这里是策略推导所需的基础设施对象（Service、Gateway、抓取端、
// apiserver 端点）。Baseline 必须带推导依据入库而非硬编码常量表，
// 依据就出自这些对象。
package snapshot

// Service 是一个 Service 的快照视图。
type Service struct {
	// ClusterID 是所属集群。不同集群的同名 Service 是不同对象。
	ClusterID string
	// Namespace 是所属命名空间。
	Namespace string
	// Name 是 Service 名。
	Name string
	// Type 是 Service 类型，如 ClusterIP、LoadBalancer。
	Type string
	// Selector 是后端 Pod 选择器。
	//
	// Baseline 推导取的是它而非 ClusterIP：NetworkPolicy 的 peer 只能是
	// selector 或 ipBlock，ClusterIP 两者都不是，写进去永远匹配不上。
	Selector map[string]string
	// ClusterIP 是虚拟 IP，仅供展示与追溯，不参与规则生成。
	ClusterIP string
	// Ports 是 Service 暴露的端口。
	Ports []ServicePort
}

// ServicePort 是 Service 的一个端口映射。
type ServicePort struct {
	// Name 是端口名。
	Name string
	// Port 是 Service 端口。
	Port int32
	// TargetPort 是后端 Pod 端口，规则生成用的是它。
	TargetPort int32
	// Protocol 是传输层协议，取值 TCP / UDP / SCTP。
	Protocol string
}

// Endpoints 是一个 Service 的后端地址快照。
//
// 单独建模而非合进 Service：Baseline 生成 DNS 规则前必须确认后端非空。
// 一个存在但没有后端的 kube-dns Service 会生成一条指向空集的放行规则，
// 看起来齐备、实际什么都没放行。
type Endpoints struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Namespace 是所属命名空间。
	Namespace string
	// Name 与对应 Service 同名。
	Name string
	// Addresses 是后端地址。
	Addresses []string
	// Ports 是后端端口。
	Ports []int32
}

// Gateway 是一个入口暴露对象的快照，涵盖 Gateway 与 Ingress。
//
// 合成一个类型：两者在 Baseline 推导中扮演同一角色 —— 指出哪些 workload
// 是外部入口，因而需要放行健康检查。分成两个类型会让推导逻辑写两遍。
type Gateway struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Namespace 是所属命名空间。
	Namespace string
	// Name 是对象名。
	Name string
	// Kind 是来源类型，取值 Gateway 或 Ingress，供推导依据回溯。
	Kind string
	// BackendService 是后端 Service 名，位于同一 namespace。
	BackendService string
}

// ScrapeTarget 是一个 metrics 抓取关系的快照。
//
// 记录抓取端而非被抓取端：规则要放行的是"谁来抓"，被抓取端是规则的主体。
type ScrapeTarget struct {
	// ClusterID 是所属集群。
	ClusterID string
	// ScraperNamespace 是抓取端所在命名空间。
	ScraperNamespace string
	// ScraperLabels 是抓取端 Pod 的标签，用作 podSelector。
	ScraperLabels map[string]string
	// TargetNamespace 是被抓取的命名空间。
	TargetNamespace string
	// TargetPort 是被抓取的 metrics 端口。
	TargetPort int32
}

// APIServerEndpoint 是集群 API server 的访问端点。
type APIServerEndpoint struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Host 是端点地址，仅供展示与追溯。
	Host string
	// CIDR 是端点所在网段，规则生成用的是它。
	CIDR string
	// Port 是访问端口。
	Port int32
}

// NodeAgent 是一个节点级 agent 的快照。
type NodeAgent struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Namespace 是所属命名空间。
	Namespace string
	// App 是 agent 的 app 标签值。
	App string
	// HostNetwork 表示该 agent 使用宿主网络。
	//
	// 为 true 时规则必须走 node CIDR 而非 podSelector：源地址是节点 IP，
	// podSelector 永远选不中它。写成 podSelector 会得到一条看起来正确、
	// 实际从不匹配的规则，上线后监控与日志静默中断。
	HostNetwork bool
	// TargetPort 是 agent 访问的目标端口。
	TargetPort int32
}

// ClusterRegistry 是集群的网段注册信息。
//
// 健康检查网段登记在这里而非写死在推导代码里：网段会变，硬编码的
// 常量表不会跟着变，且没人知道它当初是怎么来的（spec §7.2）。
type ClusterRegistry struct {
	// ClusterID 是集群标识。
	ClusterID string
	// PodCIDR 是 Pod 网段。
	PodCIDR string
	// NodeCIDR 是节点网段，节点级 agent 的 Baseline 取它。
	NodeCIDR string
	// HealthCheckSources 是负载均衡健康检查的来源网段。
	HealthCheckSources []string
}

// Assets 是一个集群的全部资产快照。
type Assets struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Services 是 Service 快照。
	Services []Service
	// Endpoints 是 Endpoints 快照。
	Endpoints []Endpoints
	// Gateways 是入口暴露对象快照。
	Gateways []Gateway
	// ScrapeTargets 是 metrics 抓取关系快照。
	ScrapeTargets []ScrapeTarget
	// APIServers 是 API server 端点快照。
	APIServers []APIServerEndpoint
	// NodeAgents 是节点级 agent 快照。
	NodeAgents []NodeAgent
	// Registry 是集群网段注册信息。
	Registry ClusterRegistry
}
