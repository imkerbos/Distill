package collect

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// collectorWith 构造一个只带登记表的采集器。classifyPodIP 与 toPod
// 都不碰 client，这里不需要一个集群。
// podClassifier 是分类用例的薄壳。
//
// 分类已经从 Collector 上搬走（design doc 2026-08-18 §3.4）：它要看全 fleet
// 的网段，而推送式接入下 agent 只看得见自己那个集群。下面这些用例验的仍然
// 是同一段判定逻辑，因此保持调用形状不变 —— 断言里挂着实测结论（例如
// hostNetwork 那条：不区分的话 kind 上 28 个 Pod 会报 12 条误报），改写它们
// 等于把那次实测作废。
type podClassifier struct {
	*Collector
	registry *cluster.Registry
}

func (c podClassifier) classifyPodIP(
	ip, subject string, hostNetwork bool,
) (cluster.Classification, []snapshot.Warning) {
	return classifyPodIP(c.registry, testClusterID, ip, subject, hostNetwork)
}

func collectorWith(registry *cluster.Registry) podClassifier {
	return podClassifier{Collector: New(testClusterID, nil, nil), registry: registry}
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

	got, warnings := c.classifyPodIP("", "default/pending", false)
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none; Phase already explains an unassigned IP", warnings)
	}
	if got.Scope != "" {
		t.Errorf("Scope = %q, want empty for a pod with no IP", got.Scope)
	}
}

func TestClassifyPodIPWarnsOnAnUnparsableAddress(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	got, warnings := c.classifyPodIP("10.0.1.999", "default/web", false)
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

	got, warnings := c.classifyPodIP("172.20.0.7", "default/web", false)
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

	got, warnings := c.classifyPodIP("10.0.1.5", "default/web", false)
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

	got, warnings := c.classifyPodIP("10.1.2.3", "default/web", false)
	if got.Scope != cluster.ScopePod || got.ClusterID != "staging" {
		t.Fatalf("classification = %+v, want POD in staging", got)
	}
	if w := onlyWarning(t, warnings); w.Kind != snapshot.WarningPodIPOutsideCluster {
		t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPOutsideCluster)
	}
}

func TestClassifyPodIPIsSilentForAnAddressInItsOwnPodCIDR(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	got, warnings := c.classifyPodIP("10.0.1.5", "default/web", false)
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
			got, warnings := c.classifyPodIP(tc.ip, "default/web", false)
			if got.Scope != tc.want {
				t.Fatalf("Scope = %q, want %q", got.Scope, tc.want)
			}
			if w := onlyWarning(t, warnings); w.Kind != snapshot.WarningPodIPOutsideCluster {
				t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPOutsideCluster)
			}
		})
	}
}

// hostNetwork Pod 的 IP 就是它所在节点的 IP，判成 NODE 是**正确答案**。
//
// 这条来自 2026-08-17 第一次对真实 kind 集群的采集：28 个 Pod 里有 12 个
// 报了 POD_IP_OUTSIDE_CLUSTER，全部是 hostNetwork Pod（cilium、etcd、
// kube-apiserver、kube-proxy、kube-scheduler…）。它们的登记一点问题都没有。
//
// 43% 的误报率不是"多几条噪音"：这条告警存在的全部理由是让一份填错的网段
// 登记在采集当时就被发现，而一条每次都在喊的告警会被整体忽略，
// 于是真正填错的那一次也一起被忽略了（同 servicesWithoutEndpoints 的取舍）。
func TestHostNetworkPodOnItsNodeIPRaisesNoWarning(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	got, warnings := c.classifyPodIP("192.168.0.10", "kube-system/kube-proxy", true)
	if got.Scope != cluster.ScopeNode {
		t.Fatalf("Scope = %q, want %q", got.Scope, cluster.ScopeNode)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none: a hostNetwork pod carries its node's address, "+
			"so NODE scope is the expected answer, not a registration error", warnings)
	}
}

// 但 hostNetwork Pod 的地址落在别处仍然要发声。
//
// 与上一条互为反面。只加一条"hostNetwork 就闭嘴"的分支，会把这类 Pod
// 整个移出这条守卫的覆盖范围 —— 而节点网段填错同样会让每一条涉及它的
// 流量被还原成错误的主体。
func TestHostNetworkPodOutsideTheNodeCIDRIsStillWarned(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	cases := []struct {
		name string
		ip   string
	}{
		{"in the pod cidr", "10.0.1.5"},
		{"outside the fleet", "8.8.8.8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, warnings := c.classifyPodIP(tc.ip, "kube-system/agent", true)
			if w := onlyWarning(t, warnings); w.Kind != snapshot.WarningPodIPOutsideCluster {
				t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPOutsideCluster)
			}
		})
	}
}

// 普通 Pod 落在节点网段仍然是错的 —— hostNetwork 那条豁免不得外溢。
func TestOrdinaryPodOnANodeIPIsStillWarned(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	_, warnings := c.classifyPodIP("192.168.0.10", "default/web", false)
	if w := onlyWarning(t, warnings); w.Kind != snapshot.WarningPodIPOutsideCluster {
		t.Errorf("warning kind = %q, want %q", w.Kind, snapshot.WarningPodIPOutsideCluster)
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
	// 归属落到快照行上这件事，现在由 Classify 负责（design doc §3.4）：
	// toPod 只搬运观测。断言的内容不变 —— 一份缺 Service 网段的登记，
	// 判不出这个地址属于哪一类，必须留 UNKNOWN 并说出是哪一处登记缺失，
	// 而不是猜一个。
	row, _ := c.toPod(p, nil)
	out := Classify(runWithPods(row), incomplete)
	got := out.Observation.Pods[0]
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
		// hostNetwork Pod 配一个节点网段的地址：这个 fixture 必须自洽。
		// 一个 hostNetwork Pod 带着 Pod 网段的 IP 在现实里不存在，
		// 而用它当"正常情形"会让这条用例顺带断言掉一条本该报出的告警。
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "192.168.0.10"},
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
		{"IP", got.IP, "192.168.0.10"},
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

	// 归属不再由 toPod 填（design doc §3.4），但它仍然必须落到这一行上 ——
	// 求值层读的就是这个字段。断言挪到 Classify 之后，内容不变：hostNetwork
	// Pod 用的就是它所在节点的地址，判成 NODE 是正确答案。
	classified := Classify(runWithPods(got), fleetRegistry(t)).Observation.Pods[0]
	if classified.IPScope != cluster.ScopeNode {
		t.Errorf("Classify().IPScope = %v, want %v", classified.IPScope, cluster.ScopeNode)
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
	// 归属判定已经从 Collect 里搬走（design doc 2026-08-18 §3.4）：它要看
	// 全 fleet 的网段，而推送式接入下采集器看不见别的集群。真实调用方
	// （PULL 的采集器、PUSH 的平台）都在采完之后调 Classify，这里照做 ——
	// 断言要验的仍然是「两类告警都挂在这次运行上」。
	run = Classify(run, fleetRegistry(t))

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

// Pod 上的 metrics 抓取声明要采下来，但**只采白名单里那三个键**。
//
// 整批采集 annotations 是不行的：kubectl.kubernetes.io/last-applied-configuration
// 里是整份 manifest —— 体积上是 labels 的几十倍，内容上可能带着 env 里的口令与
// 内网地址。而这个库会被导出到事实层长期留存（design doc 2026-08-18 §5）。
func TestToPodCollectsOnlyTheWhitelistedScrapeAnnotations(t *testing.T) {
	c := collectorWith(fleetRegistry(t))

	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-1", Namespace: "shop",
			Annotations: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/port":   "9102",
				"prometheus.io/path":   "/metrics",
				// 下面这些一个字节都不该进来。
				"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"containers":[{"env":[{"name":"DB_PASSWORD","value":"hunter2"}]}]}}`,
				"internal.company/oncall-phone":                    "13800000000",
			},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.5"},
	}

	got, _ := c.toPod(p, nil)

	want := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "9102",
		"prometheus.io/path":   "/metrics",
	}
	if !reflect.DeepEqual(got.ScrapeAnnotations, want) {
		t.Errorf("ScrapeAnnotations = %v, want %v", got.ScrapeAnnotations, want)
	}
	for _, leak := range []string{"hunter2", "DB_PASSWORD", "13800000000", "oncall"} {
		if strings.Contains(fmt.Sprint(got.ScrapeAnnotations), leak) {
			t.Errorf("ScrapeAnnotations leaked %q: %v", leak, got.ScrapeAnnotations)
		}
	}
}

func TestToPodLeavesScrapeAnnotationsEmptyWhenThePodDeclaresNothing(t *testing.T) {
	// 空 map 与 nil 都可以，但**不得凭空补一个默认端口**：一条放行到猜出来
	// 的端口的规则，看起来齐备、实际什么都没放行。
	c := collectorWith(fleetRegistry(t))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "quiet", Namespace: "shop"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.6"},
	}
	got, _ := c.toPod(p, nil)
	if len(got.ScrapeAnnotations) != 0 {
		t.Errorf("ScrapeAnnotations = %v, want empty", got.ScrapeAnnotations)
	}
}
