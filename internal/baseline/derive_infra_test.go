package baseline

import (
	"testing"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

func infraAssets() snapshot.Assets {
	return snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "gateway", Name: "gateway-lb", Type: "LoadBalancer",
			Selector: map[string]string{"app": "gateway"},
			Ports: []snapshot.ServicePort{
				{Name: "https", Port: 443, TargetPort: 8443, Protocol: "TCP"},
			},
		}},
		Gateways: []snapshot.Gateway{{
			ClusterID: "c1", Namespace: "gateway", Name: "public-gateway",
			Kind: "Gateway", BackendService: "gateway-lb",
		}},
		ScrapeTargets: []snapshot.ScrapeTarget{{
			ClusterID: "c1", ScraperNamespace: "kube-system",
			ScraperLabels:   map[string]string{"app": "metrics-agent"},
			TargetNamespace: "payment", TargetPort: 9090,
		}},
		NodeAgents: []snapshot.NodeAgent{{
			ClusterID: "c1", Namespace: "kube-system", App: "kube-proxy",
			HostNetwork: true, TargetPort: 9100,
		}},
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
	}
}

// 健康检查网段必须来自注册信息，端口必须是后端 targetPort 而非 Service port。
// 用 Service port（443）会放行一个后端根本没监听的端口，而真正的
// 健康检查（8443）仍然被挡 —— 入口中断，且规则看起来是对的。
func TestDeriveLBHealthUsesRegistryCIDRAndTargetPort(t *testing.T) {
	rules := deriveLBHealth(infraAssets())
	if len(rules) != 1 {
		t.Fatalf("deriveLBHealth returned %d rules, want 1", len(rules))
	}
	r := rules[0]
	if r.Direction != replay.DirectionIngress || r.Ingress == nil {
		t.Fatalf("rule = %+v, want an ingress rule", r)
	}
	var cidrs []string
	for _, p := range r.Ingress.From {
		if p.IPBlock == nil {
			t.Fatalf("peer %+v is not an ipBlock; health-check sources are external addresses", p)
		}
		cidrs = append(cidrs, p.IPBlock.CIDR)
	}
	if len(cidrs) != 2 || cidrs[0] != "35.191.0.0/16" || cidrs[1] != "130.211.0.0/22" {
		t.Errorf("cidrs = %v, want the two registered health-check sources", cidrs)
	}
	if len(r.Ingress.Ports) != 1 || r.Ingress.Ports[0].Port.IntValue() != 8443 {
		t.Errorf("ports = %+v, want targetPort 8443, not service port 443", r.Ingress.Ports)
	}
}

func TestDeriveLBHealthRecordsBothGatewayAndRegistry(t *testing.T) {
	r := deriveLBHealth(infraAssets())[0]
	kinds := map[SourceKind]bool{}
	for _, d := range r.Derivations {
		kinds[d.SourceKind] = true
		if d.Field == "" {
			t.Errorf("derivation %+v has empty Field", d)
		}
	}
	for _, want := range []SourceKind{SourceGateway, SourceService, SourceClusterRegistry} {
		if !kinds[want] {
			t.Errorf("derivations missing %s; cannot trace where the rule came from", want)
		}
	}
}

// 没有暴露面就不该有 LB Baseline。凭空生成一条会让齐备性校验
// 在一个根本不对外暴露的 namespace 上也报"齐备"。
func TestDeriveLBHealthSkipsWithoutGateway(t *testing.T) {
	a := infraAssets()
	a.Gateways = nil
	if rules := deriveLBHealth(a); len(rules) != 0 {
		t.Errorf("deriveLBHealth returned %d rules without exposure, want 0", len(rules))
	}
}

// 抓取关系是按被抓取 namespace 记录的，只有目标 namespace 才生成规则。
func TestDeriveMetricsOnlyForTargetNamespace(t *testing.T) {
	if rules := deriveMetrics(infraAssets(), "payment"); len(rules) != 1 {
		t.Errorf("deriveMetrics(payment) returned %d rules, want 1", len(rules))
	}
	if rules := deriveMetrics(infraAssets(), "batch"); len(rules) != 0 {
		t.Errorf("deriveMetrics(batch) returned %d rules, want 0", len(rules))
	}
}

func TestDeriveMetricsSelectsScraperPods(t *testing.T) {
	r := deriveMetrics(infraAssets(), "payment")[0]
	if len(r.Ingress.From) != 1 {
		t.Fatalf("peers = %d, want 1", len(r.Ingress.From))
	}
	peer := r.Ingress.From[0]
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["app"] != "metrics-agent" {
		t.Errorf("PodSelector = %+v, want app=metrics-agent", peer.PodSelector)
	}
	if peer.NamespaceSelector == nil ||
		peer.NamespaceSelector.MatchLabels[nsNameLabel] != "kube-system" {
		t.Errorf("NamespaceSelector = %+v, want kube-system", peer.NamespaceSelector)
	}
	if len(r.Ingress.Ports) != 1 || r.Ingress.Ports[0].Port.IntValue() != 9090 {
		t.Errorf("ports = %+v, want 9090", r.Ingress.Ports)
	}
}

// hostNetwork agent 的源地址是节点 IP，podSelector 永远选不中它。
// 写成 podSelector 会得到一条看起来正确、实际从不匹配的规则 ——
// 上线后监控与日志静默中断，且在事故时才显现。
func TestDeriveNodeAgentUsesNodeCIDRNotPodSelector(t *testing.T) {
	rules := deriveNodeAgent(infraAssets())
	if len(rules) != 1 {
		t.Fatalf("deriveNodeAgent returned %d rules, want 1", len(rules))
	}
	r := rules[0]
	if len(r.Ingress.From) != 1 {
		t.Fatalf("peers = %d, want 1", len(r.Ingress.From))
	}
	peer := r.Ingress.From[0]
	if peer.PodSelector != nil || peer.NamespaceSelector != nil {
		t.Error("node agent peer uses a selector; hostNetwork pods carry the node IP and are never selected")
	}
	if peer.IPBlock == nil || peer.IPBlock.CIDR != "10.128.0.0/20" {
		t.Errorf("ipBlock = %+v, want the registered node CIDR", peer.IPBlock)
	}
}

// 非 hostNetwork 的 agent 走 podSelector：它确实在 Pod 网络里。
func TestDeriveNodeAgentUsesSelectorForPodNetworkAgent(t *testing.T) {
	a := infraAssets()
	a.NodeAgents[0].HostNetwork = false
	r := deriveNodeAgent(a)[0]
	peer := r.Ingress.From[0]
	if peer.IPBlock != nil {
		t.Error("pod-network agent peer uses ipBlock; it is selectable by label")
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["app"] != "kube-proxy" {
		t.Errorf("PodSelector = %+v, want app=kube-proxy", peer.PodSelector)
	}
}

func TestDeriveNodeAgentSkipsWhenNodeCIDRUnregistered(t *testing.T) {
	a := infraAssets()
	a.Registry.NodeCIDR = ""
	if rules := deriveNodeAgent(a); len(rules) != 0 {
		t.Errorf("deriveNodeAgent returned %d rules without a node CIDR, want 0", len(rules))
	}
}

// 没有登记健康检查源网段就没有可放行的对端：凭空造一个会造出
// 硬编码网段常量表，正是本包 doc 开头明确禁止的那种"祖传配置"。
func TestDeriveLBHealthSkipsWhenHealthCheckSourcesUnregistered(t *testing.T) {
	a := infraAssets()
	a.Registry.HealthCheckSources = nil
	if rules := deriveLBHealth(a); len(rules) != 0 {
		t.Errorf("deriveLBHealth returned %d rules without registered health-check sources, want 0", len(rules))
	}
}
