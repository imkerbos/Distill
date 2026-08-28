package collect

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// 命名 targetPort 必须以名字落库。只取 IntVal 会把它静默记成 0，
// 而 0 是一个合法端口值 —— 照它生成的规则永远匹配不上，外观却正常。
func TestCollectServicesKeepsANamedTargetPortAsAName(t *testing.T) {
	c, _ := newTestCollector(t,
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Type:      corev1.ServiceTypeClusterIP,
				ClusterIP: "10.96.0.10",
				Selector:  map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("http")},
					{Name: "metrics", Port: 9090, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(9091)},
				},
			},
		},
	)

	var obs snapshot.Observation
	if err := c.collectServices(context.Background(), &obs); err != nil {
		t.Fatalf("collectServices = %v", err)
	}
	if len(obs.Services) != 1 || len(obs.Services[0].Ports) != 2 {
		t.Fatalf("services = %+v, want one service with two ports", obs.Services)
	}

	named := obs.Services[0].Ports[0]
	if named.TargetPortName != "http" {
		t.Errorf("named port TargetPortName = %q, want %q", named.TargetPortName, "http")
	}
	if named.TargetPort != 0 {
		t.Errorf("named port TargetPort = %d, want 0", named.TargetPort)
	}

	numeric := obs.Services[0].Ports[1]
	if numeric.TargetPort != 9091 || numeric.TargetPortName != "" {
		t.Errorf("numeric port = %+v, want TargetPort 9091 and no name", numeric)
	}
}

func TestCollectServicesCopiesTheIdentityFields(t *testing.T) {
	c, _ := newTestCollector(t,
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: "shop"},
			Spec: corev1.ServiceSpec{
				Type:         corev1.ServiceTypeExternalName,
				ExternalName: "example.com",
			},
		},
	)

	var obs snapshot.Observation
	if err := c.collectServices(context.Background(), &obs); err != nil {
		t.Fatalf("collectServices = %v", err)
	}
	got := obs.Services[0]
	if got.ClusterID != testClusterID || got.Namespace != "shop" || got.Name != "ext" {
		t.Errorf("service identity = %+v, want prod/shop/ext", got)
	}
	if got.Type != string(corev1.ServiceTypeExternalName) {
		t.Errorf("Type = %q, want %q", got.Type, corev1.ServiceTypeExternalName)
	}
}

// 双栈集群只填 PodCIDRs，老集群只填 PodCIDR。只读一个会在另一种集群上
// 得到空网段，而空网段会让该节点上每个 Pod 都判成"落在登记网段外"。
func TestCollectNodesReadsBothPodCIDRFields(t *testing.T) {
	c, _ := newTestCollector(t,
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "dual"},
			Spec:       corev1.NodeSpec{PodCIDR: "10.0.1.0/24", PodCIDRs: []string{"10.0.1.0/24", "fd00::/64"}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy"},
			Spec:       corev1.NodeSpec{PodCIDR: "10.0.2.0/24"},
		},
	)

	var obs snapshot.Observation
	if err := c.collectNodes(context.Background(), &obs); err != nil {
		t.Fatalf("collectNodes = %v", err)
	}
	byName := map[string][]string{}
	for _, n := range obs.Nodes {
		byName[n.Name] = n.PodCIDRs
	}
	if got := byName["dual"]; len(got) != 2 {
		t.Errorf("dual node PodCIDRs = %v, want both entries", got)
	}
	if got := byName["legacy"]; len(got) != 1 || got[0] != "10.0.2.0/24" {
		t.Errorf("legacy node PodCIDRs = %v, want the single legacy field", got)
	}
}

func TestCollectNodesKeepsOnlyInternalAddresses(t *testing.T) {
	c, _ := newTestCollector(t,
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.7"},
				{Type: corev1.NodeInternalIP, Address: "192.168.0.10"},
				{Type: corev1.NodeHostName, Address: "node-a.internal"},
			}},
		},
	)

	var obs snapshot.Observation
	if err := c.collectNodes(context.Background(), &obs); err != nil {
		t.Fatalf("collectNodes = %v", err)
	}
	got := obs.Nodes[0].InternalIPs
	if len(got) != 1 || got[0] != "192.168.0.10" {
		t.Errorf("InternalIPs = %v, want only the internal address", got)
	}
}

func slice(ns, name, svc string, eps []discoveryv1.Endpoint, ports []int32) *discoveryv1.EndpointSlice {
	labels := map[string]string{}
	if svc != "" {
		labels[serviceNameLabel] = svc
	}
	out := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Endpoints:  eps,
	}
	for _, p := range ports {
		out.Ports = append(out.Ports, discoveryv1.EndpointPort{Port: int32Ptr(p)})
	}
	return out
}

// 一个 Service 可以有多个 slice。不归并会让同一个 Service 落成多行，
// 后端非空校验也就查不到那个"其实有后端"的 Service。
func TestCollectEndpointSlicesMergesEverySliceOfAService(t *testing.T) {
	c, _ := newTestCollector(t,
		slice("default", "web-1", "web",
			[]discoveryv1.Endpoint{{Addresses: []string{"10.0.1.5"}}}, []int32{8080}),
		slice("default", "web-2", "web",
			[]discoveryv1.Endpoint{{Addresses: []string{"10.0.1.6", "10.0.1.5"}}}, []int32{8080, 9090}),
	)

	var obs snapshot.Observation
	if err := c.collectEndpointSlices(context.Background(), &obs); err != nil {
		t.Fatalf("collectEndpointSlices = %v", err)
	}
	if len(obs.Endpoints) != 1 {
		t.Fatalf("endpoints = %+v, want one merged record", obs.Endpoints)
	}
	got := obs.Endpoints[0]
	if strings.Join(got.Addresses, ",") != "10.0.1.5,10.0.1.6" {
		t.Errorf("Addresses = %v, want deduped and sorted", got.Addresses)
	}
	if len(got.Ports) != 2 || got.Ports[0] != 8080 || got.Ports[1] != 9090 {
		t.Errorf("Ports = %v, want [8080 9090]", got.Ports)
	}
}

// 未就绪的后端不计入：一个后端全部未就绪的 Service，照它生成的放行规则
// 同样指向一个当下谁也到不了的集合。
func TestCollectEndpointSlicesDropsNotReadyBackends(t *testing.T) {
	c, _ := newTestCollector(t,
		slice("default", "web-1", "web", []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.1.5"}, Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)}},
			{Addresses: []string{"10.0.1.6"}, Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)}},
			{Addresses: []string{"10.0.1.7"}}, // Ready 为 nil，按就绪处理
		}, nil),
	)

	var obs snapshot.Observation
	if err := c.collectEndpointSlices(context.Background(), &obs); err != nil {
		t.Fatalf("collectEndpointSlices = %v", err)
	}
	got := obs.Endpoints[0].Addresses
	if strings.Join(got, ",") != "10.0.1.6,10.0.1.7" {
		t.Errorf("Addresses = %v, want the ready and the unset ones only", got)
	}
}

func TestCollectEndpointSlicesSkipsSlicesWithNoServiceLabel(t *testing.T) {
	c, _ := newTestCollector(t,
		slice("default", "manual", "", []discoveryv1.Endpoint{{Addresses: []string{"10.0.1.5"}}}, nil),
	)

	var obs snapshot.Observation
	if err := c.collectEndpointSlices(context.Background(), &obs); err != nil {
		t.Fatalf("collectEndpointSlices = %v", err)
	}
	if len(obs.Endpoints) != 0 {
		t.Errorf("endpoints = %+v, want none; a slice with no service-name belongs to no Service", obs.Endpoints)
	}
}

func TestCollectEndpointSlicesSortsDeterministically(t *testing.T) {
	c, _ := newTestCollector(t,
		slice("b", "x", "svc", []discoveryv1.Endpoint{{Addresses: []string{"10.0.1.5"}}}, nil),
		slice("a", "x", "svc", []discoveryv1.Endpoint{{Addresses: []string{"10.0.1.6"}}}, nil),
		slice("a", "y", "other", []discoveryv1.Endpoint{{Addresses: []string{"10.0.1.7"}}}, nil),
	)

	var obs snapshot.Observation
	if err := c.collectEndpointSlices(context.Background(), &obs); err != nil {
		t.Fatalf("collectEndpointSlices = %v", err)
	}
	var got []string
	for _, e := range obs.Endpoints {
		got = append(got, e.Namespace+"/"+e.Name)
	}
	want := "a/other,a/svc,b/svc"
	if strings.Join(got, ",") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

// List 返回的对象不带 TypeMeta。不补回来的话，落库的 manifest 缺
// apiVersion 与 kind，再也无法被当作一份 Kubernetes 清单读回去。
func TestCollectNetworkPoliciesRestoresTypeMetaAndDropsManagedFields(t *testing.T) {
	c, _ := newTestCollector(t,
		&netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "deny-all", Namespace: "default", UID: "np-uid",
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}},
			},
			Spec: netv1.NetworkPolicySpec{PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress}},
		},
	)

	var obs snapshot.Observation
	if err := c.collectNetworkPolicies(context.Background(), &obs); err != nil {
		t.Fatalf("collectNetworkPolicies = %v", err)
	}
	if len(obs.Policies) != 1 {
		t.Fatalf("policies = %+v, want one", obs.Policies)
	}
	got := obs.Policies[0]
	if got.UID != "np-uid" || got.Namespace != "default" || got.Name != "deny-all" {
		t.Errorf("policy identity = %+v", got)
	}
	for _, want := range []string{"apiVersion: networking.k8s.io/v1", "kind: NetworkPolicy"} {
		if !strings.Contains(got.Manifest, want) {
			t.Errorf("manifest is missing %q; it can no longer be read back as a manifest:\n%s", want, got.Manifest)
		}
	}
	if strings.Contains(got.Manifest, "managedFields") {
		t.Errorf("manifest still carries managedFields:\n%s", got.Manifest)
	}
}

func TestIngressBackendServicesCollectsEveryBackendDedupedAndSorted(t *testing.T) {
	ing := &netv1.Ingress{
		Spec: netv1.IngressSpec{
			DefaultBackend: &netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "web"}},
			Rules: []netv1.IngressRule{
				{IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{
					Paths: []netv1.HTTPIngressPath{
						{Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "api"}}},
						{Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "web"}}},
						{Backend: netv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "Bucket"}}},
					},
				}}},
				{IngressRuleValue: netv1.IngressRuleValue{HTTP: nil}},
			},
		},
	}

	got := ingressBackendServices(ing)
	if strings.Join(got, ",") != "api,web" {
		t.Errorf("ingressBackendServices = %v, want [api web]", got)
	}
}

func TestCollectIngressesEmitsOneGatewayPerBackend(t *testing.T) {
	c, _ := newTestCollector(t,
		&netv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "front", Namespace: "default"},
			Spec: netv1.IngressSpec{
				Rules: []netv1.IngressRule{{IngressRuleValue: netv1.IngressRuleValue{
					HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
						{Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "api"}}},
						{Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "web"}}},
					}},
				}}},
			},
		},
	)

	var obs snapshot.Observation
	if err := c.collectIngresses(context.Background(), &obs); err != nil {
		t.Fatalf("collectIngresses = %v", err)
	}
	if len(obs.Gateways) != 2 {
		t.Fatalf("gateways = %+v, want one per backend service", obs.Gateways)
	}
	for _, g := range obs.Gateways {
		if g.Kind != "Ingress" || g.ClusterID != testClusterID || g.Name != "front" {
			t.Errorf("gateway = %+v, want an Ingress named front in %s", g, testClusterID)
		}
	}
}

// Service 存在但没有就绪后端是一处平台知道得比表面上少的地方：
// 照它生成的放行规则指向空集，看起来齐备，实际什么都没放行。
func TestServicesWithoutEndpointsWarnsOnlyOnSelectorBackedServices(t *testing.T) {
	services := []snapshot.Service{
		{Namespace: "default", Name: "empty", Selector: map[string]string{"app": "empty"}},
		{Namespace: "default", Name: "backed", Selector: map[string]string{"app": "backed"}},
		{Namespace: "default", Name: "external"},
		{Namespace: "default", Name: "manual-endpoints"},
		{Namespace: "other", Name: "backed", Selector: map[string]string{"app": "backed"}},
	}
	endpoints := []snapshot.Endpoints{
		{Namespace: "default", Name: "backed", Addresses: []string{"10.0.1.5"}},
		// 记录存在但地址为空，等同于没有后端。
		{Namespace: "other", Name: "backed"},
	}

	got := servicesWithoutEndpoints(services, endpoints)
	var subjects []string
	for _, w := range got {
		if w.Kind != snapshot.WarningServiceWithoutEndpoints {
			t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningServiceWithoutEndpoints)
		}
		subjects = append(subjects, w.Subject)
	}
	if strings.Join(subjects, ",") != "default/empty,other/backed" {
		t.Errorf("warned about %v, want default/empty and other/backed only", subjects)
	}
}

func TestCollectWarnsAboutAServiceWithNoReadyBackend(t *testing.T) {
	c, _ := newTestCollector(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}},
		},
	)

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if len(run.Observation.Warnings) != 1 ||
		run.Observation.Warnings[0].Kind != snapshot.WarningServiceWithoutEndpoints {
		t.Fatalf("warnings = %+v, want one %q", run.Observation.Warnings, snapshot.WarningServiceWithoutEndpoints)
	}
}

// 没采到 EndpointSlice 时这条校验没有依据。照跑会把"我们没采到后端"
// 报成"所有 Service 都没有后端" —— 一个凭空生成的、看起来很具体的结论。
func TestCollectSkipsTheBackendCheckWhenEitherSideFailed(t *testing.T) {
	for _, failing := range []string{"services", "endpointslices"} {
		t.Run(failing, func(t *testing.T) {
			c, cs := newTestCollector(t,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
					Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}},
				},
			)
			failList(cs, failing, forbidden(failing))

			run, err := c.Collect(context.Background(), "run-1")
			if err != nil {
				t.Fatalf("Collect = %v", err)
			}
			for _, w := range run.Observation.Warnings {
				if w.Kind == snapshot.WarningServiceWithoutEndpoints {
					t.Errorf("emitted %q while %s could not be listed; that is an invented conclusion",
						w.Kind, failing)
				}
			}
		})
	}
}

// LoadBalancer 的入口地址与源地址范围必须采回来。
//
// 入口地址是判定「这个入口面向公网还是只在 VPC 内」的唯一依据（spec §2）——
// 不采它，平台只能去读云厂商注解，而那是每接一朵云就要改一次的代码，
// 漏掉一个键的后果是把内部 LB 当成公网入口、开一条 0.0.0.0/0。
func TestServiceSnapshotCarriesLoadBalancerExposure(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "uat-kafka", Name: "kafka-0-external"},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeLoadBalancer,
			Selector:                 map[string]string{"app.kubernetes.io/name": "kafka"},
			LoadBalancerSourceRanges: []string{"10.0.0.0/8", "172.16.0.0/16"},
			Ports: []corev1.ServicePort{{
				Name: "tcp-kafka", Port: 9094, Protocol: corev1.ProtocolTCP,
				TargetPort: intstr.FromString("kafka-external"),
			}},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "10.170.48.193"}},
			},
		},
	}
	c, _ := newTestCollector(t, svc)

	var obs snapshot.Observation
	if err := c.collectServices(context.Background(), &obs); err != nil {
		t.Fatalf("collectServices() = %v", err)
	}
	if len(obs.Services) != 1 {
		t.Fatalf("采到 %d 个 Service，want 1", len(obs.Services))
	}
	got := obs.Services[0]
	if !reflect.DeepEqual(got.LoadBalancerIngressIPs, []string{"10.170.48.193"}) {
		t.Errorf("LoadBalancerIngressIPs = %v, want [10.170.48.193]", got.LoadBalancerIngressIPs)
	}
	if !reflect.DeepEqual(got.LoadBalancerSourceRanges, []string{"10.0.0.0/8", "172.16.0.0/16"}) {
		t.Errorf("LoadBalancerSourceRanges = %v", got.LoadBalancerSourceRanges)
	}
}

// 没有入口地址的 Service（ClusterIP、NodePort、LB 尚未就绪）两个字段都为空，
// **不是零值填充**：spec §3.3 第 4 条要靠「取不到入口 IP」这个事实报缺口，
// 填一个空串会让它看起来像取到了。
func TestServiceWithoutLoadBalancerCarriesNoExposure(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "g32-game", Name: "backend-tcp"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app.kubernetes.io/name": "backend"},
			Ports:    []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	c, _ := newTestCollector(t, svc)

	var obs snapshot.Observation
	if err := c.collectServices(context.Background(), &obs); err != nil {
		t.Fatalf("collectServices() = %v", err)
	}
	if len(obs.Services[0].LoadBalancerIngressIPs) != 0 {
		t.Errorf("NodePort 却带了入口地址: %v", obs.Services[0].LoadBalancerIngressIPs)
	}
	if len(obs.Services[0].LoadBalancerSourceRanges) != 0 {
		t.Errorf("NodePort 却带了源地址范围: %v", obs.Services[0].LoadBalancerSourceRanges)
	}
}

// 入口条目只有主机名、没有 IP 时不得记进去。
//
// AWS 的 ELB 恒是这个形态（status.loadBalancer.ingress[].hostname），而主机名
// 判不出网段归属。留一个空串进去，推导层会把它当成「取到了入口地址」，于是
// 一个判不出暴露范围的 Service 被当成公网入口 —— 而正确的结果是它落进缺口
// 清单，由人来决定放行范围（design doc 2026-08-28 §6）。
func TestServiceWithHostnameOnlyIngressCarriesNoIngressIP(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-lb"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app.kubernetes.io/name": "api"},
			Ports:    []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(8080)}},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: "a1b2.elb.amazonaws.com"}},
			},
		},
	}
	c, _ := newTestCollector(t, svc)

	var obs snapshot.Observation
	if err := c.collectServices(context.Background(), &obs); err != nil {
		t.Fatalf("collectServices() = %v", err)
	}
	if len(obs.Services[0].LoadBalancerIngressIPs) != 0 {
		t.Errorf("只有主机名却记了入口地址: %v", obs.Services[0].LoadBalancerIngressIPs)
	}
}

func TestDedupeSorted(t *testing.T) {
	if got := dedupeSorted([]string(nil)); got != nil {
		t.Errorf("dedupeSorted(nil) = %v, want nil", got)
	}
	if got := dedupeSorted([]int32{9090, 8080, 8080}); len(got) != 2 || got[0] != 8080 || got[1] != 9090 {
		t.Errorf("dedupeSorted = %v, want [8080 9090]", got)
	}
}
