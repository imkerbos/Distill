package collect

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// **双栈 Pod 的第二个地址必须被采下来。**
//
// status.podIP 只是 status.podIPs 的第一项。漏掉第二个的后果是走它的连接
// 解不出主体、判 UNKNOWN，覆盖它的规则于是缺席 —— 下发 default-deny 之后
// 那条连接会被拦断。
func TestExtraPodIPsPicksUpTheSecondFamily(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{
		PodIP: "10.4.1.7",
		PodIPs: []corev1.PodIP{
			{IP: "10.4.1.7"}, {IP: "fd00:10:4::7"},
		},
	}}
	got := extraPodIPs(p)
	if len(got) != 1 {
		t.Fatalf("取出 %d 个额外地址, want 1：%+v", len(got), got)
	}
	if got[0].IP != "fd00:10:4::7" {
		t.Errorf("额外地址 = %q, want fd00:10:4::7", got[0].IP)
	}
}

// 单栈 Pod 没有额外地址：绝大多数集群走这条路，形状必须完全不变。
func TestSingleStackPodHasNoExtraIPs(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{
		PodIP: "10.4.1.7", PodIPs: []corev1.PodIP{{IP: "10.4.1.7"}},
	}}
	if got := extraPodIPs(p); len(got) != 0 {
		t.Errorf("单栈 Pod 取出了额外地址：%+v", got)
	}
}

// **主地址不重复记一遍。**
//
// 按值比对而不是按下标跳过第一项：Kubernetes 保证两者一致，但一份不一致的
// status 不该让主地址被当成"额外地址"。重复本身无害（区间按地址建），
// 但快照里会出现一个与 IP 列相同的 ExtraIP，读起来像数据坏了。
func TestExtraPodIPsSkipsThePrimaryWhereverItAppears(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{
		PodIP: "fd00:10:4::7",
		// 主地址排在第二位：这份 status 与 Kubernetes 的约定不一致，
		// 但按值比对仍然认得出它。
		PodIPs: []corev1.PodIP{{IP: "10.4.1.7"}, {IP: "fd00:10:4::7"}},
	}}
	got := extraPodIPs(p)
	if len(got) != 1 || got[0].IP != "10.4.1.7" {
		t.Errorf("额外地址 = %+v, want 只有 10.4.1.7", got)
	}
}

// 空地址跳过：Pending 的 Pod 可能带一个空项。
func TestExtraPodIPsSkipsEmpty(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{
		PodIP:  "10.4.1.7",
		PodIPs: []corev1.PodIP{{IP: "10.4.1.7"}, {IP: ""}},
	}}
	if got := extraPodIPs(p); len(got) != 0 {
		t.Errorf("空地址被当成了一个额外地址：%+v", got)
	}
}
