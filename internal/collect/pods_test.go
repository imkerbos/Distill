package collect

import (
	"context"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// collectorWith 构造一个只带登记表的采集器。classifyPodIP 与 toPod
// 都不碰 client，这里不需要一个集群。
func collectorWith(registry *cluster.Registry) *Collector {
	return New(testClusterID, nil, registry, nil)
}

func kinds(ws []snapshot.Warning) []snapshot.WarningKind {
	out := make([]snapshot.WarningKind, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Kind)
	}
	return out
}

func onlyWarning(t *testing.T, ws []snapshot.Warning) snapshot.Warning {
	t.Helper()
	if len(ws) != 1 {
		t.Fatalf("warnings = %+v, want exactly one", ws)
	}
	return ws[0]
}

func TestClassifyPodIPStaysSilentForAPodWithoutAnIP(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	got, warnings := c.classifyPodIP("", "default/pending")
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none; Phase already explains an unassigned IP", warnings)
	}
	if got.Scope != "" {
		t.Errorf("Scope = %q, want empty for a pod with no IP", got.Scope)
	}
}

func TestClassifyPodIPWarnsOnAnUnparsableAddress(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	got, warnings := c.classifyPodIP("10.0.1.999", "default/web")
	w := onlyWarning(t, warnings)
	if w.Kind != snapshot.WarningPodIPUnparsable {
		t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPUnparsable)
	}
	if w.Subject != "default/web" || w.Detail != "10.0.1.999" {
		t.Errorf("warning = %+v, want the pod and the offending address", w)
	}
	if got.Scope != "" {
		t.Errorf("Scope = %q, want empty; an unparsable address must not be given a scope", got.Scope)
	}
}

// 分类器的合同是"宁可不答"。采集器必须把这个拒绝原样带上去，
// 而不是替它挑一个默认值 —— 一个被替换成 EXTERNAL 的 UNKNOWN
// 会作为事实进入下游，且再也无法与真正的 EXTERNAL 区分。
func TestClassifyPodIPPropagatesUnknownInsteadOfSubstitutingADefault(t *testing.T) {
	// 只登记了 Pod 网段，没有 Service 网段：这正是 §2.3 描述的现状。
	incomplete := cluster.NewRegistry([]cluster.Cluster{{
		ID:       testClusterID,
		PodCIDRs: []netip.Prefix{mustPrefix(t, "10.0.0.0/16")},
		NodeCIDRs: []netip.Prefix{
			mustPrefix(t, "192.168.0.0/24"),
		},
	}})
	c := collectorWith(incomplete)

	got, warnings := c.classifyPodIP("172.20.0.7", "default/web")
	if got.Scope != cluster.ScopeUnknown {
		t.Fatalf("Scope = %q, want %q", got.Scope, cluster.ScopeUnknown)
	}
	if got.Reason != cluster.ReasonServiceCIDRUnregistered {
		t.Errorf("Reason = %q, want %q", got.Reason, cluster.ReasonServiceCIDRUnregistered)
	}
	w := onlyWarning(t, warnings)
	if w.Kind != snapshot.WarningPodIPUnclassifiable {
		t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPUnclassifiable)
	}
}

func TestClassifyPodIPPropagatesAmbiguousWithoutPickingACluster(t *testing.T) {
	overlapping := cluster.NewRegistry([]cluster.Cluster{
		{
			ID:           testClusterID,
			PodCIDRs:     []netip.Prefix{mustPrefix(t, "10.0.0.0/16")},
			ServiceCIDRs: []netip.Prefix{mustPrefix(t, "10.96.0.0/12")},
			NodeCIDRs:    []netip.Prefix{mustPrefix(t, "192.168.0.0/24")},
		},
		{
			ID:           "staging",
			PodCIDRs:     []netip.Prefix{mustPrefix(t, "10.0.0.0/8")},
			ServiceCIDRs: []netip.Prefix{mustPrefix(t, "172.20.0.0/16")},
			NodeCIDRs:    []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
		},
	})
	c := collectorWith(overlapping)

	got, warnings := c.classifyPodIP("10.0.1.5", "default/web")
	if got.Scope != cluster.ScopeAmbiguous {
		t.Fatalf("Scope = %q, want %q", got.Scope, cluster.ScopeAmbiguous)
	}
	if got.ClusterID != "" {
		t.Errorf("ClusterID = %q, want empty; an ambiguous address must not be attributed", got.ClusterID)
	}
	if len(got.Matches) != 2 {
		t.Errorf("Matches = %v, want both colliding clusters", got.Matches)
	}
	if w := onlyWarning(t, warnings); w.Kind != snapshot.WarningPodIPAmbiguous {
		t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPAmbiguous)
	}
}

func TestClassifyPodIPWarnsWhenTheAddressBelongsToAnotherCluster(t *testing.T) {
	fleet := cluster.NewRegistry([]cluster.Cluster{
		{
			ID:           testClusterID,
			PodCIDRs:     []netip.Prefix{mustPrefix(t, "10.0.0.0/16")},
			ServiceCIDRs: []netip.Prefix{mustPrefix(t, "10.96.0.0/12")},
			NodeCIDRs:    []netip.Prefix{mustPrefix(t, "192.168.0.0/24")},
		},
		{
			ID:           "staging",
			PodCIDRs:     []netip.Prefix{mustPrefix(t, "10.1.0.0/16")},
			ServiceCIDRs: []netip.Prefix{mustPrefix(t, "172.20.0.0/16")},
			NodeCIDRs:    []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
		},
	})
	c := collectorWith(fleet)

	got, warnings := c.classifyPodIP("10.1.2.3", "default/web")
	if got.Scope != cluster.ScopePod || got.ClusterID != "staging" {
		t.Fatalf("classification = %+v, want POD in staging", got)
	}
	if w := onlyWarning(t, warnings); w.Kind != snapshot.WarningPodIPOutsideCluster {
		t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPOutsideCluster)
	}
}

func TestClassifyPodIPIsSilentForAnAddressInItsOwnPodCIDR(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	got, warnings := c.classifyPodIP("10.0.1.5", "default/web")
	if got.Scope != cluster.ScopePod || got.ClusterID != testClusterID {
		t.Fatalf("classification = %+v, want POD in %s", got, testClusterID)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
}

// SERVICE / NODE / EXTERNAL 都不是一个 Pod IP 该有的归属。
// 三者都必须发声：静默会让一份错误的网段登记一直活到求值阶段。
func TestClassifyPodIPWarnsOnAScopeNoPodShouldHave(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	cases := []struct {
		name string
		ip   string
		want cluster.Scope
	}{
		{"service cidr", "10.96.0.10", cluster.ScopeService},
		{"node cidr", "192.168.0.10", cluster.ScopeNode},
		{"outside the fleet", "8.8.8.8", cluster.ScopeExternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := c.classifyPodIP(tc.ip, "default/web")
			if got.Scope != tc.want {
				t.Fatalf("Scope = %q, want %q", got.Scope, tc.want)
			}
			if w := onlyWarning(t, warnings); w.Kind != snapshot.WarningPodIPOutsideCluster {
				t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPOutsideCluster)
			}
		})
	}
}

func TestToPodRecordsTheClassifiedScopeOnTheSnapshotRow(t *testing.T) {
	incomplete := cluster.NewRegistry([]cluster.Cluster{{
		ID:       testClusterID,
		PodCIDRs: []netip.Prefix{mustPrefix(t, "10.0.0.0/16")},
	}})
	c := collectorWith(incomplete)

	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "172.20.0.7"},
	}
	got, _ := c.toPod(p, nil)
	if got.IPScope != cluster.ScopeUnknown {
		t.Errorf("IPScope = %q, want %q on the snapshot row", got.IPScope, cluster.ScopeUnknown)
	}
	if got.IPScopeReason != cluster.ReasonServiceCIDRUnregistered {
		t.Errorf("IPScopeReason = %q, want %q", got.IPScopeReason, cluster.ReasonServiceCIDRUnregistered)
	}
}

func TestToPodCopiesEveryFieldTheEvaluationLayerNeeds(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-0",
			Namespace: "data",
			UID:       "uid-1",
			Labels:    map[string]string{"app": "db"},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "db", Controller: boolPtr(true)},
			},
		},
		Spec: corev1.PodSpec{
			HostNetwork:        true,
			NodeName:           "node-a",
			ServiceAccountName: "db-sa",
			Containers:         []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.5"},
	}

	got, warnings := c.toPod(p, nil)
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}

	fields := []struct {
		name      string
		got, want any
	}{
		{"ClusterID", got.ClusterID, testClusterID},
		{"Namespace", got.Namespace, "data"},
		{"Name", got.Name, "db-0"},
		{"UID", got.UID, "uid-1"},
		{"Phase", got.Phase, "Running"},
		{"IP", got.IP, "10.0.1.5"},
		{"IPScope", got.IPScope, cluster.ScopePod},
		{"HostNetwork", got.HostNetwork, true},
		{"NodeName", got.NodeName, "node-a"},
		{"ServiceAccount", got.ServiceAccount, "db-sa"},
		{"OwnerKind", got.OwnerKind, "StatefulSet"},
		{"OwnerName", got.OwnerName, "db"},
		{"WorkloadKind", got.WorkloadKind, "StatefulSet"},
		{"WorkloadName", got.WorkloadName, "db"},
		{"InMesh", got.InMesh, false},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("toPod().%s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if got.Labels["app"] != "db" {
		t.Errorf("Labels = %v, want app=db", got.Labels)
	}
}

// sidecar 只留在 initContainers 里的注入形态必须一样被认出来：
// 漏掉它会让一个 L4 身份已经失真的 Pod 不被标记降级。
func TestToPodDetectsASidecarAmongInitContainers(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "istio-proxy"}},
			Containers:     []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.5"},
	}
	got, _ := c.toPod(p, nil)
	if !got.InMesh {
		t.Fatal("InMesh = false, want true for a pod whose only sidecar trace is an init container")
	}
	if got.MeshSource != cluster.MeshSourceIstioSidecar {
		t.Errorf("MeshSource = %q, want %q", got.MeshSource, cluster.MeshSourceIstioSidecar)
	}
}

// ownerRef 链断掉是一处平台知道得比表面上少的地方，必须发声：
// 静默会让这个 Pod 以 ReplicaSet 名义参与对账，而 ReplicaSet
// 每次发布都换名字，于是同一个服务在发布前后变成两个主体。
func TestToPodWarnsWhenTheOwnerChainCannotBeResolved(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-5f7-abcde", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "web-5f7", Controller: boolPtr(true)},
			},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.5"},
	}

	got, warnings := c.toPod(p, map[string]ownerRef{})
	w := onlyWarning(t, warnings)
	if w.Kind != snapshot.WarningWorkloadUnresolved {
		t.Fatalf("warning kind = %q, want %q", w.Kind, snapshot.WarningWorkloadUnresolved)
	}
	if w.Subject != "default/web-5f7-abcde" {
		t.Errorf("warning subject = %q, want the pod", w.Subject)
	}
	// 落回 ReplicaSet 本身，而不是留空让上层以为这是一个裸 Pod。
	if got.WorkloadKind != "ReplicaSet" || got.WorkloadName != "web-5f7" {
		t.Errorf("workload = (%q, %q), want the unresolved ReplicaSet", got.WorkloadKind, got.WorkloadName)
	}
}

func TestResolveWorkload(t *testing.T) {
	rsOwners := map[string]ownerRef{
		"default/web-5f7":   {Kind: "Deployment", Name: "web"},
		"default/orphan-rs": {},
	}
	cases := []struct {
		name         string
		owner        ownerRef
		want         ownerRef
		wantResolved bool
	}{
		{"bare pod", ownerRef{}, ownerRef{}, true},
		{"statefulset owns the pod directly", ownerRef{Kind: "StatefulSet", Name: "db"},
			ownerRef{Kind: "StatefulSet", Name: "db"}, true},
		{"daemonset owns the pod directly", ownerRef{Kind: "DaemonSet", Name: "agent"},
			ownerRef{Kind: "DaemonSet", Name: "agent"}, true},
		{"job is the top the round can give", ownerRef{Kind: "Job", Name: "backup"},
			ownerRef{Kind: "Job", Name: "backup"}, true},
		{"replicaset hops to its deployment", ownerRef{Kind: "ReplicaSet", Name: "web-5f7"},
			ownerRef{Kind: "Deployment", Name: "web"}, true},
		{"replicaset missing from the index", ownerRef{Kind: "ReplicaSet", Name: "gone"},
			ownerRef{Kind: "ReplicaSet", Name: "gone"}, false},
		{"replicaset with no controller of its own", ownerRef{Kind: "ReplicaSet", Name: "orphan-rs"},
			ownerRef{Kind: "ReplicaSet", Name: "orphan-rs"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, resolved := resolveWorkload("default", tc.owner, rsOwners)
			if got != tc.want || resolved != tc.wantResolved {
				t.Errorf("resolveWorkload = (%+v, %v), want (%+v, %v)", got, resolved, tc.want, tc.wantResolved)
			}
		})
	}
}

func TestCollectPodsAttachesWarningsToTheObservation(t *testing.T) {
	c, _ := newTestCollector(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "web-5f7-abcde", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "ReplicaSet", Name: "web-5f7", Controller: boolPtr(true)},
				},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "203.0.113.9"},
		},
	)

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}

	got := kinds(run.Observation.Warnings)
	wantEach := []snapshot.WarningKind{
		snapshot.WarningWorkloadUnresolved,
		snapshot.WarningPodIPOutsideCluster,
	}
	for _, want := range wantEach {
		found := false
		for _, k := range got {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("warning %q missing from the observation; got %v", want, got)
		}
	}
}
