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
	// LoadBalancerIngressIPs 是 LoadBalancer 实际分配到的入口地址。
	//
	// **它是判定暴露范围的依据**：地址落在已登记网段内说明这个入口只在
	// VPC 内可达，落在网段之外说明它面向公网（design doc 2026-08-28 §2）。
	// 取它而不是读云厂商注解 —— GKE 用 networking.gke.io/load-balancer-type，
	// AWS、阿里云各不相同，而漏掉一个键的后果是把内部 LB 当成公网入口。
	//
	// 为空表示这个 Service 没有 LoadBalancer 入口：ClusterIP、NodePort，
	// 或者 LB 尚未就绪。三者在这里不区分，由推导层按 Type 分流。
	LoadBalancerIngressIPs []string
	// LoadBalancerSourceRanges 是 Service 声明的允许来源网段。
	//
	// 声明过就用它，优先于按入口地址推出来的范围：运维显式写下的范围比
	// 平台推导的更准，而两者不一致时推导的那个只会更宽。
	LoadBalancerSourceRanges []string
}

// ServicePort 是 Service 的一个端口映射。
type ServicePort struct {
	// Name 是端口名。
	Name string
	// Port 是 Service 端口。
	Port int32
	// TargetPort 是后端 Pod 端口，规则生成用的是它。
	// targetPort 为命名端口时此处为 0，端口名在 TargetPortName。
	TargetPort int32
	// TargetPortName 是命名形式的后端端口，为空表示 targetPort 是数字。
	//
	// 与 TargetPort 分成两个字段：命名端口必须解析到具体 Pod 才知道是哪个
	// 数字，合成一个 int32 会把它静默记成 0 —— 而 0 是合法端口值，
	// 一条指向端口 0 的规则永远匹配不上，外观却完全正常。
	TargetPortName string
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

// ScrapeDeclaration 是一个 Pod 声明「我要被抓」这件观测事实。
//
// 只记它在哪、叫什么，不记端口：端口属于「怎么抓」，那一半由登记的抓取端
// 给出。本类型回答的是适用性 —— 这个 namespace 里有没有东西会因为
// default-deny 而丢掉抓取流量。
type ScrapeDeclaration struct {
	// ClusterID 是所属集群。
	ClusterID string
	// Namespace 是声明所在命名空间。
	Namespace string
	// PodName 是发出声明的 Pod。
	//
	// 留着它是为了事后答得出「凭什么说这个 namespace 需要抓取放行」——
	// 而那时这个 Pod 可能已经不在了（同 ScrapeAnnotations 的理由）。
	PodName string
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
	// ScrapeDeclarations 是观测到的「谁愿意被抓」，每个声明的 Pod 一条。
	//
	// **与 ScrapeTargets 分开，因为它们的来源不同。** ScrapeTargets 是
	// 「登记的抓取端 × 观测到的被抓端」拼出来的，一个还没登记任何抓取端的
	// 集群它是空的；而本字段只取观测得到的那一半。
	//
	// 分开的用处在于判定 METRICS_SCRAPE 这一类适不适用：拿 ScrapeTargets
	// 当判据，未登记抓取端的集群每个 namespace 都会显得「不需要放行抓取
	// 流量」，而下发之后真正的 Prometheus 会被挡住
	// （design doc 2026-08-18-baseline-applicability §4.2）。
	//
	// **声明决定适不适用，登记决定推不推得出规则。**
	ScrapeDeclarations []ScrapeDeclaration
	// APIServers 是 API server 端点快照。
	APIServers []APIServerEndpoint
	// NodeAgents 是节点级 agent 快照。
	NodeAgents []NodeAgent
	// Registry 是集群网段注册信息。
	Registry ClusterRegistry
}
