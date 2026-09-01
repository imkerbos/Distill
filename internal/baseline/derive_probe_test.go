package baseline_test

import (
	"slices"
	"testing"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// probeAssets 是一份只够推 KUBELET_PROBE 的最小资产。
func probeAssets(targets ...snapshot.ProbeTarget) snapshot.Assets {
	return snapshot.Assets{
		ClusterID:    "uat-1",
		ProbeTargets: targets,
		Registry: snapshot.ClusterRegistry{
			ClusterID: "uat-1", NodeCIDR: "10.170.48.0/20", PodCIDR: "10.4.0.0/14",
		},
	}
}

func probeTarget(ns, key, wl string, ports ...int32) snapshot.ProbeTarget {
	t := snapshot.ProbeTarget{
		ClusterID: "uat-1", Namespace: ns, WorkloadKey: key, Workload: wl,
	}
	for _, p := range ports {
		t.Ports = append(t.Ports, snapshot.NamedPort{Port: p, Protocol: "TCP"})
	}
	return t
}

// probeRules 挑出一个集合里的 KUBELET_PROBE 规则。
func probeRules(s baseline.Set) []baseline.Rule {
	var out []baseline.Rule
	for _, r := range s.Rules {
		if r.Kind == baseline.KindKubeletProbe {
			out = append(out, r)
		}
	}
	return out
}

// 对端必须是节点网段。kubelet 用宿主网络，探测的源地址是节点 IP——
// 写成 podSelector 会得到一条外观正确、永远选不中的规则，症状是
// Pod 被判不健康后杀掉重启。
func TestKubeletProbeAllowsTheNodeCIDRToTheDeclaredPort(t *testing.T) {
	a := probeAssets(probeTarget("uat-app", "app", "baccarat-resource-backend", 8088))
	rules := probeRules(baseline.Derive(a, "uat-app", nil))
	if len(rules) != 1 {
		t.Fatalf("推出了 %d 条 KUBELET_PROBE，want 1", len(rules))
	}
	r := rules[0]
	if r.Direction != replay.DirectionIngress {
		t.Errorf("Direction = %s, want INGRESS", r.Direction)
	}
	if r.Ingress == nil {
		t.Fatal("入站规则体为空")
	}
	if len(r.Ingress.From) != 1 || r.Ingress.From[0].IPBlock == nil {
		t.Fatalf("对端不是 ipBlock: %+v —— podSelector 选不中 kubelet", r.Ingress.From)
	}
	if got := r.Ingress.From[0].IPBlock.CIDR; got != "10.170.48.0/20" {
		t.Errorf("对端网段 = %q, want 10.170.48.0/20", got)
	}
	if len(r.Ingress.Ports) != 1 || r.Ingress.Ports[0].Port.IntValue() != 8088 {
		t.Errorf("端口 = %+v, want 8088", r.Ingress.Ports)
	}
	// 挂在这一个 workload 上，不广播：同 namespace 里探针端口是 80 的
	// 前端服务不该拿到一条 8088 的放行。
	if got := r.Subject["app"]; got != "baccarat-resource-backend" {
		t.Errorf("Subject = %v, want {app: baccarat-resource-backend}", r.Subject)
	}
}

// podSelector 必须用**实际命中的**归属键构造。一个按 k8s-app 归属的
// workload 被拼成 {app: ...}，集群里没有任何 Pod 命中，是一条幽灵策略。
func TestKubeletProbeUsesTheWorkloadsOwnLabelKey(t *testing.T) {
	a := probeAssets(probeTarget("kube-system", "k8s-app", "kube-dns", 8081))
	rules := probeRules(baseline.Derive(a, "kube-system", nil))
	if len(rules) != 1 {
		t.Fatalf("推出了 %d 条，want 1", len(rules))
	}
	if got := rules[0].Subject["k8s-app"]; got != "kube-dns" {
		t.Errorf("Subject = %v, want {k8s-app: kube-dns}", rules[0].Subject)
	}
}

// 每条规则都要能溯源到「哪个 workload 的哪个探针」与「网段从哪来」。
// 只记其一会把审计的人指向错的地方。
func TestKubeletProbeRecordsBothTheProbeAndTheNodeCIDR(t *testing.T) {
	a := probeAssets(probeTarget("uat-app", "app", "api", 8088))
	rules := probeRules(baseline.Derive(a, "uat-app", nil))
	if len(rules) != 1 {
		t.Fatalf("推出了 %d 条，want 1", len(rules))
	}
	var probe, registry bool
	for _, d := range rules[0].Derivations {
		switch d.SourceKind {
		case baseline.SourcePodProbe:
			probe = true
			if d.Name != "api" || d.Namespace != "uat-app" {
				t.Errorf("探针依据指错了对象: %+v", d)
			}
		case baseline.SourceClusterRegistry:
			registry = true
			if d.Field != "nodeCIDR" {
				t.Errorf("网段依据的字段 = %q, want nodeCIDR", d.Field)
			}
		}
	}
	if !probe || !registry {
		t.Errorf("依据不全 (probe=%v registry=%v): %+v", probe, registry, rules[0].Derivations)
	}
}

// 多个端口各出一条 port 项；多个 workload 各出一条规则，端口互不串。
func TestKubeletProbeKeepsEachWorkloadsPortsToItself(t *testing.T) {
	a := probeAssets(
		probeTarget("uat-app", "app", "backend", 8088, 8081),
		probeTarget("uat-app", "app", "frontend", 80),
	)
	rules := probeRules(baseline.Derive(a, "uat-app", nil))
	if len(rules) != 2 {
		t.Fatalf("推出了 %d 条，want 2", len(rules))
	}
	byWorkload := map[string][]int{}
	for _, r := range rules {
		wl := r.Subject["app"]
		for _, p := range r.Ingress.Ports {
			byWorkload[wl] = append(byWorkload[wl], p.Port.IntValue())
		}
	}
	if got := byWorkload["backend"]; !slices.Equal(got, []int{8088, 8081}) {
		t.Errorf("backend 的端口 = %v, want [8088 8081]", got)
	}
	if got := byWorkload["frontend"]; !slices.Equal(got, []int{80}) {
		t.Errorf("frontend 的端口 = %v, want [80] —— 串进了别的 workload 的端口", got)
	}
}

// 只跨 namespace 挑自己那一份：一个 namespace 的探针端口不该出现在
// 另一个 namespace 的候选策略里。
func TestKubeletProbeOnlyDerivesTheRequestedNamespace(t *testing.T) {
	a := probeAssets(
		probeTarget("uat-app", "app", "backend", 8088),
		probeTarget("uat-other", "app", "other", 9999),
	)
	rules := probeRules(baseline.Derive(a, "uat-app", nil))
	if len(rules) != 1 || rules[0].Subject["app"] != "backend" {
		t.Fatalf("推出了 %+v —— 串了别的 namespace", rules)
	}
}

// 一个走网络的探针都没有（全是 exec，或干脆没声明）→ **不适用**，不是缺失。
//
// 两者的处置不同：不适用不用做任何事，缺失要去补依据。报成缺失是一条
// 永远在喊、而且喊错的告警，会把整份清单的可信度一起拖垮。
func TestANamespaceWithoutProbesReportsInapplicableNotMissing(t *testing.T) {
	a := probeAssets(probeTarget("uat-app", "app", "batch-worker"))
	set := baseline.Derive(a, "uat-app", nil)
	if !slices.Contains(set.NotApplicable, baseline.KindKubeletProbe) {
		t.Errorf("NotApplicable = %v, 少了 KUBELET_PROBE", set.NotApplicable)
	}
	if slices.Contains(set.Missing(), baseline.KindKubeletProbe) {
		t.Error("没有探针的 namespace 被报成缺 KUBELET_PROBE —— 那是一条喊错的告警")
	}
}

// 有探针、却没登记 node CIDR → **缺失**，不是不适用。
//
// 推不出对端时不臆造一个：一条 ipBlock.cidr="" 的规则会在 GitOps 合并之后
// 才被 kubectl 拒掉，那时症状是一份推不上去的策略文件，而成因在这里。
func TestProbesWithoutANodeCIDRAreReportedMissing(t *testing.T) {
	a := probeAssets(probeTarget("uat-app", "app", "backend", 8088))
	a.Registry.NodeCIDR = ""
	set := baseline.Derive(a, "uat-app", nil)
	if len(probeRules(set)) != 0 {
		t.Error("没有网段依据却推出了规则")
	}
	if !slices.Contains(set.Missing(), baseline.KindKubeletProbe) {
		t.Errorf("Missing() = %v, 少了 KUBELET_PROBE —— 有探针却推不出放行正是缺口的定义", set.Missing())
	}
	if slices.Contains(set.NotApplicable, baseline.KindKubeletProbe) {
		t.Error("被判成不适用 —— 那会让这个 namespace 悄悄绕过门禁")
	}
}

// 解不出归属标签的 workload 不生成规则：一条 Subject 为空的规则会被下游
// 读成「广播」，把这个 workload 的探针端口放行给整个 namespace。
func TestAProbeTargetWithoutAWorkloadLabelProducesNoRule(t *testing.T) {
	a := probeAssets(probeTarget("uat-app", "", "", 8088))
	if rules := probeRules(baseline.Derive(a, "uat-app", nil)); len(rules) != 0 {
		t.Errorf("推出了 %+v —— Subject 为空会被读成广播", rules)
	}
}

// 这一类没采回依据时一个都不许判成「不适用」：资产里「这个 namespace
// 没有探针」与「我们没看过」长得一模一样，把后者读成前者就是把一次采集
// 失败变成一次放行。
func TestAnUnassessedProbeKindIsNeverInapplicable(t *testing.T) {
	a := probeAssets()
	set := baseline.Derive(a, "uat-app", []baseline.Kind{baseline.KindKubeletProbe})
	if slices.Contains(set.NotApplicable, baseline.KindKubeletProbe) {
		t.Error("没采回依据却被判成不适用 —— 一次采集失败变成一次放行")
	}
	if !slices.Contains(set.Missing(), baseline.KindKubeletProbe) {
		t.Errorf("Missing() = %v, 少了 KUBELET_PROBE", set.Missing())
	}
}
