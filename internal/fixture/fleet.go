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

	return Cluster{ID: clusterID, Namespaces: namespaces, Pods: pods, Policies: policies}
}

// buildEU 构造 prod-eu-1 集群：payment 与 asia 同名但是独立对象，partner
// 命名空间的 Pod 会向 asia 发起跨集群流量。EU 没有任何 NetworkPolicy，
// 用来对比"有策略"与"无策略"集群在同一份数据集里的判定差异。
func buildEU() Cluster {
	const clusterID = "prod-eu-1"

	namespaces := []replay.NamespaceRef{
		{ClusterID: clusterID, Name: "payment", Labels: map[string]string{"env": "prod"}},
		{ClusterID: clusterID, Name: "partner", Labels: map[string]string{"env": "prod"}},
	}

	// payment 的 IP 故意与 asia 的 gateway Pod 重叠（10.4.0.1 / .2）：
	// 平台的集群维度必须真正被使用，否则这两个 Pod 会被当成同一个。
	pods := []replay.PodRef{
		{ClusterID: clusterID, Namespace: "payment", Name: "payment-1", IP: "10.4.0.1", Labels: map[string]string{"app": "api"}},
		{ClusterID: clusterID, Namespace: "payment", Name: "payment-2", IP: "10.4.0.2", Labels: map[string]string{"app": "api"}},
		{ClusterID: clusterID, Namespace: "partner", Name: "partner-1", IP: "10.4.1.1", Labels: map[string]string{"app": "partner"}},
		{ClusterID: clusterID, Namespace: "partner", Name: "partner-2", IP: "10.4.1.2", Labels: map[string]string{"app": "partner"}},
	}

	return Cluster{ID: clusterID, Namespaces: namespaces, Pods: pods}
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

// buildFlows 生成 asia、eu 两个集群之间的合成流量，覆盖 payment 的正常
// 放行/拒绝、mesh 降级、策略写错、无策略的 batch、hostNetwork 直连、
// 采集丢事件、跨集群、出公网八类场景。用循环而非手写字面量，是因为
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

	return b.flows
}
