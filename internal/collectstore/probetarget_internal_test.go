package collectstore

import (
	"reflect"
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
)

func probePod(ns, name string, labels map[string]string, ports ...int32) observedPod {
	p := observedPod{
		namespace: ns, name: name, labels: labels, probesCollected: true,
	}
	for _, port := range ports {
		p.probePorts = append(p.probePorts, snapshot.NamedPort{Port: port, Protocol: "TCP"})
	}
	return p
}

// 按 workload 聚合，不按 Pod：一个 20 副本的 Deployment 只出一条，
// 否则候选策略里会有 20 条一模一样的规则。
func TestProbeTargetsAggregateReplicasIntoOneWorkload(t *testing.T) {
	labels := map[string]string{"app": "backend"}
	got := probeTargetsOf("uat-1", []observedPod{
		probePod("uat-app", "backend-0", labels, 8088),
		probePod("uat-app", "backend-1", labels, 8088),
		probePod("uat-app", "backend-2", labels, 8088),
	})
	want := []snapshot.ProbeTarget{{
		ClusterID: "uat-1", Namespace: "uat-app",
		WorkloadKey: "app", Workload: "backend",
		Ports: []snapshot.NamedPort{{Port: 8088, Protocol: "TCP"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("probeTargetsOf() = %+v, want %+v", got, want)
	}
}

// 滚动更新的两侧探针端口不同时取并集：漏掉的那一侧会在下发之后被 kubelet
// 判成不健康，而多放行的那个端口本来就是这个 workload 自己的探针端口。
//
// 端口排序固定：不排序的话同一份快照会生成内容相同、字节不同的 YAML，
// GitOps 上表现为一次没有实质变化的 diff。
func TestProbeTargetsUnionPortsAcrossReplicasInAStableOrder(t *testing.T) {
	labels := map[string]string{"app": "backend"}
	got := probeTargetsOf("uat-1", []observedPod{
		probePod("uat-app", "backend-new", labels, 8081),
		probePod("uat-app", "backend-old", labels, 8088),
	})
	if len(got) != 1 {
		t.Fatalf("聚合出 %d 个 workload，want 1", len(got))
	}
	want := []snapshot.NamedPort{{Port: 8081, Protocol: "TCP"}, {Port: 8088, Protocol: "TCP"}}
	if !reflect.DeepEqual(got[0].Ports, want) {
		t.Errorf("Ports = %+v, want %+v（并集，按端口号排序）", got[0].Ports, want)
	}
}

// 归属键取实际命中的那个，不固定假设 app：一个按 k8s-app 归属的 workload
// 被拼成 {app: ...}，集群里没有任何 Pod 命中，是一条幽灵策略。
func TestProbeTargetsCarryTheWorkloadsOwnLabelKey(t *testing.T) {
	got := probeTargetsOf("uat-1", []observedPod{
		probePod("kube-system", "coredns-0", map[string]string{"k8s-app": "kube-dns"}, 8081),
	})
	if len(got) != 1 {
		t.Fatalf("聚合出 %d 个 workload，want 1", len(got))
	}
	if got[0].WorkloadKey != "k8s-app" || got[0].Workload != "kube-dns" {
		t.Errorf("归属 = %s=%s, want k8s-app=kube-dns", got[0].WorkloadKey, got[0].Workload)
	}
}

// 一个探针都没有的 workload 仍然要出现在列表里，端口为空 —— 那是
// KUBELET_PROBE 的「不适用」一档的依据。整条丢掉的话，
// probeDeclared 判出来的结果一样，但一个 namespace 里所有 workload 都没有
// 探针与「这个 namespace 里根本没有 workload」就分不开了。
func TestAWorkloadWithoutProbesIsStillListedWithNoPorts(t *testing.T) {
	got := probeTargetsOf("uat-1", []observedPod{
		probePod("uat-app", "worker-0", map[string]string{"app": "worker"}),
	})
	if len(got) != 1 {
		t.Fatalf("聚合出 %d 个 workload，want 1", len(got))
	}
	if len(got[0].Ports) != 0 {
		t.Errorf("Ports = %+v, want 空", got[0].Ports)
	}
}

// 解不出归属标签的 Pod 跳过：它在候选策略里也没有主体可挂，
// 挂一个空主体会被下游读成「广播给整个 namespace」。
func TestAPodWithoutAWorkloadLabelIsSkipped(t *testing.T) {
	got := probeTargetsOf("uat-1", []observedPod{
		probePod("uat-app", "orphan-0", map[string]string{"release": "canary"}, 8088),
	})
	if len(got) != 0 {
		t.Errorf("probeTargetsOf() = %+v, want 空", got)
	}
}

// **NULL 与空数组不是同一件事。**
//
// 空数组是「采过，这个 Pod 没有走网络的探针」——KUBELET_PROBE 的不适用一档；
// NULL 是「这一行写在 migrations/000036 之前，我们没看过」。混成一个，
// 升级之后到下一次采集之前每个 namespace 都会被判成不需要这条基线，
// 从缺失清单里消失——一次数据缺口变成一次放行。
func TestProbesCollectedDistinguishesNeverCollectedFromNoProbes(t *testing.T) {
	labels := map[string]string{"app": "backend"}
	collected := probePod("uat-app", "backend-0", labels)
	never := collected
	never.probesCollected = false

	if !probesCollected([]observedPod{collected}) {
		t.Error("采过、只是没有探针的行被判成「没采过」—— 会被误报成数据缺口")
	}
	if probesCollected([]observedPod{never}) {
		t.Error("迁移之前写下的行被判成「采过」—— 一次数据缺口变成一次放行")
	}
	// 一行采过就算采过：那一列是整批一起写的。
	if !probesCollected([]observedPod{never, collected}) {
		t.Error("有采过的行却判成没采过")
	}
	// 一个 Pod 都没有时算采过：那时没有任何 workload 要放行。
	if !probesCollected(nil) {
		t.Error("空快照被判成没采过 —— 会给一个没有 Pod 的集群报一条不存在的盲区")
	}
}

// **hostNetwork Pod 不产生探针主体。**
//
// 它不受 NetworkPolicy 管控，候选策略的花名册明确把它排除
// （generate.go 的 ExclusionHostNetwork）。为它推一条探针规则的后果不是
// 多一条无害的放行，而是一个**永远挂不上**的主体：花名册里没有这个
// workload，规则进 UnattachedBaselines，写回门禁据此拒绝出计划，而操作者
// 无论怎么改标签都消不掉它。UAT 上 monitoring/prometheus-node-exporter
// 正是这样把整个集群的写回卡死的。
func TestHostNetworkPodsProduceNoProbeTarget(t *testing.T) {
	hn := probePod("monitoring", "node-exporter-abc",
		map[string]string{"app": "prometheus-node-exporter"}, 9100)
	hn.hostNetwork = true
	got := probeTargetsOf("uat-1", []observedPod{hn})
	if len(got) != 0 {
		t.Errorf("probeTargetsOf() = %+v, want 空 —— 这个主体永远挂不上，会把写回门禁卡死", got)
	}
}

// 同一个 workload 混着 hostNetwork 与普通 Pod 时，只按普通 Pod 出主体。
func TestAMixedWorkloadKeepsOnlyTheGovernedPods(t *testing.T) {
	labels := map[string]string{"app": "mixed"}
	hn := probePod("uat-app", "mixed-host", labels, 9100)
	hn.hostNetwork = true
	got := probeTargetsOf("uat-1", []observedPod{hn, probePod("uat-app", "mixed-pod", labels, 8088)})
	if len(got) != 1 {
		t.Fatalf("聚合出 %d 个，want 1", len(got))
	}
	if len(got[0].Ports) != 1 || got[0].Ports[0].Port != 8088 {
		t.Errorf("Ports = %+v, want 只有 8088 —— hostNetwork Pod 的端口不该并进来", got[0].Ports)
	}
}
