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
//
// 断言的是"依据里包含一条指向入口地址的 SourceService"，不是"全部依据都是
// SourceService"——后者会锁死 I5 的修复：命中已登记网段时还应该有一条
// SourceClusterRegistry 指向具体命中的那个字段（见下面的
// TestExposedIngressRegisteredMatchAlsoCitesTheRegistryField）。
func TestExposedIngressCarriesItsDerivation(t *testing.T) {
	a := assetsWith(svcLB("istio-system", "uat-istio-ingressgateway-extra",
		[]string{"34.150.1.177"}, nil, port("", 443)))
	rules := deriveExposedIngress(a, "istio-system")
	var found bool
	for _, d := range rules[0].Derivations {
		if d.SourceKind == SourceService && d.Field == "status.loadBalancer.ingress" {
			found = true
		}
	}
	if !found {
		t.Errorf("依据没有一条 SourceService 指向入口地址那一行: %+v", rules[0].Derivations)
	}
}

// I5：命中已登记网段时，那个 CIDR 实际来自 ClusterRegistry 的哪个字段，
// 推导依据要能指到那一行——不能只写"入口地址"，审计的人会被送到错的地方
// （derive_infra.go 的 deriveNodeAgent/deriveLBHealth 对 SourceClusterRegistry
// 是同一条纪律）。
func TestExposedIngressRegisteredMatchAlsoCitesTheRegistryField(t *testing.T) {
	a := assetsWith(svcLB("rocketmq", "rocketmq-nameserver",
		[]string{"10.170.48.55"}, nil, port("", 9876)))
	rules := deriveExposedIngress(a, "rocketmq")
	var found bool
	for _, d := range rules[0].Derivations {
		if d.SourceKind == SourceClusterRegistry && d.Field == "nodeCIDR" {
			found = true
		}
	}
	if !found {
		t.Errorf("命中 node_cidr 却没有 SourceClusterRegistry/nodeCIDR 依据: %+v", rules[0].Derivations)
	}
}

// NodePort 的对端来自 a.Registry.NodeCIDR，推导依据必须指到 nodeCIDR 这
// 个字段，不能只写 spec.type——spec.type 说明的是"为什么走节点网段"，
// 不是那个 CIDR 值的出处。
func TestExposedIngressNodePortAlsoCitesTheRegistryField(t *testing.T) {
	a := assetsWith(svcNodePort("metersphere2", "selenium-hub", port("", 4444)))
	rules := deriveExposedIngress(a, "metersphere2")
	var found bool
	for _, d := range rules[0].Derivations {
		if d.SourceKind == SourceClusterRegistry && d.Field == "nodeCIDR" {
			found = true
		}
	}
	if !found {
		t.Errorf("NodePort 却没有 SourceClusterRegistry/nodeCIDR 依据: %+v", rules[0].Derivations)
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

// --- Fix round 1 (design review 2026-08-28) ---

// C1: 每条规则的 Subject 必须来自 Service 的 selector，不能是空——空
// Subject 在 policygen 那一侧会被读成"广播给整个 namespace"，把这条规则
// 发给 shop/worker 这种没有暴露对象的 workload。
func TestExposedIngressCarriesSubjectFromServiceSelector(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb",
		[]string{"34.150.1.177"}, nil, port("", 8080)))
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 1 {
		t.Fatalf("生成 %d 条规则，want 1", len(rules))
	}
	if !reflect.DeepEqual(rules[0].Subject, map[string]string{"app": "api-lb"}) {
		t.Errorf("Subject = %v，want Service 的 selector {app: api-lb}", rules[0].Subject)
	}
}

// I8: 用一个落在 pod_cidr（而非 node_cidr）的入口地址钉住"命中的是哪一个
// 网段"这件事本身被测到了。此前把 registeredCIDRContaining 的返回值
// 硬改成恒返回 node_cidr，三个包的测试全绿——因为②号用例恰好落在
// node_cidr、③号用例哪个都不命中，从没有一条用例走到 pod_cidr 那条分支
// （design review I8）。
func TestExposedIngressUsesPodCIDRWhenTheIngressIPFallsThere(t *testing.T) {
	a := assetsWith(svcLB("batch", "batch-lb",
		[]string{"172.16.4.9"}, nil, port("", 8080)))
	rules := deriveExposedIngress(a, "batch")
	if len(rules) != 1 {
		t.Fatalf("生成 %d 条规则，want 1", len(rules))
	}
	if got := cidrsOf(rules[0]); !reflect.DeepEqual(got, []string{"172.16.0.0/16"}) {
		t.Errorf("对端 = %v，want [172.16.0.0/16]（入口地址落在 pod_cidr）", got)
	}
}

// I3: 多个入口地址（双栈、多可用区）落在不同的范围时，两个方向都危险——
// 认内网的那个会切断真实公网入口，认公网的那个会开一个不该开的
// 0.0.0.0/0。没有依据选哪个，因此不生成，报缺口；顺序不能改变这个结论，
// 这里两种顺序都测，钉住"不是取列表第一个"。
func TestExposedIngressDisagreeingIngressIPsGenerateNothing(t *testing.T) {
	for _, ips := range [][]string{
		{"10.170.48.9", "34.150.1.177"},
		{"34.150.1.177", "10.170.48.9"},
	} {
		a := assetsWith(svcLB("shop", "api-lb", ips, nil, port("", 8080)))
		rules := deriveExposedIngress(a, "shop")
		if len(rules) != 0 {
			t.Errorf("ips=%v: 生成了 %d 条规则，入口地址判定不一致时 want 0", ips, len(rules))
		}
	}
}

// I3 反面：多个入口地址一致时，正常生成——不能因为修了"不一致就报缺口"
// 而把"一致的多地址"也一起挡住了。
func TestExposedIngressAgreeingIngressIPsUseThatCIDR(t *testing.T) {
	a := assetsWith(svcLB("rocketmq", "rocketmq-nameserver",
		[]string{"10.170.48.55", "10.170.48.56"}, nil, port("", 9876)))
	rules := deriveExposedIngress(a, "rocketmq")
	if len(rules) != 1 {
		t.Fatalf("生成 %d 条规则，want 1（两个地址一致）", len(rules))
	}
	if got := cidrsOf(rules[0]); !reflect.DeepEqual(got, []string{"10.170.48.0/24"}) {
		t.Errorf("对端 = %v，want [10.170.48.0/24]", got)
	}
}

// I4: 入口地址本身解析不了，不是"不在任何注册网段"的证据——判不出就是
// 判不出，不能退化成 0.0.0.0/0。
func TestExposedIngressUnparsableIngressIPGeneratesNothing(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb",
		[]string{"not-an-ip"}, nil, port("", 8080)))
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 0 {
		t.Errorf("生成了 %d 条规则，入口地址解析失败时 want 0", len(rules))
	}
}

// I4: 登记的网段本身写错了（不是合法 CIDR），同样判不出——不能把"这段
// 解析不了"读成"这个地址不在这段里"从而落到"哪个都不命中 → 公网"。
// 一个打错的 node_cidr 不该把这个集群里每一个内部 LB 都变成 0.0.0.0/0。
func TestExposedIngressMalformedRegistryCIDRGeneratesNothing(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb",
		[]string{"8.8.8.8"}, nil, port("", 8080)))
	a.Registry.NodeCIDR = "not-a-cidr"
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 0 {
		t.Errorf("生成了 %d 条规则，node_cidr 登记畸形时 want 0（判不出，不是"+
			"「不在这段里」）", len(rules))
	}
}

// I7: exposedByLBOrNodePortService 只认 LoadBalancer/NodePort Service，
// 不像 exposed() 那样把 Gateway 也算进来——一个跑 ingress controller、
// 后端是 ClusterIP 的 namespace，EXPOSED_INGRESS 推不出规则（deriveExposedIngress
// 根本不处理 Gateway），若判据仍是 exposed()，这个 namespace 会永久卡在
// Missing() 里，永远补不上（design review I7）。
func TestExposedIngressNotApplicableWhenOnlyExposedByAnIngressWithClusterIPBackend(t *testing.T) {
	a := assetsWith(svcClusterIP("web", "web-api", port("", 8080)))
	a.Gateways = []snapshot.Gateway{{
		ClusterID: "c1", Namespace: "web", Name: "web-ingress",
		Kind: "Ingress", BackendService: "web-api",
	}}
	set := Derive(a, "web", nil)
	if !slices.Contains(set.NotApplicable, KindExposedIngress) {
		t.Errorf("只有 Ingress+ClusterIP 后端却没判成不适用: %v", set.NotApplicable)
	}
	if slices.Contains(set.Missing(), KindExposedIngress) {
		t.Errorf("EXPOSED_INGRESS 落进了永远补不上的 Missing(): %v", set.Missing())
	}
}

// --- Fix round 2 (design review 2026-08-28) ---

// NC1: 没有 selector 的 LoadBalancer Service（手工维护 Endpoints 的合法
// 形态，常见于外部后端）不能生成一条 Subject 为空的规则——那会被 policygen
// 读成"广播"，把 peers=[0.0.0.0/0] 发给这个 namespace 里完全无关的
// workload，正是修复前 C1 的原样复现。deriveExposedIngress 必须直接跳过，
// 不生成任何规则。
func TestExposedIngressSkipsServiceWithoutSelector(t *testing.T) {
	svc := svcLB("shop", "external-backend", []string{"34.150.1.177"}, nil, port("", 8080))
	svc.Selector = nil
	a := assetsWith(svc)
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 0 {
		t.Errorf("没有 selector 的 Service 生成了 %d 条规则，want 0", len(rules))
	}
}

// NC1 的另一半：跳过不等于消失。UnresolvedExposureSubjects 必须把这个
// Service 报出来，policygen 据此生成看得见的缺口（TestUnattachedBaselines*
// 系列在 policygen 包里验证端到端行为）。
func TestUnresolvedExposureSubjectsNamesTheSelectorlessService(t *testing.T) {
	svc := svcLB("shop", "external-backend", []string{"34.150.1.177"}, nil, port("", 8080))
	svc.Selector = nil
	a := assetsWith(svc)
	got := UnresolvedExposureSubjects(a, "shop")
	if len(got) != 1 || got[0].Namespace != "shop" || got[0].Name != "external-backend" {
		t.Errorf("UnresolvedExposureSubjects = %+v, want 一条 shop/external-backend", got)
	}
}

// 有 selector 的 Service 不该出现在 UnresolvedExposureSubjects 里——它有
// 主体可挂，不是这份清单要报的那种缺口。
func TestUnresolvedExposureSubjectsExcludesNormalServices(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb", []string{"34.150.1.177"}, nil, port("", 8080)))
	if got := UnresolvedExposureSubjects(a, "shop"); len(got) != 0 {
		t.Errorf("有 selector 的 Service 出现在 UnresolvedExposureSubjects 里: %+v", got)
	}
}

// --- Final review fixes (2026-08-28) ---

// C2：登记为空时不得判成面向公网。
//
// 空登记不是"查过、都不命中"，是根本没有判据。旧实现对空网段一个 continue
// 跳过去，落到 addrPublic —— 于是 10.128.0.5 这样一个明明白白在 RFC1918
// 节点网段里的入口地址被判成 0.0.0.0/0，一条把整个互联网放进来的规则。
// 登记打错时（TestExposedIngressMalformedRegistryCIDRGeneratesNothing）
// 已经按"判不出"处理，没登记是同一个危险更常见的版本。
func TestExposedIngressEmptyRegistryGeneratesNothing(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb",
		[]string{"10.128.0.5"}, nil, port("", 8080)))
	a.Registry.PodCIDR, a.Registry.NodeCIDR = "", ""
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 0 {
		t.Errorf("生成了 %d 条规则（对端 %v），空登记时 want 0 —— 判不出，"+
			"不是「面向公网」", len(rules), cidrsOf(rules[0]))
	}
}

// C2 的另一半：只缺一段登记同样判不出。
//
// node_cidr 没登记时，没有任何依据说 10.128.0.5 不是一个节点地址；
// pod_cidr 命中不了只说明它不是 Pod。判据不全就不许答"面向公网"，
// 与 internal/cluster 那条"登记不全时返回 UNKNOWN 而不假定它在集群外"
// 是同一条纪律。
func TestExposedIngressPartialRegistryGeneratesNothing(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb",
		[]string{"10.128.0.5"}, nil, port("", 8080)))
	a.Registry.NodeCIDR = ""
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 0 {
		t.Errorf("生成了 %d 条规则（对端 %v），node_cidr 未登记时 want 0",
			len(rules), cidrsOf(rules[0]))
	}
}

// I3：双栈登记是逗号分隔的多段（cluster.ParsePrefixes），归属判定必须认它。
//
// 旧实现拿整串去调 netip.ParsePrefix，直接失败 —— 于是双栈集群上每一个
// LoadBalancer 都推不出对端，报出一串并不存在的缺口。
func TestExposedIngressClassifiesAgainstDualStackRegistry(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb",
		[]string{"10.128.0.5"}, nil, port("", 8080)))
	a.Registry.NodeCIDR = "10.128.0.0/20,fd00:10:128::/64"
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 1 {
		t.Fatalf("生成 %d 条规则，want 1（入口地址落在双栈 node_cidr 的 v4 段里）",
			len(rules))
	}
	// 命中的是那条登记，对端就是这条登记的全部段：一个 v4 入口与一个 v6
	// 入口指的是同一片节点网络，按段拆开报只会让其中一半的流量落在规则外。
	want := []string{"10.128.0.0/20", "fd00:10:128::/64"}
	if got := cidrsOf(rules[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("对端 = %v，want %v", got, want)
	}
}

// I3：双栈 LB 的两个入口地址（v4 一个、v6 一个）命中的是同一条登记，
// 必须算"一致"。按网段字面量比会让它们互相不等，于是一条完全正常的
// 双栈 LB 被 I3 那条"判定不一致就报缺口"误伤。
func TestExposedIngressDualStackIngressIPsAgree(t *testing.T) {
	a := assetsWith(svcLB("shop", "api-lb",
		[]string{"10.128.0.5", "fd00:10:128::7"}, nil, port("", 8080)))
	a.Registry.NodeCIDR = "10.128.0.0/20,fd00:10:128::/64"
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 1 {
		t.Fatalf("生成 %d 条规则，want 1（两个入口地址命中同一条登记）", len(rules))
	}
}

// I3：NodePort 的对端也来自同一条登记，同样不得把整串塞进 IPBlock.CIDR。
//
// `cidr: "10.128.0.0/20,fd00:10:128::/64"` 是一个 NetworkPolicy 不认的值，
// 而候选策略在被 apply 之前谁都不会发现 —— 症状出现在 GitOps 合并之后。
func TestNodePortPeersSplitDualStackRegistry(t *testing.T) {
	a := assetsWith(svcNodePort("shop", "api-np", port("", 8080)))
	a.Registry.NodeCIDR = "10.128.0.0/20,fd00:10:128::/64"
	rules := deriveExposedIngress(a, "shop")
	if len(rules) != 1 {
		t.Fatalf("生成 %d 条规则，want 1", len(rules))
	}
	want := []string{"10.128.0.0/20", "fd00:10:128::/64"}
	if got := cidrsOf(rules[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("对端 = %v，want %v（一段一条 ipBlock，不是整串）", got, want)
	}
}

// M10：node_cidr 用不了时 NodePort 不生成规则。
//
// 少了这个判断，规则里是 `ipBlock: {cidr: ""}` —— 一条 apply 会被 API
// server 拒掉的策略，而拒绝发生在 GitOps 合并之后，症状是"文件推不上去"，
// 成因却在生成侧。这条守卫此前零覆盖：删掉它，全套测试仍然全绿。
func TestNodePortWithoutUsableNodeCIDRGeneratesNothing(t *testing.T) {
	for _, cidr := range []string{"", "not-a-cidr"} {
		a := assetsWith(svcNodePort("shop", "api-np", port("", 8080)))
		a.Registry.NodeCIDR = cidr
		rules := deriveExposedIngress(a, "shop")
		if len(rules) != 0 {
			t.Errorf("node_cidr=%q 生成了 %d 条规则（对端 %v），want 0",
				cidr, len(rules), cidrsOf(rules[0]))
		}
	}
}
