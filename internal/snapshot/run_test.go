package snapshot_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
)

func failures(n int) []snapshot.Failure {
	fs := make([]snapshot.Failure, 0, n)
	for i := 0; i < n; i++ {
		fs = append(fs, snapshot.Failure{Resource: snapshot.ResourcePod, Reason: snapshot.FailureOther})
	}
	return fs
}

// PARTIAL 必须与 OK 区分：一份缺了 NetworkPolicy 的快照报成 OK，会让
// "这个集群没有任何策略"与"我们没被授权看策略"在下游变得无法区分。
func TestDeriveRunStatus(t *testing.T) {
	all := []snapshot.ResourceKind{
		snapshot.ResourceNamespace,
		snapshot.ResourcePod,
		snapshot.ResourceNetworkPolicy,
	}

	cases := []struct {
		name      string
		attempted []snapshot.ResourceKind
		failures  []snapshot.Failure
		want      snapshot.RunStatus
	}{
		{"everything collected", all, nil, snapshot.RunOK},
		{"one resource failed", all, failures(1), snapshot.RunPartial},
		{"all but one failed", all, failures(2), snapshot.RunPartial},
		{"every resource failed", all, failures(3), snapshot.RunFailed},
		{"more failures than attempts", all, failures(4), snapshot.RunFailed},
		{"a single resource, collected", all[:1], nil, snapshot.RunOK},
		{"a single resource, failed", all[:1], failures(1), snapshot.RunFailed},

		// 一次什么都没尝试的运行不是成功。这条缺陷最常见的成因是
		// 循环体从没执行过 —— 报 OK 会把它藏起来。
		{"nothing attempted", nil, nil, snapshot.RunFailed},
		{"nothing attempted, empty slice", []snapshot.ResourceKind{}, nil, snapshot.RunFailed},
		{"nothing attempted but failures recorded", nil, failures(1), snapshot.RunFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := snapshot.DeriveRunStatus(c.attempted, c.failures); got != c.want {
				t.Errorf("DeriveRunStatus(%d attempted, %d failures) = %q, want %q",
					len(c.attempted), len(c.failures), got, c.want)
			}
		})
	}
}

// 每一类计数必须来自它自己的那个切片。长度两两不同，一次复制粘贴
// 把 Services 写成 Endpoints 就会被这条用例抓住。
func TestObservationCountsEachResourceFromItsOwnSlice(t *testing.T) {
	o := snapshot.Observation{
		ClusterID:  "cluster-a",
		Namespaces: make([]snapshot.Namespace, 1),
		Pods:       make([]snapshot.Pod, 2),
		Nodes:      make([]snapshot.Node, 3),
		Services:   make([]snapshot.Service, 4),
		Endpoints:  make([]snapshot.Endpoints, 5),
		Policies:   make([]snapshot.NetworkPolicy, 6),
		Gateways:   make([]snapshot.Gateway, 7),
	}

	want := map[snapshot.ResourceKind]int{
		snapshot.ResourceNamespace:     1,
		snapshot.ResourcePod:           2,
		snapshot.ResourceNode:          3,
		snapshot.ResourceService:       4,
		snapshot.ResourceEndpointSlice: 5,
		snapshot.ResourceNetworkPolicy: 6,
		snapshot.ResourceIngress:       7,
	}
	got := o.Counts()
	for kind, n := range want {
		if got[kind] != n {
			t.Errorf("Counts()[%s] = %d, want %d", kind, got[kind], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Counts() has %d entries (%v), want exactly %d", len(got), got, len(want))
	}

	// ReplicaSet 只为解 ownerRef 链而采，不是一类资产，不进计数。
	if _, ok := got[snapshot.ResourceReplicaSet]; ok {
		t.Error("Counts() reports REPLICASET, want it excluded: it is not a collected asset")
	}
}

// 空观测的每一类都必须出现且为 0，而不是缺键。落库时一类资源一行，
// 缺键与 0 的区别是"这次没采到"与"采到了零条"。
func TestObservationCountsReportsZeroForEveryKind(t *testing.T) {
	got := snapshot.Observation{}.Counts()
	if len(got) == 0 {
		t.Fatal("Counts() on an empty observation is empty, want one entry per collected kind")
	}
	for kind, n := range got {
		if n != 0 {
			t.Errorf("Counts()[%s] = %d on an empty observation, want 0", kind, n)
		}
	}
}
