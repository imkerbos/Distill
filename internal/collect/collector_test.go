package collect

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/snapshot"
)

const testClusterID = "prod"

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("ParsePrefix(%q) = %v", s, err)
	}
	return p
}

// fleetRegistry 是一份完整登记：三类网段都有，因此 Classify 不会因为
// 登记缺口而整体退化成 UNKNOWN。想验证退化路径的测试自己另建登记。
func fleetRegistry(t *testing.T) *cluster.Registry {
	t.Helper()
	return cluster.NewRegistry([]cluster.Cluster{{
		ID:           testClusterID,
		PodCIDRs:     []netip.Prefix{mustPrefix(t, "10.0.0.0/16")},
		ServiceCIDRs: []netip.Prefix{mustPrefix(t, "10.96.0.0/12")},
		NodeCIDRs:    []netip.Prefix{mustPrefix(t, "192.168.0.0/24")},
	}})
}

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }

// healthyObjects 是一个所有资源都齐备的小集群：每类资源至少一个对象，
// 且 Service 有就绪后端、Pod IP 落在登记的 Pod 网段内，
// 因此一次干净的采集不该产生任何告警。
func healthyObjects() []runtime.Object {
	return []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-5f7",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "Deployment", Name: "web", Controller: boolPtr(true)},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-5f7-abcde",
				Namespace: "default",
				UID:       "pod-uid",
				Labels:    map[string]string{"app": "web"},
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "ReplicaSet", Name: "web-5f7", Controller: boolPtr(true)},
				},
			},
			Spec: corev1.PodSpec{
				NodeName:           "node-a",
				ServiceAccountName: "web-sa",
				Containers:         []corev1.Container{{Name: "app"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.5"},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.0.1.0/24"}},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.0.10"},
			}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Type:      corev1.ServiceTypeClusterIP,
				ClusterIP: "10.96.0.10",
				Selector:  map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{{
					Name:       "http",
					Port:       80,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromString("http"),
				}},
			},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-xyz",
				Namespace: "default",
				Labels:    map[string]string{serviceNameLabel: "web"},
			},
			Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.1.5"}}},
			Ports:     []discoveryv1.EndpointPort{{Port: int32Ptr(8080)}},
		},
		&netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "deny-all", Namespace: "default", UID: "np-uid"},
		},
		&netv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: netv1.IngressSpec{
				DefaultBackend: &netv1.IngressBackend{
					Service: &netv1.IngressServiceBackend{Name: "web"},
				},
			},
		},
	}
}

// stubClock 返回一串递增时刻，用来分辨 StartedAt / ObservedAt / FinishedAt
// 到底是不是同一次取值。
func stubClock(start time.Time) func() time.Time {
	var n int
	return func() time.Time {
		t := start.Add(time.Duration(n) * time.Minute)
		n++
		return t
	}
}

func newTestCollector(t *testing.T, objs ...runtime.Object) (*Collector, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	return New(testClusterID, cs, fleetRegistry(t), stubClock(time.Unix(1700000000, 0).UTC())), cs
}

// failList 让某类资源的 List 直接失败。resource 用 "*" 表示所有类型。
func failList(cs *fake.Clientset, resource string, err error) {
	cs.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

func forbidden(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "",
		errors.New("no read permission"))
}

func failureFor(t *testing.T, run snapshot.Run, kind snapshot.ResourceKind) snapshot.Failure {
	t.Helper()
	for _, f := range run.Failures {
		if f.Resource == kind {
			return f
		}
	}
	t.Fatalf("no failure recorded for %s; failures = %+v", kind, run.Failures)
	return snapshot.Failure{}
}

func TestCollectRejectsAnEmptyRunID(t *testing.T) {
	c, _ := newTestCollector(t, healthyObjects()...)

	run, err := c.Collect(context.Background(), "")
	if err == nil {
		t.Fatalf("Collect with empty run id = %+v, nil; want an error", run)
	}
	if run.Status != "" {
		t.Errorf("Collect returned Status %q alongside the error; want the zero Run", run.Status)
	}
}

func TestCollectReportsOKOnAHealthyCluster(t *testing.T) {
	c, _ := newTestCollector(t, healthyObjects()...)

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if run.Status != snapshot.RunOK {
		t.Fatalf("Status = %q, want %q (failures: %+v)", run.Status, snapshot.RunOK, run.Failures)
	}
	if len(run.Failures) != 0 {
		t.Errorf("Failures = %+v, want none", run.Failures)
	}

	want := map[snapshot.ResourceKind]int{
		snapshot.ResourceNamespace:     1,
		snapshot.ResourcePod:           1,
		snapshot.ResourceNode:          1,
		snapshot.ResourceService:       1,
		snapshot.ResourceEndpointSlice: 1,
		snapshot.ResourceNetworkPolicy: 1,
		snapshot.ResourceIngress:       1,
	}
	for kind, n := range want {
		if got := run.Observation.Counts()[kind]; got != n {
			t.Errorf("Counts()[%s] = %d, want %d", kind, got, n)
		}
	}
	if len(run.Observation.Warnings) != 0 {
		t.Errorf("Warnings = %+v, want none on a healthy cluster", run.Observation.Warnings)
	}
}

// 一次丢了 Pod 的采集绝不能报 OK：下游会把一份少了工作负载的快照
// 当成完整事实，于是"这个集群没有这些 Pod"与"我们没被授权看 Pod"
// 变得无法区分。
func TestCollectIsPartialWhenOneResourceKindFails(t *testing.T) {
	c, cs := newTestCollector(t, healthyObjects()...)
	failList(cs, "pods", forbidden("pods"))

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if run.Status != snapshot.RunPartial {
		t.Fatalf("Status = %q, want %q", run.Status, snapshot.RunPartial)
	}
	if got := failureFor(t, run, snapshot.ResourcePod).Reason; got != snapshot.FailureForbidden {
		t.Errorf("pod failure reason = %q, want %q", got, snapshot.FailureForbidden)
	}
	if n := run.Observation.Counts()[snapshot.ResourcePod]; n != 0 {
		t.Errorf("pod count = %d, want 0 when the pod list failed", n)
	}
	// 其余资源照常采到：中断整轮会把一个权限缺口表现成"什么都采不到"。
	if n := run.Observation.Counts()[snapshot.ResourceNamespace]; n != 1 {
		t.Errorf("namespace count = %d, want 1; one failing kind must not abort the run", n)
	}
}

func TestCollectIsFailedWhenEveryResourceKindFails(t *testing.T) {
	c, cs := newTestCollector(t, healthyObjects()...)
	failList(cs, "*", forbidden("everything"))

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if run.Status != snapshot.RunFailed {
		t.Fatalf("Status = %q, want %q", run.Status, snapshot.RunFailed)
	}
	if len(run.Failures) != 8 {
		t.Errorf("len(Failures) = %d, want 8 (one per attempted kind); got %+v", len(run.Failures), run.Failures)
	}
}

// 每一类资源都必须真的被尝试过。少尝试一类会让 DeriveRunStatus 的分母
// 变小，于是"我们没去采"和"采到了"在 status 上无法区分。
func TestCollectAttemptsEveryResourceKind(t *testing.T) {
	c, cs := newTestCollector(t, healthyObjects()...)
	failList(cs, "*", forbidden("everything"))

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}

	want := []snapshot.ResourceKind{
		snapshot.ResourceNamespace, snapshot.ResourceReplicaSet, snapshot.ResourcePod,
		snapshot.ResourceNode, snapshot.ResourceService, snapshot.ResourceEndpointSlice,
		snapshot.ResourceNetworkPolicy, snapshot.ResourceIngress,
	}
	for _, kind := range want {
		failureFor(t, run, kind)
	}
}

func TestCollectSharesOneObservedAtAcrossTheRun(t *testing.T) {
	c, _ := newTestCollector(t, healthyObjects()...)

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if !run.Observation.ObservedAt.Equal(run.StartedAt) {
		t.Errorf("ObservedAt = %v, StartedAt = %v; the whole batch must share one observation instant",
			run.Observation.ObservedAt, run.StartedAt)
	}
	if !run.FinishedAt.After(run.StartedAt) {
		t.Errorf("FinishedAt = %v, StartedAt = %v; want FinishedAt taken after the run",
			run.FinishedAt, run.StartedAt)
	}
	if run.Observation.RunID != "run-1" || run.Observation.ClusterID != testClusterID {
		t.Errorf("Observation identity = (%q, %q), want (%q, %q)",
			run.Observation.ClusterID, run.Observation.RunID, testClusterID, "run-1")
	}
}

// 采集是只读的。这条把它从注释变成每次运行都被检查的事实：
// 整轮结束后 apiserver 上不该留下 list 以外的任何动作。
func TestCollectIssuesNothingButListCalls(t *testing.T) {
	c, cs := newTestCollector(t, healthyObjects()...)

	if _, err := c.Collect(context.Background(), "run-1"); err != nil {
		t.Fatalf("Collect = %v", err)
	}

	actions := cs.Actions()
	if len(actions) == 0 {
		t.Fatal("Collect issued no API calls at all")
	}
	for _, a := range actions {
		if a.GetVerb() != "list" {
			t.Errorf("Collect issued %q on %s; the collector must never write to a cluster",
				a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestClassifyFailureMapsErrorsToTheClosedEnum(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name string
		err  error
		want snapshot.FailureReason
	}{
		{"forbidden", apierrors.NewForbidden(gr, "", errors.New("nope")), snapshot.FailureForbidden},
		{"unauthorized", apierrors.NewUnauthorized("no token"), snapshot.FailureForbidden},
		{"not found", apierrors.NewNotFound(gr, "x"), snapshot.FailureNotFound},
		{"api timeout", apierrors.NewTimeoutError("slow", 1), snapshot.FailureTimeout},
		{"context deadline", context.DeadlineExceeded, snapshot.FailureTimeout},
		{"service unavailable", apierrors.NewServiceUnavailable("down"), snapshot.FailureUnavailable},
		{"server timeout", apierrors.NewServerTimeout(gr, "list", 1), snapshot.FailureUnavailable},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), snapshot.FailureUnavailable},
		{"anything else", errors.New("connection reset"), snapshot.FailureOther},
		// 包装过的错误必须仍然归对：采集器把每个 List 错误都 %w 进了
		// 带集群名的上下文里，只认裸错误会让全部失败退化成 OTHER。
		{"wrapped forbidden", errors.Join(errors.New("list pods in prod"),
			apierrors.NewForbidden(gr, "", errors.New("nope"))), snapshot.FailureForbidden},
		{"wrapped deadline", errors.Join(errors.New("list pods in prod"),
			context.DeadlineExceeded), snapshot.FailureTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyFailure(c.err); got != c.want {
				t.Errorf("classifyFailure(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// 失败记录必须带上真实的错误文本，且不得把凭据一类的东西带进去。
// 这里只钉前半条：detail 为空会让操作者只看到一个枚举值。
func TestCollectRecordsFailureDetail(t *testing.T) {
	c, cs := newTestCollector(t, healthyObjects()...)
	failList(cs, "networkpolicies", forbidden("networkpolicies"))

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if d := failureFor(t, run, snapshot.ResourceNetworkPolicy).Detail; d == "" {
		t.Error("networkpolicy failure Detail is empty; the operator gets an enum with no evidence")
	}
}

func TestEachPageWalksEveryContinuationToken(t *testing.T) {
	var seen []metav1.ListOptions
	tokens := []string{"page-2", "page-3", ""}

	err := eachPage(context.Background(), func(_ context.Context, opts metav1.ListOptions) (string, error) {
		seen = append(seen, opts)
		return tokens[len(seen)-1], nil
	})
	if err != nil {
		t.Fatalf("eachPage = %v", err)
	}

	if len(seen) != 3 {
		t.Fatalf("eachPage made %d calls, want 3; a break in the middle yields a partial cluster that looks whole", len(seen))
	}
	wantContinue := []string{"", "page-2", "page-3"}
	for i, opts := range seen {
		if opts.Continue != wantContinue[i] {
			t.Errorf("call %d Continue = %q, want %q", i, opts.Continue, wantContinue[i])
		}
		if opts.Limit != listPageSize {
			t.Errorf("call %d Limit = %d, want %d", i, opts.Limit, listPageSize)
		}
	}
}

func TestEachPageStopsAtTheFirstError(t *testing.T) {
	want := errors.New("boom")
	calls := 0

	err := eachPage(context.Background(), func(_ context.Context, _ metav1.ListOptions) (string, error) {
		calls++
		return "page-2", want
	})
	if !errors.Is(err, want) {
		t.Fatalf("eachPage = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Errorf("eachPage made %d calls after an error, want 1", calls)
	}
}

// pagedListReactor 把一类资源的 List 拆成多页：第 i 次调用返回 pages[i]
// 与指向下一页的 continue token，最后一页返回空 token。
// 它同时记录每次收到的 ListOptions，用来验证 token 真的被回传了。
func pagedListReactor[L runtime.Object](cs *fake.Clientset, resource string, pages []L, setContinue func(L, string)) *[]metav1.ListOptions {
	var seen []metav1.ListOptions
	cs.PrependReactor("list", resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		opts := action.(k8stesting.ListActionImpl).ListOptions
		seen = append(seen, opts)
		i := len(seen) - 1
		if i >= len(pages) {
			return true, nil, errors.New("collector asked for more pages than the fixture has")
		}
		page := pages[i]
		if i < len(pages)-1 {
			setContinue(page, fmt.Sprintf("token-%d", i))
		}
		return true, page, nil
	})
	return &seen
}

// 分页截断与"整类采集失败"是同一种缺陷，只是走了另一条路：
// 停在第一页会交出一个看起来完整、实际少了大半 Pod 的集群。
func TestCollectPodsWalksEveryPage(t *testing.T) {
	c, cs := newTestCollector(t, healthyObjects()...)

	pages := []*corev1.PodList{
		{Items: []corev1.Pod{podOnPage("a"), podOnPage("b")}},
		{Items: []corev1.Pod{podOnPage("c")}},
	}
	seen := pagedListReactor(cs, "pods", pages, func(l *corev1.PodList, tok string) { l.Continue = tok })

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if n := run.Observation.Counts()[snapshot.ResourcePod]; n != 3 {
		t.Fatalf("pod count = %d, want 3 across two pages", n)
	}
	if len(*seen) != 2 {
		t.Fatalf("pods listed %d times, want 2", len(*seen))
	}
	if got := (*seen)[1].Continue; got != "token-0" {
		t.Errorf("second page Continue = %q, want %q", got, "token-0")
	}
}

func podOnPage(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.5"},
	}
}

func TestCollectNamespacesWalksEveryPage(t *testing.T) {
	c, cs := newTestCollector(t, healthyObjects()...)

	pages := []*corev1.NamespaceList{
		{Items: []corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}},
		{Items: []corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "b"}}}},
	}
	pagedListReactor(cs, "namespaces", pages, func(l *corev1.NamespaceList, tok string) { l.Continue = tok })

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if n := run.Observation.Counts()[snapshot.ResourceNamespace]; n != 2 {
		t.Errorf("namespace count = %d, want 2 across two pages", n)
	}
}

// ReplicaSet 索引跨页丢失不会让采集报错，只会让第二页上的 Pod
// 静默解析不到 Deployment —— 一个没有任何红色信号的降级。
func TestCollectReplicaSetOwnersWalksEveryPage(t *testing.T) {
	c, cs := newTestCollector(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "ReplicaSet", Name: "second-page-rs", Controller: boolPtr(true)},
				},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.5"},
		},
	)

	rs := func(name, owner string) appsv1.ReplicaSet {
		return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: owner, Controller: boolPtr(true)},
			},
		}}
	}
	pages := []*appsv1.ReplicaSetList{
		{Items: []appsv1.ReplicaSet{rs("first-page-rs", "other")}},
		{Items: []appsv1.ReplicaSet{rs("second-page-rs", "web")}},
	}
	pagedListReactor(cs, "replicasets", pages, func(l *appsv1.ReplicaSetList, tok string) { l.Continue = tok })

	run, err := c.Collect(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Collect = %v", err)
	}
	if len(run.Observation.Pods) != 1 {
		t.Fatalf("pods = %d, want 1", len(run.Observation.Pods))
	}
	got := run.Observation.Pods[0]
	if got.WorkloadKind != "Deployment" || got.WorkloadName != "web" {
		t.Errorf("workload = (%q, %q), want (Deployment, web); the owning ReplicaSet was on page 2",
			got.WorkloadKind, got.WorkloadName)
	}
}

func TestControllerOfPicksTheControllerNotTheFirstOwner(t *testing.T) {
	refs := []metav1.OwnerReference{
		{Kind: "SomethingElse", Name: "bystander"},
		{Kind: "ReplicaSet", Name: "web-5f7", Controller: boolPtr(false)},
		{Kind: "StatefulSet", Name: "db", Controller: boolPtr(true)},
	}
	got := controllerOf(refs)
	if got.Kind != "StatefulSet" || got.Name != "db" {
		t.Errorf("controllerOf = %+v, want {StatefulSet db}", got)
	}
}

func TestControllerOfReturnsEmptyWithoutAController(t *testing.T) {
	cases := [][]metav1.OwnerReference{
		nil,
		{{Kind: "ReplicaSet", Name: "web"}},
		{{Kind: "ReplicaSet", Name: "web", Controller: boolPtr(false)}},
	}
	for _, refs := range cases {
		if got := controllerOf(refs); !got.empty() {
			t.Errorf("controllerOf(%+v) = %+v, want empty", refs, got)
		}
	}
}

func TestFailedReportsPerResourceKind(t *testing.T) {
	failures := []snapshot.Failure{{Resource: snapshot.ResourceService}}
	if !failed(failures, snapshot.ResourceService) {
		t.Error("failed(SERVICE) = false, want true")
	}
	if failed(failures, snapshot.ResourceEndpointSlice) {
		t.Error("failed(ENDPOINTSLICE) = true, want false")
	}
	if failed(nil, snapshot.ResourceService) {
		t.Error("failed(nil, SERVICE) = true, want false")
	}
}
