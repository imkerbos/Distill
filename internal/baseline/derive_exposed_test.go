package baseline

import (
	"reflect"
	"slices"
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// assetsWith 造一份最小的 Assets：给定 Service 列表，网段登记固定为
// pod_cidr=172.16.0.0/16、node_cidr=10.170.48.0/24。
func assetsWith(svcs ...snapshot.Service) snapshot.Assets {
	return snapshot.Assets{
		ClusterID: "c1",
		Services:  svcs,
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "172.16.0.0/16", NodeCIDR: "10.170.48.0/24",
		},
	}
}

// svcLB 造一个 LoadBalancer Service。
func svcLB(namespace, name string, ingressIPs, sourceRanges []string, ports ...snapshot.ServicePort) snapshot.Service {
	return snapshot.Service{
		ClusterID: "c1", Namespace: namespace, Name: name, Type: serviceTypeLoadBalancer,
		Selector:                 map[string]string{"app": name},
		Ports:                    ports,
		LoadBalancerIngressIPs:   ingressIPs,
		LoadBalancerSourceRanges: sourceRanges,
	}
}

// svcNodePort 造一个 NodePort Service。
func svcNodePort(namespace, name string, ports ...snapshot.ServicePort) snapshot.Service {
	return snapshot.Service{
		ClusterID: "c1", Namespace: namespace, Name: name, Type: serviceTypeNodePort,
		Selector: map[string]string{"app": name},
		Ports:    ports,
	}
}

// svcClusterIP 造一个 ClusterIP Service —— 不对外暴露，EXPOSED_INGRESS 应当
// 判它不适用而非缺失。
func svcClusterIP(namespace, name string, ports ...snapshot.ServicePort) snapshot.Service {
	return snapshot.Service{
		ClusterID: "c1", Namespace: namespace, Name: name, Type: "ClusterIP",
		Selector: map[string]string{"app": name},
		Ports:    ports,
	}
}

// port 造一个 Service 端口。name 非空时是命名端口（TargetPortName=name），
// 否则 num 既是 Service port 也是数字 targetPort。
func port(name string, num int32) snapshot.ServicePort {
	if name != "" {
		return snapshot.ServicePort{Name: name, Port: num, TargetPortName: name, Protocol: "TCP"}
	}
	return snapshot.ServicePort{Port: num, TargetPort: num, Protocol: "TCP"}
}

// portNum 造一个数字 Service 端口，Service port 与 targetPort 可以不同。
func portNum(svcPort, targetPort int32) snapshot.ServicePort {
	return snapshot.ServicePort{Port: svcPort, TargetPort: targetPort, Protocol: "TCP"}
}

// cidrsOf 取出一条规则入站对端里的全部 CIDR，按声明顺序。
func cidrsOf(r Rule) []string {
	var out []string
	for _, p := range r.Ingress.From {
		if p.IPBlock != nil {
			out = append(out, p.IPBlock.CIDR)
		}
	}
	return out
}

// ① 声明了 loadBalancerSourceRanges 就用它，优先于按入口地址推导。
//
// 运维显式写下的范围比平台推出来的准，而两者不一致时推导的那个只会更宽。
func TestExposedIngressPrefersDeclaredSourceRanges(t *testing.T) {
	a := assetsWith(svcLB("uat-kafka", "kafka-0-external",
		[]string{"10.170.48.193"}, []string{"10.0.0.0/8", "172.16.0.0/16"},
		port("kafka-external", 9094)))
	rules := deriveExposedIngress(a, "uat-kafka")

	if len(rules) != 1 {
		t.Fatalf("生成 %d 条规则，want 1", len(rules))
	}
	got := cidrsOf(rules[0])
	if !reflect.DeepEqual(got, []string{"10.0.0.0/8", "172.16.0.0/16"}) {
		t.Errorf("对端 = %v，want 声明的那两段", got)
	}
}

// ② 没有声明时，入口地址落在已登记网段内 → 用该网段。
func TestExposedIngressUsesTheRegisteredRangeTheIngressIPFallsIn(t *testing.T) {
	a := assetsWith(svcLB("rocketmq", "rocketmq-nameserver",
		[]string{"10.170.48.55"}, nil, port("", 9876)))
	rules := deriveExposedIngress(a, "rocketmq")
	if got := cidrsOf(rules[0]); !reflect.DeepEqual(got, []string{"10.170.48.0/24"}) {
		t.Errorf("对端 = %v，want [10.170.48.0/24]（入口地址落在 node_cidr）", got)
	}
}

// ③ 入口地址不在任何已登记网段 → 面向公网。
//
// 这是唯一该开 0.0.0.0/0 的形态。UAT 十二个暴露型 Service 里只有一个。
func TestExposedIngressOpensTheInternetOnlyForAPublicIngressIP(t *testing.T) {
	a := assetsWith(svcLB("istio-system", "uat-istio-ingressgateway-extra",
		[]string{"34.150.1.177"}, nil, port("", 443)))
	rules := deriveExposedIngress(a, "istio-system")
	if got := cidrsOf(rules[0]); !reflect.DeepEqual(got, []string{"0.0.0.0/0"}) {
		t.Errorf("对端 = %v，want [0.0.0.0/0]", got)
	}
}

// ④ LoadBalancer 取不到入口地址 → 不生成，报缺口。
//
// **不退化成 0.0.0.0/0，也不退化成静默不生成。** 前者是自信地开一个
// 可能不该开的口子，后者会让入口在下发之后无声中断。
//
// Missing() 那一半留给下一个任务：Derive 目前还没有接上 deriveExposedIngress
// （见 derive.go），因此这里只直接断言本函数的行为——没有入口地址、也没有
// 声明源范围时，一条规则都不生成。
func TestExposedIngressRefusesWhenTheIngressIPIsUnknown(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb", nil, nil, port("", 8080)))
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 0 {
		t.Errorf("取不到入口地址却生成了 %d 条放行规则，want 0", len(rules))
	}
}

// NodePort 的对端恒为节点网段，不走入口地址那条判据。
//
// 流量经 kube-proxy 到达 Pod 时源地址是节点地址（externalTrafficPolicy
// 缺省为 Cluster），因此在 Pod 侧看对端就是节点网段。
func TestExposedIngressOfNodePortIsTheNodeRange(t *testing.T) {
	a := assetsWith(svcNodePort("metersphere2", "selenium-hub", port("", 4444)))
	rules := deriveExposedIngress(a, "metersphere2")
	if len(rules) != 1 {
		t.Fatalf("NodePort 生成 %d 条规则，want 1 —— 它的范围是确定的", len(rules))
	}
	if got := cidrsOf(rules[0]); !reflect.DeepEqual(got, []string{"10.170.48.0/24"}) {
		t.Errorf("对端 = %v，want [10.170.48.0/24]", got)
	}
}

// 端口取 targetPort，命名端口原样写进规则。
//
// 取 Service port 会放开一个后端没监听的端口，真正的流量仍被挡；
// 而命名端口写成 0（TargetPort 的零值）会产出一条永远匹配不上的规则，
// 外观却完全正常。
//
// 第二个端口的 Service port（9308）故意不同于 targetPort（9200）：两者相等
// 时把 Port 错写成 TargetPort 的变异测不出来，断言会在两者都对的巧合下
// 照样通过——这是一条不会失败的测试，等于没测。
func TestExposedIngressPortsComeFromTargetPort(t *testing.T) {
	a := assetsWith(svcLB("uat-kafka", "kafka-0-external",
		[]string{"10.170.48.193"}, []string{"10.0.0.0/8"},
		port("kafka-external", 9094), portNum(9308, 9200)))
	rules := deriveExposedIngress(a, "uat-kafka")

	var got []string
	for _, p := range rules[0].Ingress.Ports {
		got = append(got, p.Port.String())
	}
	if !reflect.DeepEqual(got, []string{"kafka-external", "9200"}) {
		t.Errorf("端口 = %v，want [kafka-external 9200]", got)
	}
}

// 每条规则必须带推导依据 —— NewRule 拒绝空依据，这里断言依据指到了字段。
func TestExposedIngressCarriesItsDerivation(t *testing.T) {
	a := assetsWith(svcLB("istio-system", "uat-istio-ingressgateway-extra",
		[]string{"34.150.1.177"}, nil, port("", 443)))
	rules := deriveExposedIngress(a, "istio-system")
	var fields []string
	for _, d := range rules[0].Derivations {
		if d.SourceKind != SourceService {
			t.Errorf("依据来源 = %s, want SERVICE", d.SourceKind)
		}
		fields = append(fields, d.Field)
	}
	if !slices.Contains(fields, "status.loadBalancer.ingress") {
		t.Errorf("依据没有指向入口地址那一行: %v", fields)
	}
}

// Derive 必须把暴露型入站放行接出来。
//
// deriveExposedIngress 自己有测试，这一条守的是**调用方仍然在调它** ——
// 摘掉 Derive 里那一行，上面那些照样全绿，而入口网关会重新拿到零放行。
func TestDeriveIncludesExposedIngress(t *testing.T) {
	a := assetsWith(svcLB("istio-system", "uat-istio-ingressgateway-extra",
		[]string{"34.150.1.177"}, nil, port("", 443)))
	set := Derive(a, "istio-system", nil)

	var found bool
	for _, r := range set.Rules {
		if r.Kind == KindExposedIngress {
			found = true
		}
	}
	if !found {
		t.Fatal("Derive 没有产出 EXPOSED_INGRESS —— 入口网关会拿到零放行的 default-deny")
	}
}

// 没有暴露对象的 namespace，这一类不适用，而不是「缺失」。
//
// 判成缺失会让每个内部 namespace 都挂上一条永远补不上的缺口。
func TestExposedIngressIsNotApplicableWithoutAnyExposure(t *testing.T) {
	a := assetsWith(svcClusterIP("shop", "api", port("", 8080)))
	set := Derive(a, "shop", nil)
	if !slices.Contains(set.NotApplicable, KindExposedIngress) {
		t.Errorf("没有暴露对象却没判成不适用: %v", set.NotApplicable)
	}
}

// 有暴露对象、却推不出放行规则（LoadBalancer 拿不到入口地址）：这一类要落进
// Missing()，且不得落进 NotApplicable —— 这一对断言合起来才证明「这里确实
// 有要暴露的东西，只是我们判不出它的放行范围」而不是「这里根本没有暴露面」。
// Correction 1（Task 5）把这半条断言挪到了这里，因为它依赖 Derive 接线。
func TestExposedIngressWithUnknownIngressIPIsMissingNotInapplicable(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb", nil, nil, port("", 8080)))
	set := Derive(a, "shop", nil)

	for _, r := range set.Rules {
		if r.Kind == KindExposedIngress {
			t.Error("取不到入口地址却生成了放行规则")
		}
	}
	if slices.Contains(set.NotApplicable, KindExposedIngress) {
		t.Error("有 LoadBalancer 却被判成「不适用」—— 缺口会就此消失")
	}
	if !slices.Contains(set.Missing(), KindExposedIngress) {
		t.Errorf("取不到入口地址却没报缺口，Missing = %v", set.Missing())
	}
}

// **既有缺陷同修**：LB 健康检查的端口在命名端口下写成了 0。
//
// intstr.FromInt32(p.TargetPort) 在 TargetPortName 非空时 TargetPort 是 0，
// 而 0 是合法端口值 —— 一条指向端口 0 的规则永远匹配不上，外观完全正常。
// UAT 的 kafka-0-external 正是这个形态。
func TestLBHealthCheckResolvesNamedTargetPorts(t *testing.T) {
	a := assetsWith(svcLB("uat-kafka", "kafka-0-external",
		[]string{"10.170.48.193"}, nil, port("kafka-external", 9094)))
	a.Registry.HealthCheckSources = []string{"35.191.0.0/16"}
	set := Derive(a, "uat-kafka", nil)

	for _, r := range set.Rules {
		if r.Kind != KindLBHealth {
			continue
		}
		for _, p := range r.Ingress.Ports {
			if p.Port.String() == "0" {
				t.Fatal("LB 健康检查指向端口 0 —— 命名端口没被解析，规则永远匹配不上")
			}
		}
	}
}
