package policygen_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// generateWith 用给定的 Pod 名册与单个暴露型 Service 跑一次生成。集群 ID
// 固定为 "c1"：这组测试只关心 Service selector 与 workload podSelector
// 之间的落差，不需要走完整 fixture 的装配。
func generateWith(t *testing.T, pods []replay.PodRef, svc snapshot.Service) policygen.Result {
	t.Helper()
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services:  []snapshot.Service{svc},
	}
	return policygen.Generate(policygen.Input{ClusterID: "c1", Pods: pods, Assets: assets})
}

// threeZookeeperPods 是一个三副本 StatefulSet：三个 Pod 共享 app: zookeeper，
// 各自另带 statefulset.kubernetes.io/pod-name 区分身份——per-Pod Service
// 常见的点名依据。
func threeZookeeperPods() []replay.PodRef {
	return []replay.PodRef{
		{ClusterID: "c1", Namespace: "devops", Name: "zookeeper-0", IP: "10.4.1.1",
			Labels: map[string]string{
				"app": "zookeeper", "statefulset.kubernetes.io/pod-name": "zookeeper-0",
			}},
		{ClusterID: "c1", Namespace: "devops", Name: "zookeeper-1", IP: "10.4.1.2",
			Labels: map[string]string{
				"app": "zookeeper", "statefulset.kubernetes.io/pod-name": "zookeeper-1",
			}},
		{ClusterID: "c1", Namespace: "devops", Name: "zookeeper-2", IP: "10.4.1.3",
			Labels: map[string]string{
				"app": "zookeeper", "statefulset.kubernetes.io/pod-name": "zookeeper-2",
			}},
	}
}

// zk0LBService 是只点名 zookeeper-0 的 LoadBalancer Service——devops/zk-0-lb，
// selector 里的 statefulset.kubernetes.io/pod-name 把范围收窄到单个 Pod，
// 而候选策略仍然是 workload（app: zookeeper）粒度的。
func zk0LBService() snapshot.Service {
	return snapshot.Service{
		ClusterID: "c1", Namespace: "devops", Name: "zk-0-lb", Type: "LoadBalancer",
		Selector: map[string]string{
			"app": "zookeeper", "statefulset.kubernetes.io/pod-name": "zookeeper-0",
		},
		Ports: []snapshot.ServicePort{
			{Name: "client", Port: 2181, TargetPort: 2181, Protocol: "TCP"},
		},
		LoadBalancerIngressIPs: []string{"34.150.1.177"},
	}
}

// oneIstioPod 是单副本的 istio-ingressgateway：selector 与 podSelector
// 选中的是同一批（唯一一个）Pod，挂靠无损。
func oneIstioPod() []replay.PodRef {
	return []replay.PodRef{
		{ClusterID: "c1", Namespace: "istio-system", Name: "istio-ingressgateway-1", IP: "10.4.2.1",
			Labels: map[string]string{"app": "istio-ingressgateway"}},
	}
}

// istioLBService 的 selector 只用 workload 归属键本身，没有额外收窄。
func istioLBService() snapshot.Service {
	return snapshot.Service{
		ClusterID: "c1", Namespace: "istio-system", Name: "istio-ingressgateway", Type: "LoadBalancer",
		Selector: map[string]string{"app": "istio-ingressgateway"},
		Ports: []snapshot.ServicePort{
			{Name: "https", Port: 443, TargetPort: 443, Protocol: "TCP"},
		},
		LoadBalancerIngressIPs: []string{"34.150.1.178"},
	}
}

// Service 的 selector 点名单个 Pod 时，workload 粒度的规则会放宽 ——
// 必须报出来。
//
// devops/zk-0-lb 的 selector 含 statefulset.kubernetes.io/pod-name: zookeeper-0，
// 它选中一个 Pod；而候选策略是 workload 粒度的，生成的规则覆盖全部三个。
// 不报出来，操作者读到的是「按 Service 放行」，实际是「按 workload 放行」。
func TestExposureWideningIsReportedWhenTheSelectorNamesOnePod(t *testing.T) {
	// zookeeper 三个 Pod，LB 只点名 zookeeper-0
	res := generateWith(t, threeZookeeperPods(), zk0LBService())

	if len(res.ExposureWidenings) != 1 {
		t.Fatalf("报了 %d 条放宽，want 1", len(res.ExposureWidenings))
	}
	w := res.ExposureWidenings[0]
	if w.SelectedPods != 1 || w.WorkloadPods != 3 || w.ExtraPods != 2 {
		t.Errorf("放宽 = 选中 %d / 共 %d / 多出 %d，want 1/3/2",
			w.SelectedPods, w.WorkloadPods, w.ExtraPods)
	}
	if w.Namespace != "devops" || w.Service != "zk-0-lb" || w.Workload != "zookeeper" {
		t.Errorf("身份字段 = %+v，want namespace=devops service=zk-0-lb workload=zookeeper", w)
	}
}

// 无损时也要报一条 ExtraPods=0，不能省略。
//
// 把无损的与真的放宽了的混在一起（省略即无损），操作者分不出哪几条值得
// 回到 Pod 粒度看——与 Widening.ExtraGrants 那条注释同一条理由。
func TestExposureWideningReportsZeroWhenLossless(t *testing.T) {
	res := generateWith(t, oneIstioPod(), istioLBService())
	if len(res.ExposureWidenings) != 1 {
		t.Fatalf("报了 %d 条放宽，want 1（无损也要报）", len(res.ExposureWidenings))
	}
	if got := res.ExposureWidenings[0].ExtraPods; got != 0 {
		t.Errorf("ExtraPods = %d, want 0", got)
	}
}

// ExposureWidenings 恒为空切片而不是 nil：与 UnattachedBaselines 同一条
// 纪律，序列化成 null 会被读成"这一栏没算过"。
func TestNoExposureWideningsIsAnEmptySliceNotNil(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	if res.ExposureWidenings == nil {
		t.Error("ExposureWidenings 是 nil —— 它会序列化成 null，而空清单要读作" +
			"「算过，没有一条挂靠」，不是「没算过」")
	}
}

// namespace 折叠不改变某个 Service 当初点没点名单个 Pod——这一栏必须原样
// 带过来。这条链路上曾经连续两轮把新加的字段漏在 AtNamespaceGranularity
// 的显式字面量之外，因为它只在这里、不在编译器能查出来的地方。
func TestExposureWideningSurvivesNamespaceFold(t *testing.T) {
	res := generateWith(t, threeZookeeperPods(), zk0LBService())
	if len(res.ExposureWidenings) != 1 {
		t.Fatalf("折叠前先要有一条放宽，报了 %d 条", len(res.ExposureWidenings))
	}

	folded, _ := res.AtNamespaceGranularity()
	if len(folded.ExposureWidenings) != 1 {
		t.Fatalf("折叠后 ExposureWidenings 丢了：报了 %d 条，want 1", len(folded.ExposureWidenings))
	}
	if folded.ExposureWidenings[0] != res.ExposureWidenings[0] {
		t.Errorf("折叠后内容变了：%+v，want %+v",
			folded.ExposureWidenings[0], res.ExposureWidenings[0])
	}
}

// mixedLabelKeyZookeeperPods 是 Helm 迁移中期的形态：三个 Pod 还在用旧的
// app 标签，一个 Pod 已经迁移到 app.kubernetes.io/name（resolveWinningKeys
// 判定的赢家键，优先级更高）。这不是构造出来的边界情况——
// resolveWinningKeys 的文档注释原话就是这个场景（滚动更新期间新旧
// ReplicaSet 各用一套标签）。
func mixedLabelKeyZookeeperPods() []replay.PodRef {
	return []replay.PodRef{
		{ClusterID: "c1", Namespace: "devops", Name: "zookeeper-0", IP: "10.4.3.1",
			Labels: map[string]string{"app": "zookeeper"}},
		{ClusterID: "c1", Namespace: "devops", Name: "zookeeper-1", IP: "10.4.3.2",
			Labels: map[string]string{"app": "zookeeper"}},
		{ClusterID: "c1", Namespace: "devops", Name: "zookeeper-2", IP: "10.4.3.3",
			Labels: map[string]string{"app": "zookeeper"}},
		{ClusterID: "c1", Namespace: "devops", Name: "zookeeper-3", IP: "10.4.3.4",
			Labels: map[string]string{"app.kubernetes.io/name": "zookeeper"}},
	}
}

// zkLegacyKeyLBService 是一个还没跟上迁移的 LoadBalancer：它的 selector
// 仍然用旧的 app 键，而 resolveWinningKeys 已经把这个 workload 的赢家键
// 判给了 app.kubernetes.io/name（因为至少一个 Pod 已经迁移、且优先级更高）。
// selector 键与 podSelector 实际会用的键因此不是同一个。
func zkLegacyKeyLBService() snapshot.Service {
	return snapshot.Service{
		ClusterID: "c1", Namespace: "devops", Name: "zk-legacy-lb", Type: "LoadBalancer",
		Selector: map[string]string{"app": "zookeeper"},
		Ports: []snapshot.ServicePort{
			{Name: "client", Port: 2181, TargetPort: 2181, Protocol: "TCP"},
		},
		LoadBalancerIngressIPs: []string{"34.150.1.179"},
	}
}

// TI1（design review 2026-08-28）：Service selector 的键与 podSelector 实际
// 会用的赢家键不是同一个键时，ExtraPods 不能算出负数。
//
// 早前的实现分别数了两遍——一遍按 Service selector 的字面键扫全体 Pod，
// 一遍按赢家键扫全体 Pod——再相减。这里 selector 命中的是三个还没迁移的
// 旧标签 Pod（SelectedPods=3），而赢家键只命中一个已迁移的 Pod
// （WorkloadPods=1），相减得到 -2：一个比不报这个字段更糟的读数，因为它
// 看起来是权威结论。
//
// 正确的问法是"podSelector 真正覆盖的那一批里，Service 没点到的有几个"——
// 这里 podSelector 只覆盖 zookeeper-3（唯一命中赢家键的 Pod），而它并不
// 满足 Service 那条还停留在旧键上的 selector，所以 SelectedPods=0、
// WorkloadPods=1、ExtraPods=1，不可能是负数。
func TestExposureWideningStaysNonNegativeUnderMixedLabelKeys(t *testing.T) {
	res := generateWith(t, mixedLabelKeyZookeeperPods(), zkLegacyKeyLBService())

	if len(res.ExposureWidenings) != 1 {
		t.Fatalf("报了 %d 条放宽，want 1", len(res.ExposureWidenings))
	}
	w := res.ExposureWidenings[0]
	if w.ExtraPods < 0 {
		t.Fatalf("ExtraPods = %d，负数是一个比不报这个字段更糟的读数", w.ExtraPods)
	}
	if w.SelectedPods != 0 || w.WorkloadPods != 1 || w.ExtraPods != 1 {
		t.Errorf("放宽 = 选中 %d / 共 %d / 多出 %d，want 0/1/1",
			w.SelectedPods, w.WorkloadPods, w.ExtraPods)
	}
}
