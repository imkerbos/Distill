package policygen_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
)

// importedPolicy 造一条导入策略：选中 app=<workload>，放行到指定端口。
func importedPolicy(ns, workload string, port int32, dir replay.Direction) policygen.ImportedPolicy {
	proto := corev1.ProtocolTCP
	p := intstr.FromInt32(port)
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &p}}
	pol := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "manual-" + workload},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": workload}},
		},
	}
	if dir == replay.DirectionEgress {
		pol.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{Ports: ports}}
	} else {
		pol.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{Ports: ports}}
	}
	return policygen.ImportedPolicy{ImportID: "imp-1", Policy: pol}
}

// podsFor 造一份最小名册：一个带 app 标签的 Pod。
func podsFor(cluster, ns, workload string) []replay.PodRef {
	return []replay.PodRef{{
		ClusterID: cluster, Namespace: ns, Name: workload + "-1",
		Labels: map[string]string{"app": workload},
	}}
}

// 导入的规则进候选集，挂在正确的主体上，来源可区分。
//
// 这条路存在的理由是观测看不见的东西：月结批处理、季度对账、只在故障时走的
// 灾备链路 —— 不在窗口里就学不出规则，而 dry-run 也报不出来。
func TestImportedPolicyBecomesACandidateRule(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      podsFor("c1", "batch", "worker"),
		Imports:   []policygen.ImportedPolicy{importedPolicy("batch", "worker", 5432, replay.DirectionEgress)},
	})

	if len(res.UnattachedImports) != 0 {
		t.Fatalf("导入没挂上：%+v", res.UnattachedImports)
	}
	var found *policygen.Rule
	for _, c := range res.Policies {
		if c.Namespace != "batch" || c.Workload != "worker" {
			continue
		}
		for i, r := range c.Rules {
			if r.Origin == policygen.OriginImported {
				found = &c.Rules[i]
			}
		}
	}
	if found == nil {
		t.Fatalf("候选集里没有导入来源的规则：%+v", res.Policies)
	}
	if found.Direction != replay.DirectionEgress {
		t.Errorf("Direction = %s, want EGRESS", found.Direction)
	}
	if !found.Enabled {
		t.Error("导入规则默认没启用 —— 导入本身就是一次带审计的明示决定，" +
			"再要一次确认是把同一个决定问两遍")
	}
	// 展示视图与指纹必须齐备：人工确认挂在指纹上，缺了它这条规则禁不掉。
	if found.Fingerprint == "" {
		t.Error("导入规则没有指纹，它无法被人工禁用")
	}
	if len(found.Ports) == 0 {
		t.Errorf("导入规则没有端口展示视图：%+v", found)
	}
}

// FlowCount 为 0 但来源可区分。
//
// **那个 0 不是"没有流量"**：导入这条路存在的理由正是那条连接不在观测里。
// 把它与 LEARNED 混成一栏，一条人工补上的月结批处理规则会显示成
// "没人用、可以收紧"。
func TestImportedRuleIsDistinguishableFromAnUnusedLearnedRule(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      podsFor("c1", "batch", "worker"),
		Imports:   []policygen.ImportedPolicy{importedPolicy("batch", "worker", 5432, replay.DirectionEgress)},
	})
	for _, c := range res.Policies {
		for _, r := range c.Rules {
			if r.Origin != policygen.OriginImported {
				continue
			}
			if r.FlowCount != 0 {
				t.Errorf("FlowCount = %d, want 0", r.FlowCount)
			}
			if r.Evidence != "" {
				t.Errorf("Evidence = %q，导入规则不是学出来的，不该带学习证据类别", r.Evidence)
			}
			if r.Origin == policygen.OriginLearned {
				t.Error("导入规则被标成了 LEARNED")
			}
		}
	}
}

// 一条策略里的每一段 ingress/egress 各成一条规则。
//
// 整条折成一行，操作者就只能整条禁用，而他想否掉的多半只是其中一段。
func TestEachImportedRuleSectionBecomesItsOwnRule(t *testing.T) {
	proto := corev1.ProtocolTCP
	a, b := intstr.FromInt32(5432), intstr.FromInt32(6379)
	pol := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "manual"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &a}}},
				{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &b}}},
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &a}}},
			},
		},
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: podsFor("c1", "batch", "worker"),
		Imports: []policygen.ImportedPolicy{{ImportID: "i", Policy: pol}},
	})

	var eg, in int
	seen := map[string]bool{}
	for _, c := range res.Policies {
		for _, r := range c.Rules {
			if r.Origin != policygen.OriginImported {
				continue
			}
			if seen[r.Fingerprint] {
				t.Errorf("两条导入规则撞了同一个指纹 %s —— 人工确认会同时作用于两条",
					r.Fingerprint)
			}
			seen[r.Fingerprint] = true
			if r.Direction == replay.DirectionEgress {
				eg++
			} else {
				in++
			}
		}
	}
	if eg != 2 || in != 1 {
		t.Errorf("拆出 %d 条 egress / %d 条 ingress, want 2 / 1", eg, in)
	}
}

// 挂不上主体的导入必须报出来，不静默丢弃。
//
// 一条导入进来了却没出现在候选集里，操作者会以为它生效了 —— 而它恰恰是用来
// 补那条平台看不见的连接的，"以为补上了"比"知道没补上"危险得多。
func TestUnattachableImportsAreReportedNotDropped(t *testing.T) {
	// 没有 workload 归属标签：空 podSelector 选中整个 namespace。
	wide := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "wide"},
		Spec: networkingv1.NetworkPolicySpec{
			Egress: []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
	// 一条规则都没有：空规则是 default-deny，那是收紧，不是补充放行。
	empty := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "empty"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
		},
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: podsFor("c1", "batch", "worker"),
		Imports: []policygen.ImportedPolicy{
			{ImportID: "wide", Policy: wide},
			{ImportID: "empty", Policy: empty},
			// 集群里没有这个 workload 的 Pod：挂上去会生成一条选不中任何
			// Pod 的幽灵策略，它不报错，只是永远不生效。
			importedPolicy("batch", "ghost", 443, replay.DirectionEgress),
		},
	})

	got := map[string]policygen.UnattachedReason{}
	for _, u := range res.UnattachedImports {
		got[u.ImportID] = u.Reason
	}
	if len(got) != 3 {
		t.Fatalf("报出 %d 条挂不上的导入, want 3：%+v", len(got), res.UnattachedImports)
	}
	if got["wide"] != policygen.UnattachedNoWorkloadLabel {
		t.Errorf("wide 的原因 = %q", got["wide"])
	}
	if got["empty"] != policygen.UnattachedNoRules {
		t.Errorf("empty 的原因 = %q", got["empty"])
	}
	if got["imp-1"] != policygen.UnattachedNoSuchWorkload {
		t.Errorf("ghost 的原因 = %q", got["imp-1"])
	}
	// 挂不上的一条都不该进候选集。
	for _, c := range res.Policies {
		for _, r := range c.Rules {
			if r.Origin == policygen.OriginImported {
				t.Errorf("挂不上的导入仍然进了候选集：%s/%s", c.Namespace, c.Workload)
			}
		}
	}
}

// 命中风险端口的导入照旧标注，但不因此停用。
//
// 命中风险清单说明"这条放行值得看一眼"，而操作者已经看过了 —— 他就是写下
// 它的那个人。标注留着，是为了让下一个读这一屏的人也看见。
func TestImportedRuleOnARiskyPortStaysEnabledButFlagged(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: podsFor("c1", "batch", "worker"),
		Imports: []policygen.ImportedPolicy{importedPolicy("batch", "worker", 22, replay.DirectionEgress)},
	})
	var found bool
	for _, c := range res.Policies {
		for _, r := range c.Rules {
			if r.Origin != policygen.OriginImported {
				continue
			}
			found = true
			if r.Risk == nil {
				t.Error("22 端口没有被标成风险端口，下一个读这屏的人看不见它")
			}
			if !r.Enabled {
				t.Error("风险端口把一条人工明示的导入停用了")
			}
		}
	}
	if !found {
		t.Fatal("导入没进候选集")
	}
}

// 端口落在 1..65535 之外时不参与风险判定。
//
// 注意边界在哪：intstr.IntOrString.IntVal 本身就是 int32，**截断发生在构造
// 那一侧**，而不是这里 —— 一个 4294967318 在 intstr.FromInt 里就已经变成 22，
// 等它到达本函数时已经是一个合法端口，任何下游检查都救不回来。真实路径上
// 这种值进不来（YAML 解析到 int32 会直接报错），因此这里只守本函数能守的
// 那一段：不把范围外的取值硬转成 int32 送进风险查询。
func TestOutOfRangePortsDoNotWrapIntoRiskLookups(t *testing.T) {
	proto := corev1.ProtocolTCP
	// 65558 = 22 + 65536，int32 装得下所以不会被 intstr 截断 —— 它检验的正是
	// 本函数的那道范围检查：硬转的话它会原样送进 risk.Lookup，而 22 那条
	// 风险登记描述的是 SSH，与这条规则毫无关系。
	p := intstr.FromInt32(65558)
	pol := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "oob"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &p}},
			}},
		},
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: podsFor("c1", "batch", "worker"),
		Imports: []policygen.ImportedPolicy{{ImportID: "i", Policy: pol}},
	})
	var seen bool
	for _, c := range res.Policies {
		for _, r := range c.Rules {
			if r.Origin != policygen.OriginImported {
				continue
			}
			seen = true
			if r.Risk != nil {
				t.Errorf("范围外的端口 65558 命中了风险端口 %d", r.Risk.Port)
			}
		}
	}
	if !seen {
		t.Fatal("规则没进候选集，上面的检查等于没跑")
	}
}

// 命名端口同样不猜：风险清单按数字端口登记，而名字要对着 containerPort 解析。
func TestNamedPortsAreNotGuessedIntoRiskLookups(t *testing.T) {
	proto := corev1.ProtocolTCP
	named := intstr.FromString("ssh")
	pol := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "named"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &named}},
			}},
		},
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: podsFor("c1", "batch", "worker"),
		Imports: []policygen.ImportedPolicy{{ImportID: "i", Policy: pol}},
	})
	for _, c := range res.Policies {
		for _, r := range c.Rules {
			if r.Origin == policygen.OriginImported && r.Risk != nil {
				t.Errorf("命名端口 %q 被猜成了风险端口 %d", named.StrVal, r.Risk.Port)
			}
		}
	}
}

// 没有挂不上的导入时是**空数组，不是 nil**。
//
// Generate 一定跑过那一段，因此"一条都没有"是一个算过的空集；序列化成 null
// 会被界面读成"这一栏没人算过"，而那正是这一栏要消除的状态。
func TestNoUnattachedImportsIsAnEmptySliceNotNil(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: podsFor("c1", "batch", "worker"),
	})
	if res.UnattachedImports == nil {
		t.Error("UnattachedImports 是 nil —— 它会序列化成 null，" +
			"读起来是「没人算过」，而事实是算过、没有")
	}
	if len(res.UnattachedImports) != 0 {
		t.Errorf("UnattachedImports = %+v, want 空", res.UnattachedImports)
	}
}
