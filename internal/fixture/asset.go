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
		APIServers: []snapshot.APIServerEndpoint{
			{ClusterID: clusterID, Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
		},
		NodeAgents: []snapshot.NodeAgent{
			{
				ClusterID: clusterID, Namespace: "kube-system", App: "kube-proxy",
				HostNetwork: true, TargetPort: 9100,
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
// 刻意没有 Gateway：eu 不对外暴露。缺少暴露面时不生成 LB Baseline，
// 这条路径必须有数据能走到，否则「五类齐备」在任何集群上都恒真，
// Missing() 就成了永远返回空的摆设。
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
