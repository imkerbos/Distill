package cluster_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
)

// pod 造一条最小的 kube-system Pod 记录。
func cniPod(ns string, labels map[string]string) cluster.CNIPod {
	return cluster.CNIPod{Namespace: ns, Labels: labels}
}

// **从已采集的 Pod 快照认出 CNI，不新增任何权限、不新增出站。**
//
// 这是一个事实，不是判断：平台报告"这个集群跑着 Cilium"，而不报告
// "Cilium 执不执行 ANP"—— 后者会变成一张随版本过时的表，而过时的那天
// 没有任何东西会报错。
func TestDetectCNIRecognisesCiliumAndCalico(t *testing.T) {
	for _, tc := range []struct {
		name string
		pods []cluster.CNIPod
		want cluster.CNI
	}{
		{"cilium", []cluster.CNIPod{
			cniPod("kube-system", map[string]string{"k8s-app": "cilium"}),
		}, cluster.CNICilium},
		{"calico", []cluster.CNIPod{
			cniPod("kube-system", map[string]string{"k8s-app": "calico-node"}),
		}, cluster.CNICalico},
		{"cilium 用 app.kubernetes.io/name", []cluster.CNIPod{
			cniPod("kube-system", map[string]string{"app.kubernetes.io/name": "cilium-agent"}),
		}, cluster.CNICilium},
	} {
		if got := cluster.DetectCNI(tc.pods); got != tc.want {
			t.Errorf("%s: DetectCNI() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// **认不出就是认不出，不猜。**
//
// 一个猜出来的 CNI 会让下游据此做判断（比如"这个 CNI 不执行 ANP，所以
// 那些对象是死的，不必降级"），而猜错的方向是把一个真的在执行的平面
// 当成死的 —— 那正是这条链路上最危险的错误。
func TestDetectCNIAnswersUnknownRatherThanGuessing(t *testing.T) {
	for _, tc := range []struct {
		name string
		pods []cluster.CNIPod
	}{
		{"没有任何 Pod", nil},
		{"只有业务 Pod", []cluster.CNIPod{
			cniPod("payment", map[string]string{"app": "api"}),
		}},
		{"名字里带 cilium 但不在 kube-system", []cluster.CNIPod{
			cniPod("payment", map[string]string{"k8s-app": "cilium"}),
		}},
		{"认不出的 CNI", []cluster.CNIPod{
			cniPod("kube-system", map[string]string{"k8s-app": "some-future-cni"}),
		}},
	} {
		if got := cluster.DetectCNI(tc.pods); got != cluster.CNIUnknown {
			t.Errorf("%s: DetectCNI() = %q, want UNKNOWN —— 猜出来的 CNI 会让下游"+
				"据此判断，而猜错的方向是把在执行的平面当成死的", tc.name, got)
		}
	}
}

// **认出多个就是 UNKNOWN，不挑一个。**
//
// 一个同时装着两套 CNI 的集群（迁移中、或装错了）本身就是要被修的东西。
// 挑一个作答，等于替运维隐瞒了它。
func TestDetectCNIRefusesWhenSeveralArePresent(t *testing.T) {
	got := cluster.DetectCNI([]cluster.CNIPod{
		cniPod("kube-system", map[string]string{"k8s-app": "cilium"}),
		cniPod("kube-system", map[string]string{"k8s-app": "calico-node"}),
	})
	if got != cluster.CNIUnknown {
		t.Errorf("DetectCNI() = %q，两套 CNI 并存时挑了一个作答", got)
	}
}
