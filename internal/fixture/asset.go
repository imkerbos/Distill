package fixture

import "github.com/imkerbos/Distill/internal/snapshot"

// asiaAssets 构造 prod-asia-1 的资产快照。
//
// 每个对象的存在都对应一类 Baseline 的推导依据：没有这些对象，
// Baseline 只能靠硬编码常量表得出，而常量表不会随网段变化更新，
// 也说不清当初是怎么来的（spec §7.2）。
func asiaAssets() snapshot.Assets {
	const clusterID = "prod-asia-1"
	return snapshot.Assets{
		ClusterID: clusterID,
		Services: []snapshot.Service{
			{
				ClusterID: clusterID, Namespace: "kube-system", Name: "kube-dns",
				Type: "ClusterIP", ClusterIP: "10.8.0.10",
				Selector: map[string]string{"k8s-app": "kube-dns"},
				Ports: []snapshot.ServicePort{
					{Name: "dns", Port: 53, TargetPort: 53, Protocol: "UDP"},
					{Name: "dns-tcp", Port: 53, TargetPort: 53, Protocol: "TCP"},
				},
			},
			{
				ClusterID: clusterID, Namespace: "gateway", Name: "gateway-lb",
				Type: "LoadBalancer", ClusterIP: "10.8.0.40",
				Selector: map[string]string{"app": "gateway"},
				Ports: []snapshot.ServicePort{
					{Name: "https", Port: 443, TargetPort: 8443, Protocol: "TCP"},
				},
				// 入口地址落在下面登记的 node_cidr 内：EXPOSED_INGRESS 也要能在
				// 这个「各类齐备」的 namespace 上推出规则，否则 gateway 就不再
				// 是齐备侧的样本了。
				LoadBalancerIngressIPs: []string{"10.128.0.5"},
			},
		},
		Endpoints: []snapshot.Endpoints{
			{
				ClusterID: clusterID, Namespace: "kube-system", Name: "kube-dns",
				Addresses: []string{"10.4.0.18", "10.4.0.19"}, Ports: []int32{53},
			},
			{
				ClusterID: clusterID, Namespace: "gateway", Name: "gateway-lb",
				Addresses: []string{"10.4.0.1", "10.4.0.2", "10.4.0.3"}, Ports: []int32{8443},
			},
		},
		Gateways: []snapshot.Gateway{
			{
				ClusterID: clusterID, Namespace: "gateway", Name: "public-gateway",
				Kind: "Gateway", BackendService: "gateway-lb",
			},
		},
		ScrapeTargets: []snapshot.ScrapeTarget{
			{
				ClusterID: clusterID, ScraperNamespace: "kube-system",
				ScraperLabels:   map[string]string{"app": "metrics-agent"},
				TargetNamespace: "payment", TargetPort: 9090,
			},
			{
				ClusterID: clusterID, ScraperNamespace: "kube-system",
				ScraperLabels:   map[string]string{"app": "metrics-agent"},
				TargetNamespace: "gateway", TargetPort: 9090,
			},
		},
		// 被抓端的声明，与上面那份抓取关系是同一件事的另一半：
		// 声明决定 METRICS_SCRAPE 这一类适不适用，登记决定推不推得出规则
		// （design doc 2026-08-18-baseline-applicability §4.2）。
		// 两者必须对得上 —— 有声明没登记的 namespace 会被判成缺失。
		ScrapeDeclarations: []snapshot.ScrapeDeclaration{
			{ClusterID: clusterID, Namespace: "payment", PodName: "payment-api-7d4"},
			{ClusterID: clusterID, Namespace: "gateway", PodName: "gateway-lb-6c8"},
		},
		// Registry 与 APIServers 现在由 internal/registry 提供，
		// 这里的值仅供不接数据库的单元测试兜底。两处必须保持一致 ——
		// 不一致时以注册信息为准，而 fixture 的值会静默失效。
		APIServers: []snapshot.APIServerEndpoint{
			{ClusterID: clusterID, Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
		},
		NodeAgents: []snapshot.NodeAgent{
			{
				ClusterID: clusterID, Namespace: "kube-system", App: "kube-proxy",
				HostNetwork: true, TargetPort: 9100,
			},
		},
		// gateway 是「齐备」那一侧的样本，所以它也得声明探针 ——
		// 少了这一条，KUBELET_PROBE 会把 gateway 判成不适用，齐备侧的样本
		// 就不再齐备了。
		ProbeTargets: []snapshot.ProbeTarget{
			{
				ClusterID: clusterID, Namespace: "gateway",
				WorkloadKey: "app", Workload: "gateway",
				Ports: []snapshot.NamedPort{{Port: 8443, Protocol: "TCP"}},
			},
		},
		Registry: snapshot.ClusterRegistry{
			ClusterID: clusterID,
			PodCIDR:   "10.4.0.0/14",
			NodeCIDR:  "10.128.0.0/20",
			// GCLB 健康检查来源。登记在集群注册信息里而非写死在推导代码中：
			// 网段变更时改这里一处，且 derivation 能指回它。
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
	}
}

// euAssets 构造 prod-eu-1 的资产快照。
//
// **这个集群承担「真的缺 Baseline」那一侧**，asia 承担齐备那一侧。两侧都
// 必须有数据：全齐备时「各类齐备」恒真、Missing() 成了摆设；全缺失时
// 「不适用」那条路径没人走。
//
// partner 的缺失是真缺，不是没有暴露面：它有一个 type=LoadBalancer 的
// Service（因此健康检查确实会打进来），但平台今天只从 Ingress 类入口对象
// 推 LB Baseline，推不出规则；它也有 Pod 声明要被抓，却没有任何抓取端
// 登记到它。两者都是「有对象、推不出规则」——
// 正是 Enforcing 门禁该挡住的那一种（design doc 2026-08-18-enforcing-gate）。
//
// 刻意没有 Gateway：eu 不从 Ingress 对外暴露。
func euAssets() snapshot.Assets {
	const clusterID = "prod-eu-1"
	return snapshot.Assets{
		ClusterID: clusterID,
		Services: []snapshot.Service{
			{
				ClusterID: clusterID, Namespace: "kube-system", Name: "kube-dns",
				Type: "ClusterIP", ClusterIP: "10.12.0.10",
				Selector: map[string]string{"k8s-app": "kube-dns"},
				Ports: []snapshot.ServicePort{
					{Name: "dns", Port: 53, TargetPort: 53, Protocol: "UDP"},
					{Name: "dns-tcp", Port: 53, TargetPort: 53, Protocol: "TCP"},
				},
			},
			{
				ClusterID: clusterID, Namespace: "partner", Name: "partner-lb",
				Type: "LoadBalancer", ClusterIP: "10.12.0.44",
				Selector: map[string]string{"app": "partner-api"},
				Ports: []snapshot.ServicePort{
					{Name: "https", Port: 443, TargetPort: 8443, Protocol: "TCP"},
				},
			},
		},
		Endpoints: []snapshot.Endpoints{
			{
				ClusterID: clusterID, Namespace: "kube-system", Name: "kube-dns",
				Addresses: []string{"10.4.2.1", "10.4.2.2"}, Ports: []int32{53},
			},
		},
		ScrapeTargets: []snapshot.ScrapeTarget{
			{
				ClusterID: clusterID, ScraperNamespace: "kube-system",
				ScraperLabels:   map[string]string{"app": "metrics-agent"},
				TargetNamespace: "payment", TargetPort: 9090,
			},
		},
		ScrapeDeclarations: []snapshot.ScrapeDeclaration{
			{ClusterID: clusterID, Namespace: "payment", PodName: "payment-api-9f2"},
			// partner 声明了要被抓，却没有任何抓取端登记到它：
			// 适用且推不出规则，因此如实报缺失。
			{ClusterID: clusterID, Namespace: "partner", PodName: "partner-api-4d7"},
		},
		// Registry 与 APIServers 现在由 internal/registry 提供，
		// 这里的值仅供不接数据库的单元测试兜底。两处必须保持一致 ——
		// 不一致时以注册信息为准，而 fixture 的值会静默失效。
		APIServers: []snapshot.APIServerEndpoint{
			{ClusterID: clusterID, Host: "10.13.0.2", CIDR: "10.13.0.0/28", Port: 443},
		},
		NodeAgents: []snapshot.NodeAgent{
			{
				ClusterID: clusterID, Namespace: "kube-system", App: "log-agent",
				HostNetwork: true, TargetPort: 9100,
			},
		},
		Registry: snapshot.ClusterRegistry{
			ClusterID:          clusterID,
			PodCIDR:            "10.4.0.0/14",
			NodeCIDR:           "10.132.0.0/20",
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
	}
}
