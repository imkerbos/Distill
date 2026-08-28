package collect

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// serviceNameLabel 是 EndpointSlice 指回其 Service 的标准标签。
const serviceNameLabel = "kubernetes.io/service-name"

// collectNodes 采集全部节点。
//
// 节点是集群自己报的网段事实。它与注册表里人填的网段不一致时，
// 错的是注册表 —— 这一比对留给可见面，采集只负责如实记下两边。
func (c *Collector) collectNodes(ctx context.Context, obs *snapshot.Observation) error {
	return eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list nodes in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			n := &list.Items[i]

			// PodCIDRs 是新字段，PodCIDR 是单值旧字段。双栈集群只填前者，
			// 老集群只填后者。只读一个会在另一种集群上得到空网段，
			// 而空网段会让本节点上的每个 Pod 都判成"落在登记网段外"。
			cidrs := slices.Clone(n.Spec.PodCIDRs)
			if len(cidrs) == 0 && n.Spec.PodCIDR != "" {
				cidrs = []string{n.Spec.PodCIDR}
			}

			var internal []string
			for _, a := range n.Status.Addresses {
				if a.Type == corev1.NodeInternalIP {
					internal = append(internal, a.Address)
				}
			}

			obs.Nodes = append(obs.Nodes, snapshot.Node{
				ClusterID:   c.clusterID,
				Name:        n.Name,
				PodCIDRs:    cidrs,
				InternalIPs: internal,
			})
		}
		return list.Continue, nil
	})
}

// collectServices 采集全部 Service。
func (c *Collector) collectServices(ctx context.Context, obs *snapshot.Observation) error {
	return eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.CoreV1().Services(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list services in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			s := &list.Items[i]

			ports := make([]snapshot.ServicePort, 0, len(s.Spec.Ports))
			for _, p := range s.Spec.Ports {
				sp := snapshot.ServicePort{
					Name:     p.Name,
					Port:     p.Port,
					Protocol: string(p.Protocol),
				}
				// targetPort 可以是数字，也可以是后端容器上的端口名。
				// 只取数字会把命名端口静默记成 0，而 0 是一个合法端口值 ——
				// 一条指向端口 0 的规则永远匹配不上，外观却完全正常。
				if p.TargetPort.StrVal != "" {
					sp.TargetPortName = p.TargetPort.StrVal
				} else {
					sp.TargetPort = p.TargetPort.IntVal
				}
				ports = append(ports, sp)
			}

			// 入口地址取 status 而非 spec：spec.loadBalancerIP 是**申请**的
			// 地址，可能没生效；status 里的才是云厂商实际分配的那个，而
			// 判定暴露范围要的是实际可达的入口。
			var ingressIPs []string
			for _, ing := range s.Status.LoadBalancer.Ingress {
				if ing.IP != "" {
					ingressIPs = append(ingressIPs, ing.IP)
				}
			}
			obs.Services = append(obs.Services, snapshot.Service{
				ClusterID:                c.clusterID,
				Namespace:                s.Namespace,
				Name:                     s.Name,
				Type:                     string(s.Spec.Type),
				Selector:                 s.Spec.Selector,
				ClusterIP:                s.Spec.ClusterIP,
				Ports:                    ports,
				LoadBalancerIngressIPs:   ingressIPs,
				LoadBalancerSourceRanges: s.Spec.LoadBalancerSourceRanges,
			})
		}
		return list.Continue, nil
	})
}

// collectEndpointSlices 采集全部 EndpointSlice 并按 Service 归并。
//
// 用 EndpointSlice 而非 Endpoints：后者已废弃，且在后端超过 1000 个时
// 会被静默截断 —— 一个被截断的后端列表看起来和一个完整的完全一样。
func (c *Collector) collectEndpointSlices(ctx context.Context, obs *snapshot.Observation) error {
	// 一个 Service 可以有多个 slice，必须跨页归并后再落库。
	merged := map[string]*snapshot.Endpoints{}

	err := eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.DiscoveryV1().EndpointSlices(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list endpointslices in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			es := &list.Items[i]
			svc := es.Labels[serviceNameLabel]
			if svc == "" {
				// 没有 service-name 标签的 slice 是手工管理的，归不到任何
				// Service 上。跳过而非编一个归属。
				continue
			}

			key := es.Namespace + "/" + svc
			e, ok := merged[key]
			if !ok {
				e = &snapshot.Endpoints{ClusterID: c.clusterID, Namespace: es.Namespace, Name: svc}
				merged[key] = e
			}

			for _, ep := range es.Endpoints {
				// Ready 为 nil 按就绪处理，这是 EndpointSlice 的约定。
				// 未就绪的后端不计入：一个后端全部未就绪的 Service，
				// 照它生成的放行规则同样指向一个当下谁也到不了的集合。
				if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
					continue
				}
				e.Addresses = append(e.Addresses, ep.Addresses...)
			}
			for _, p := range es.Ports {
				if p.Port != nil {
					e.Ports = append(e.Ports, *p.Port)
				}
			}
		}
		return list.Continue, nil
	})
	if err != nil {
		return err
	}

	out := make([]snapshot.Endpoints, 0, len(merged))
	for _, e := range merged {
		e.Addresses = dedupeSorted(e.Addresses)
		e.Ports = dedupeSorted(e.Ports)
		out = append(out, *e)
	}
	// map 迭代顺序随机。排序键取 (namespace, name)，两者合起来唯一 ——
	// 只按其中一个排会在同名跨命名空间时得到不稳定的输出。
	slices.SortFunc(out, func(a, b snapshot.Endpoints) int {
		if v := cmp.Compare(a.Namespace, b.Namespace); v != 0 {
			return v
		}
		return cmp.Compare(a.Name, b.Name)
	})
	obs.Endpoints = append(obs.Endpoints, out...)
	return nil
}

// dedupeSorted 排序并去重。多个 slice 会重复报告同一个地址或端口。
func dedupeSorted[T cmp.Ordered](in []T) []T {
	if len(in) == 0 {
		return in
	}
	slices.Sort(in)
	return slices.Compact(in)
}

// collectNetworkPolicies 采集全部 NetworkPolicy，保留 YAML 原文。
func (c *Collector) collectNetworkPolicies(ctx context.Context, obs *snapshot.Observation) error {
	return eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list networkpolicies in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			np := list.Items[i]

			// List 返回的对象不带 TypeMeta，client-go 会剥掉它。
			// 不补回来的话，落库的 manifest 缺 apiVersion 与 kind，
			// 再也无法被当作一份 Kubernetes 清单读回去。
			np.TypeMeta = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}
			// managedFields 是 apply 的簿记，与策略语义无关，且体积常常
			// 超过 spec 本身。它是这份清单里唯一被刻意丢掉的东西。
			np.ManagedFields = nil

			manifest, err := yaml.Marshal(&np)
			if err != nil {
				return "", fmt.Errorf("marshal networkpolicy %s/%s in %s: %w", np.Namespace, np.Name, c.clusterID, err)
			}

			obs.Policies = append(obs.Policies, snapshot.NetworkPolicy{
				ClusterID: c.clusterID,
				Namespace: np.Namespace,
				Name:      np.Name,
				UID:       string(np.UID),
				Manifest:  string(manifest),
			})
		}
		return list.Continue, nil
	})
}

// collectIngresses 采集全部 Ingress，转成 Gateway 视图。
//
// 一个 Ingress 可以指向多个后端 Service，每个后端产出一条记录：
// Baseline 关心的是"哪些 workload 是外部入口"，而那是按 Service 数的。
func (c *Collector) collectIngresses(ctx context.Context, obs *snapshot.Observation) error {
	return eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list ingresses in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			ing := &list.Items[i]
			for _, svc := range ingressBackendServices(ing) {
				obs.Gateways = append(obs.Gateways, snapshot.Gateway{
					ClusterID:      c.clusterID,
					Namespace:      ing.Namespace,
					Name:           ing.Name,
					Kind:           "Ingress",
					BackendService: svc,
				})
			}
		}
		return list.Continue, nil
	})
}

// ingressBackendServices 返回一个 Ingress 引用的全部后端 Service 名，已去重排序。
func ingressBackendServices(ing *netv1.Ingress) []string {
	var names []string
	if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
		names = append(names, b.Service.Name)
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil {
				names = append(names, path.Backend.Service.Name)
			}
		}
	}
	return dedupeSorted(names)
}

// servicesWithoutEndpoints 找出有 selector 却没有就绪后端的 Service。
//
// 只查有 selector 的：ExternalName 与手工管理 Endpoints 的 Service
// 本来就不该有 EndpointSlice，把它们报成缺后端是一次误报，
// 而误报会让这条告警很快被整体忽略。
func servicesWithoutEndpoints(services []snapshot.Service, endpoints []snapshot.Endpoints) []snapshot.Warning {
	backed := make(map[string]bool, len(endpoints))
	for _, e := range endpoints {
		if len(e.Addresses) > 0 {
			backed[e.Namespace+"/"+e.Name] = true
		}
	}

	var out []snapshot.Warning
	for _, s := range services {
		if len(s.Selector) == 0 {
			continue
		}
		if backed[s.Namespace+"/"+s.Name] {
			continue
		}
		out = append(out, snapshot.Warning{
			Kind:    snapshot.WarningServiceWithoutEndpoints,
			Subject: s.Namespace + "/" + s.Name,
			Detail:  "selector matches no ready backend",
		})
	}
	return out
}
