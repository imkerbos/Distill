// Package fixture 提供一套合成的 Fleet 数据，用于 demo 与开发。
//
// 数据刻意包含"不完美"：mesh 内的 Pod、身份未还原的流量、写错的策略、
// 跨集群流量、hostNetwork Pod。全绿的数据集会把平台包装成什么都知道，
// 而真实集群上线后必然大量 UNKNOWN —— 落差会直接损伤信任。
package fixture

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// baseTime 是数据集的基准时刻。固定值保证每次加载完全一致。
var baseTime = time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)

// Flow 是一条带标识的合成流量。
type Flow struct {
	// ID 是流量标识，供 API 按 ID 查询单条判定。
	ID string
	// Flow 是求值引擎的输入。
	Flow replay.Flow
}

// Cluster 是一个合成集群。
type Cluster struct {
	// ID 是集群标识。
	ID string
	// Namespaces 是该集群的命名空间快照。
	Namespaces []replay.NamespaceRef
	// Pods 是该集群的 Pod 快照。
	Pods []replay.PodRef
	// Policies 是该集群的 NetworkPolicy。
	Policies []networkingv1.NetworkPolicy
	// CCNPPresent 表示该集群存在 Cilium 策略，判定需降级。
	CCNPPresent bool
	// Assets 是该集群的资产快照，Baseline 推导的依据来源。
	Assets snapshot.Assets
}

// Fleet 是全部合成数据。
type Fleet struct {
	// Clusters 是集群列表。
	Clusters []Cluster
	// Flows 是全部流量。
	Flows []Flow
}

// Cluster 按 ID 查找集群。
func (f Fleet) Cluster(id string) (Cluster, bool) {
	for _, c := range f.Clusters {
		if c.ID == id {
			return c, true
		}
	}
	return Cluster{}, false
}

// Load 构造并返回合成 Fleet。
//
// 每次调用返回等价数据，不做缓存：数据量小，且避免调用方之间
// 通过共享的可变切片互相影响。
func Load() Fleet {
	asia := buildAsia()
	eu := buildEU()
	return Fleet{
		Clusters: []Cluster{asia, eu},
		Flows:    buildFlows(asia, eu),
	}
}

// podsByNamespace 从集群快照里筛出指定命名空间的 Pod，供 buildFlows
// 按 namespace 取样，不必在生成 Pod 时另存一份索引。
func podsByNamespace(c Cluster, ns string) []replay.PodRef {
	var out []replay.PodRef
	for _, p := range c.Pods {
		if p.Namespace == ns {
			out = append(out, p)
		}
	}
	return out
}

// buildAsia 构造 prod-asia-1 集群：网关、支付、mesh 化的 checkout、
// 无策略的 batch、含 hostNetwork Pod 的 kube-system，以及带错误
// ipBlock 策略的 legacy 命名空间。
func buildAsia() Cluster {
	const clusterID = "prod-asia-1"

	namespaces := []replay.NamespaceRef{
		{ClusterID: clusterID, Name: "gateway", Labels: map[string]string{"env": "prod", "role": "edge"}},
		{ClusterID: clusterID, Name: "payment", Labels: map[string]string{"env": "prod"}},
		{ClusterID: clusterID, Name: "checkout", Labels: map[string]string{"env": "prod", "istio-injection": "enabled"}},
		{ClusterID: clusterID, Name: "batch", Labels: map[string]string{"env": "prod"}},
		{ClusterID: clusterID, Name: "kube-system", Labels: map[string]string{"env": "prod"}},
		{ClusterID: clusterID, Name: "legacy", Labels: map[string]string{"env": "prod"}},
	}

	// IP 从 10.4.0.0/14 段顺序分配，与 buildEU 的 payment Pod 故意重叠：
	// 两个集群的 Pod 网段在真实环境里完全可能重叠，任何按 IP 单独建索引、
	// 不带 cluster_id 的代码路径都会在这份数据上出错。
	var pods []replay.PodRef
	ip := func(n int) string { return fmt.Sprintf("10.4.0.%d", n) }

	for i := 1; i <= 3; i++ {
		pods = append(pods, replay.PodRef{
			ClusterID: clusterID, Namespace: "gateway", Name: fmt.Sprintf("gateway-%d", i),
			IP: ip(i), Labels: map[string]string{"app": "gateway"},
		})
	}
	for i := 1; i <= 4; i++ {
		pods = append(pods, replay.PodRef{
			ClusterID: clusterID, Namespace: "payment", Name: fmt.Sprintf("payment-%d", i),
			IP: ip(3 + i), Labels: map[string]string{"app": "api"},
			NamedPorts: []replay.NamedPort{{Name: "http", Port: 8080, Protocol: replay.ProtocolTCP}},
		})
	}
	for i := 1; i <= 3; i++ {
		pods = append(pods, replay.PodRef{
			ClusterID: clusterID, Namespace: "checkout", Name: fmt.Sprintf("checkout-%d", i),
			IP: ip(7 + i), Labels: map[string]string{"app": "checkout"}, InMesh: true,
		})
	}
	for i := 1; i <= 2; i++ {
		pods = append(pods, replay.PodRef{
			ClusterID: clusterID, Namespace: "batch", Name: fmt.Sprintf("batch-%d", i),
			IP: ip(10 + i), Labels: map[string]string{"app": "worker"},
		})
	}
	// kube-system: 至少一个 hostNetwork Pod，代表 NetworkPolicy 管不到的
	// 特权组件；另一个留作普通 Pod，避免整个 namespace 看起来"全体特权"。
	pods = append(pods,
		replay.PodRef{
			ClusterID: clusterID, Namespace: "kube-system", Name: "kube-proxy-1",
			IP: ip(13), Labels: map[string]string{"app": "kube-proxy"}, HostNetwork: true,
		},
		replay.PodRef{
			ClusterID: clusterID, Namespace: "kube-system", Name: "metrics-agent-1",
			IP: ip(14), Labels: map[string]string{"app": "metrics-agent"},
		},
	)
	for i := 1; i <= 2; i++ {
		pods = append(pods, replay.PodRef{
			ClusterID: clusterID, Namespace: "legacy", Name: fmt.Sprintf("legacy-%d", i),
			IP: ip(14 + i), Labels: map[string]string{"app": "legacy"},
		})
	}
	// 一个没有 app 标签的 Pod。真实集群里这类工作负载相当常见 ——
	// 老服务、手工建的 Pod、第三方 Chart 用了别的标签约定。
	// workload 级拓扑必须把它显示成 UNKNOWN，而不是靠 Pod 名猜一个
	// 看起来合理的归属。数据集里没有这类 Pod，这条路径就永远走不到，
	// 而真实集群上线第一天就会遇到。
	pods = append(pods, replay.PodRef{
		ClusterID: clusterID, Namespace: "legacy", Name: "legacy-unlabelled",
		IP: ip(17), Labels: map[string]string{"env": "prod"},
	})

	// kube-dns 的后端。DNS Baseline 的 peer 是这两个 Pod，不是 ClusterIP ——
	// NetworkPolicy 选不中 ClusterIP。没有它们，生成的 DNS 规则指向空集。
	for i := 1; i <= 2; i++ {
		pods = append(pods, replay.PodRef{
			ClusterID: clusterID, Namespace: "kube-system", Name: fmt.Sprintf("kube-dns-%d", i),
			IP: ip(17 + i), Labels: map[string]string{"k8s-app": "kube-dns"},
			NamedPorts: []replay.NamedPort{{Name: "dns", Port: 53, Protocol: replay.ProtocolUDP}},
		})
	}

	emptySelector := metav1.LabelSelector{}
	apiSelector := metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
	tcpProto := corev1.ProtocolTCP
	port8080 := intstr.FromInt32(8080)
	tcp8080 := networkingv1.NetworkPolicyPort{Protocol: &tcpProto, Port: &port8080}

	policies := []networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "default-deny-ingress"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: emptySelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "allow-gateway"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: apiSelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						From: []networkingv1.NetworkPolicyPeer{
							{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}}},
						},
						Ports: []networkingv1.NetworkPolicyPort{tcp8080},
					},
				},
			},
		},
		{
			// 故意缺一段的 CIDR：产出 POLICY_MALFORMED，验证求值引擎不会把
			// 写错的策略静默吞掉、也不会把它当成一次可信的 DENY。
			ObjectMeta: metav1.ObjectMeta{Namespace: "legacy", Name: "broken-ipblock"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: emptySelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0/8"}}}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "checkout", Name: "default-deny-ingress"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: emptySelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			},
		},
	}

	return Cluster{ID: clusterID, Namespaces: namespaces, Pods: pods, Policies: policies, Assets: asiaAssets()}
}

// buildEU 构造 prod-eu-1 集群：payment 与 asia 同名但是独立对象，partner
// 命名空间的 Pod 会向 asia 发起跨集群流量。EU 没有任何 NetworkPolicy，
// 用来对比"有策略"与"无策略"集群在同一份数据集里的判定差异。
func buildEU() Cluster {
	const clusterID = "prod-eu-1"

	namespaces := []replay.NamespaceRef{
		{ClusterID: clusterID, Name: "payment", Labels: map[string]string{"env": "prod"}},
		{ClusterID: clusterID, Name: "partner", Labels: map[string]string{"env": "prod"}},
		// eu 也必须有 kube-dns：DNS Baseline 是每个集群每个 namespace 都要的，
		// 只在一个集群里造数据会让「另一个集群缺 DNS」这种假象被当成真结论。
		{ClusterID: clusterID, Name: "kube-system", Labels: map[string]string{"env": "prod"}},
	}

	// payment 的 IP 故意与 asia 的 gateway Pod 重叠（10.4.0.1 / .2）：
	// 平台的集群维度必须真正被使用，否则这两个 Pod 会被当成同一个。
	pods := []replay.PodRef{
		{ClusterID: clusterID, Namespace: "payment", Name: "payment-1", IP: "10.4.0.1", Labels: map[string]string{"app": "api"}},
		{ClusterID: clusterID, Namespace: "payment", Name: "payment-2", IP: "10.4.0.2", Labels: map[string]string{"app": "api"}},
		{ClusterID: clusterID, Namespace: "partner", Name: "partner-1", IP: "10.4.1.1", Labels: map[string]string{"app": "partner"}},
		{ClusterID: clusterID, Namespace: "partner", Name: "partner-2", IP: "10.4.1.2", Labels: map[string]string{"app": "partner"}},
		{ClusterID: clusterID, Namespace: "kube-system", Name: "kube-dns-1", IP: "10.4.2.1",
			Labels:     map[string]string{"k8s-app": "kube-dns"},
			NamedPorts: []replay.NamedPort{{Name: "dns", Port: 53, Protocol: replay.ProtocolUDP}}},
		{ClusterID: clusterID, Namespace: "kube-system", Name: "kube-dns-2", IP: "10.4.2.2",
			Labels:     map[string]string{"k8s-app": "kube-dns"},
			NamedPorts: []replay.NamedPort{{Name: "dns", Port: 53, Protocol: replay.ProtocolUDP}}},
		// 抓取端必须真的存在：euAssets 登记了一条 ScrapeTarget，抓取端是
		// kube-system 的 app=metrics-agent。没有这个 Pod，METRICS_SCRAPE
		// 会推导出一条选不中任何东西的 ingress 规则，且齐备性校验报"已具备"
		// —— 一条永不匹配的规则比缺失更危险，因为它让缺口消失在报告里。
		{ClusterID: clusterID, Namespace: "kube-system", Name: "metrics-agent-1", IP: "10.4.2.3",
			Labels: map[string]string{"app": "metrics-agent"}},
	}

	return Cluster{ID: clusterID, Namespaces: namespaces, Pods: pods, Assets: euAssets()}
}

// podEndpoint 把一个已还原身份的 Pod 转成流量端点。取 p 的副本取址，
// 避免端点持有的指针在 range 变量复用时被后续迭代悄悄改写。
func podEndpoint(p replay.PodRef) replay.Endpoint {
	pod := p
	return replay.Endpoint{ClusterID: pod.ClusterID, IP: pod.IP, Pod: &pod}
}

// externalEndpoint 构造一个没有集群归属、没有 Pod 身份的端点，代表公网地址。
func externalEndpoint(ip string) replay.Endpoint {
	return replay.Endpoint{IP: ip}
}

// hostNetworkPod 返回 pods 中第一个 HostNetwork Pod。
//
// buildFlows 用它把 kube-system 流量的目的地锁定在真正不受管控的 Pod
// 上——如果传入的切片里没有 hostNetwork Pod，说明数据集本身已经不满足
// 前提，panic 比生成一批文不对题的"unmanaged"流量更早暴露问题。
func hostNetworkPod(pods []replay.PodRef) replay.PodRef {
	for _, p := range pods {
		if p.HostNetwork {
			return p
		}
	}
	panic("fixture: no hostNetwork pod found")
}

// unlabelledPod 返回 pods 中第一个没有 app 标签的 Pod。
//
// 与 hostNetworkPod 同样的用意：按属性显式查找而非依赖切片下标，
// 避免未来往这个 namespace 插入新 Pod 时，NO_WORKLOAD_LABEL 场景
// 因为下标偏移而静默失效。
func unlabelledPod(pods []replay.PodRef) replay.PodRef {
	for _, p := range pods {
		if p.Labels["app"] == "" {
			return p
		}
	}
	panic("fixture: no unlabelled pod found")
}

// unresolvedEndpoint 构造一个"声称属于某集群、但身份没能还原"的端点，
// 模拟采集管线丢事件：flow 记录了源 IP 与它所在的集群，唯独没能把 IP
// 对应回具体 Pod。求值引擎必须把这种情况判成 SNAPSHOT_MISSING，
// 而不是因为 Pod 为 nil 就悄悄跳过该方向、产出一个虚假的 ALLOW。
func unresolvedEndpoint(clusterID, ip string) replay.Endpoint {
	return replay.Endpoint{ClusterID: clusterID, IP: ip}
}

// flowBuilder 累积 flow 列表并保证 ID 与时间戳单调、确定。
type flowBuilder struct {
	flows []Flow
	n     int
	at    time.Time
}

// add 追加一条 flow，ID 用固定宽度序号，时间戳从 baseTime 逐条推进一秒。
func (b *flowBuilder) add(src, dst replay.Endpoint, proto replay.Protocol, port int32) {
	b.n++
	b.flows = append(b.flows, Flow{
		ID: fmt.Sprintf("flow-%04d", b.n),
		Flow: replay.Flow{
			Source:    src,
			Dest:      dst,
			Protocol:  proto,
			Port:      port,
			Timestamp: b.at,
		},
	})
	b.at = b.at.Add(time.Second)
}

// buildFlows 生成 asia、eu 两个集群之间以及各自内部的合成流量，覆盖
// payment 的正常放行/拒绝、mesh 降级、策略写错、无策略的 batch、
// hostNetwork 直连、采集丢事件、跨集群、出公网、以及 eu 集群自身的
// 内部流量共九类场景。此前的版本只有 asia 有自己的内部流量，eu 只
// 作为跨集群 flow 的源端出现——一个集群有拓扑和数据质量、另一个
// 全是空白，观众第一反应会是"多集群是不是没做完"，而不是平台想
// 传达的"这个集群还没上线策略管控"。用循环而非手写字面量，是因为
// 单条 flow 本身没有个体意义，量级和分布才是 demo 要展示的东西。
func buildFlows(asia, eu Cluster) []Flow {
	b := &flowBuilder{at: baseTime}

	gateway := podsByNamespace(asia, "gateway")
	payment := podsByNamespace(asia, "payment")
	checkout := podsByNamespace(asia, "checkout")
	batch := podsByNamespace(asia, "batch")
	kubeSystem := podsByNamespace(asia, "kube-system")
	legacy := podsByNamespace(asia, "legacy")
	partner := podsByNamespace(eu, "partner")
	paymentEU := podsByNamespace(eu, "payment")

	// gateway -> payment:8080，命中 allow-gateway（NamespaceSelector role:edge）。
	for i := 0; i < 45; i++ {
		src := gateway[i%len(gateway)]
		dst := payment[i%len(payment)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 8080)
	}

	// gateway -> payment:9090，allow-gateway 只放行 8080，无规则匹配 -> DENY。
	for i := 0; i < 45; i++ {
		src := gateway[i%len(gateway)]
		dst := payment[i%len(payment)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 9090)
	}

	// -> checkout：目的 Pod 在 mesh 内，sidecar 掩盖了 L4 身份 -> DEGRADED。
	checkoutSources := append(append([]replay.PodRef{}, gateway...), payment...)
	for i := 0; i < 30; i++ {
		src := checkoutSources[i%len(checkoutSources)]
		dst := checkout[i%len(checkout)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 8443)
	}

	// -> legacy：broken-ipblock 的 CIDR 解析失败 -> POLICY_MALFORMED。
	legacySources := append(append([]replay.PodRef{}, gateway...), batch...)
	for i := 0; i < 30; i++ {
		src := legacySources[i%len(legacySources)]
		dst := legacy[i%len(legacy)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 8080)
	}

	// legacy-unlabelled -> gateway：无 app 标签的 Pod 做主体，产出
	// NO_WORKLOAD_LABEL。目的地必须换成 legacy 以外的 namespace ——
	// broken-ipblock 只挂在 legacy 的 Ingress 方向，任何打进 legacy 的
	// 流量都会先被判成整体 POLICY_MALFORMED（UNKNOWN），无标签 Pod
	// 自身的表达失败反而验证不到。换成出向、目的地是没有入向策略的
	// gateway，这条流量才会以 TRUSTED ALLOW 结论走到分类逻辑，让
	// classify 因为源 Pod 缺 app 标签报出 NO_WORKLOAD_LABEL。
	unlabelled := unlabelledPod(legacy)
	for i := 0; i < 3; i++ {
		dst := gateway[i%len(gateway)]
		b.add(podEndpoint(unlabelled), podEndpoint(dst), replay.ProtocolTCP, 8080)
	}

	// batch -> payment：batch 没有任何 NetworkPolicy 选中它，出向不受限；
	// payment 一侧仍按自己的策略判——role:edge 选不中 batch，落到 DENY。
	for i := 0; i < 30; i++ {
		src := batch[i%len(batch)]
		dst := payment[i%len(payment)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 8080)
	}

	// -> kube-system 的 hostNetwork Pod：不在 Pod 网络内，NetworkPolicy 管
	// 不到它。全部 20 条都打到这一个 Pod 上，而不是在 namespace 内的两个
	// Pod 间轮换——轮换会让一半流量只是打到"无策略的普通 Pod"，稀释掉
	// hostNetwork 这个场景本该有的分量。
	kubeSystemSources := append(append([]replay.PodRef{}, gateway...), payment...)
	unmanaged := hostNetworkPod(kubeSystem)
	for i := 0; i < 20; i++ {
		src := kubeSystemSources[i%len(kubeSystemSources)]
		b.add(podEndpoint(src), podEndpoint(unmanaged), replay.ProtocolTCP, 9100)
	}

	// 源身份未还原，模拟采集丢事件 -> SNAPSHOT_MISSING。
	for i := 0; i < 10; i++ {
		src := unresolvedEndpoint(asia.ID, fmt.Sprintf("10.4.9.%d", i+1))
		dst := payment[i%len(payment)]
		b.add(src, podEndpoint(dst), replay.ProtocolTCP, 8080)
	}

	// partner(eu) -> payment(asia)：标准 NetworkPolicy 表达不了跨集群，
	// 平台只做可见性标记，不做 enforce —— 这是已知的敞口，不是 bug。
	for i := 0; i < 10; i++ {
		src := partner[i%len(partner)]
		dst := payment[i%len(payment)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 8080)
	}

	// 少量出公网流量：目的地没有集群归属、没有 Pod 身份。
	externalIPs := []string{"8.8.8.8", "1.1.1.1"}
	for i := 0; i < 10; i++ {
		src := gateway[i%len(gateway)]
		dst := externalEndpoint(externalIPs[i%len(externalIPs)])
		b.add(podEndpoint(src), dst, replay.ProtocolTCP, 443)
	}

	// partner -> payment(eu)：eu 集群没有任何 NetworkPolicy，全部放行
	// ALLOW——这是一个尚未上线策略管控的集群该有的样子，不是求值引擎
	// 的漏洞。这批流量让 eu 拥有自己的拓扑边与数据质量数字，而不是只
	// 在 asia 的跨集群边里以对端身份出现。
	for i := 0; i < 30; i++ {
		src := partner[i%len(partner)]
		dst := paymentEU[i%len(paymentEU)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 8080)
	}

	// 三类真实可疑流量。数据集其余部分的端口全是 443/8080/9090/9100，
	// 没有一个在语义上危险 —— 缺了这三类，高风险端口报告只能永远空着，
	// 而一块空着的安全报告读起来是"这套集群很干净"。
	//
	// 三类刻意落在不同风险位置上，报告不能把它们合成一个分数：
	// 跨 namespace 的数据库直连、跨 namespace 的管理端口、出公网的管理端口，
	// 处置方式完全不同。

	// batch -> payment:3306：跨 namespace 直连数据库端口。payment 的策略
	// 只放行 role:edge 的来源，batch 不是，因此这批会被判 DENY——
	// 一条被策略挡住的高风险连接，仍然是需要有人去问"谁在连数据库"的信号。
	for i := 0; i < 8; i++ {
		src := batch[i%len(batch)]
		dst := payment[i%len(payment)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 3306)
	}

	// batch -> legacy:22：跨 namespace SSH。legacy 的策略写错了 ipBlock，
	// 因此这批会落进 UNKNOWN —— "有人在跨 namespace 连 SSH，而我们连它
	// 通不通都算不出来"，比单纯的 DENY 更需要出现在报告里。
	for i := 0; i < 4; i++ {
		src := batch[i%len(batch)]
		dst := legacy[i%len(legacy)]
		b.add(podEndpoint(src), podEndpoint(dst), replay.ProtocolTCP, 22)
	}

	// gateway -> 203.0.113.10:22：出公网的管理端口，风险最高的一类。
	// 地址取 RFC 5737 的文档保留段，避免与任何真实主机重合。
	for i := 0; i < 3; i++ {
		src := gateway[i%len(gateway)]
		b.add(podEndpoint(src), externalEndpoint("203.0.113.10"), replay.ProtocolTCP, 22)
	}

	// eu 少量出公网流量，与 asia 的出公网场景对称。
	externalIPsEU := []string{"9.9.9.9"}
	for i := 0; i < 6; i++ {
		src := partner[i%len(partner)]
		dst := externalEndpoint(externalIPsEU[i%len(externalIPsEU)])
		b.add(podEndpoint(src), dst, replay.ProtocolTCP, 443)
	}

	return b.flows
}
