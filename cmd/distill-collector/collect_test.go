package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/kubeclient"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshot"
)

const testClusterID = "prod"

// recordingStore 记下每一次落库收到的东西，以及落库那一刻上下文的状态。
type recordingStore struct {
	runs   []snapshot.Run
	ctxErr []error
	err    error
	// aborted 记下每一次「还没开始采集就失败」的落库。
	aborted []abortedRun
}

// abortedRun 是一次中止运行的落库参数。
type abortedRun struct {
	clusterID string
	runID     string
	reason    snapshot.RunErrorReason
}

func (s *recordingStore) Save(ctx context.Context, run snapshot.Run) error {
	s.runs = append(s.runs, run)
	s.ctxErr = append(s.ctxErr, ctx.Err())
	return s.err
}

func (s *recordingStore) SaveAbortedRun(
	_ context.Context, clusterID, runID string, _, _ time.Time, reason snapshot.RunErrorReason,
) error {
	s.aborted = append(s.aborted, abortedRun{clusterID: clusterID, runID: runID, reason: reason})
	return s.err
}

// onlyAborted 返回唯一一次中止落库；不是恰好一次即致命。
func (s *recordingStore) onlyAborted(t *testing.T) abortedRun {
	t.Helper()
	if len(s.aborted) != 1 {
		t.Fatalf("store received %d aborted runs, want exactly 1", len(s.aborted))
	}
	return s.aborted[0]
}

// only 返回唯一一次落库收到的运行；不是恰好一次即致命。
func (s *recordingStore) only(t *testing.T) snapshot.Run {
	t.Helper()
	if len(s.runs) != 1 {
		t.Fatalf("store received %d runs, want exactly 1", len(s.runs))
	}
	return s.runs[0]
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testFleet() *cluster.Registry {
	reg, _ := fleetRegistry([]registry.Cluster{
		{ID: testClusterID, PodCIDR: "10.4.0.0/16", NodeCIDR: "192.168.0.0/24"},
	})
	return reg
}

// answerReviews 让 fake 按 allowed 回答每一次只读自证的问询。
//
// fake clientset 不做鉴权，SSAR 打到它身上不会得到一个可用的应答 ——
// 自证的两个分支在 fake 上都得自己接管。这也正是 spec §5.4 记下的
// 永久未覆盖面：这条守卫的真实受力只能在 kind 集群上证伪。
func answerReviews(cs *fake.Clientset, allowed bool) {
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			review, ok := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			if !ok {
				return true, nil, errors.New("unexpected object submitted for review")
			}
			out := review.DeepCopy()
			out.Status.Allowed = allowed
			return true, out, nil
		})
}

// readOnlyCluster 是一个回答"你不能写策略"的空集群。
func readOnlyCluster() *fake.Clientset {
	cs := fake.NewClientset()
	answerReviews(cs, false)
	return cs
}

// forbidList 让某一类资源的 List 返回 403。
func forbidList(cs *fake.Clientset, resource string) {
	cs.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: resource}, "", errors.New("no read access"))
	})
}

// firstList 返回第一次读集群的动作下标，没有则返回 -1。
func firstList(actions []k8stesting.Action) int {
	for i, a := range actions {
		if a.GetVerb() == "list" {
			return i
		}
	}
	return -1
}

// lastSelfCheck 返回最后一次自证问询的下标，没有则返回 -1。
func lastSelfCheck(actions []k8stesting.Action) int {
	last := -1
	for i, a := range actions {
		if a.GetVerb() == "create" && a.GetResource().Resource == "selfsubjectaccessreviews" {
			last = i
		}
	}
	return last
}

// 只读自证必须在**任何一次读集群之前**跑完（spec §3.3）。
//
// 这条用例钉的是顺序，不是覆盖。internal/collect 的 AST 绊线只证明这个
// 二进制调用了 AssertReadOnly，证明不了它调用在什么时候 —— 把调用挪到
// Collect 之后，那条绊线依旧是绿的，而这条会变红。自证的全部价值在于
// 它跑在读集群之前：读完才自证，证的是"我接下来不打算写"，而这条自检
// 要回答的是"我到底有没有写的权限"。
func TestReadOnlyIsProvenBeforeTheClusterIsRead(t *testing.T) {
	cs := readOnlyCluster()
	store := &recordingStore{}

	if _, err := collectOnce(t.Context(), testClusterID, cs, testFleet(), store, quietLogger()); err != nil {
		t.Fatalf("collectOnce() error = %v", err)
	}

	actions := cs.Actions()
	selfCheck, list := lastSelfCheck(actions), firstList(actions)
	if selfCheck < 0 {
		t.Fatal("no SelfSubjectAccessReview was issued: the collector never proved it is read-only")
	}
	if list < 0 {
		t.Fatal("no list was issued: the collector never read the cluster")
	}
	if selfCheck > list {
		t.Errorf("the read-only self-check finished at action %d but the first list happened at action %d: "+
			"a process that has already read the cluster is proving intent, not permission (spec §3.3)",
			selfCheck, list)
	}
}

// 自证失败时一个资源都不许读，但这一轮必须在历史里留下痕迹。
//
// 前半条是守卫本身。后半条是它 2026-08-16 才补上的另一半：原先这条路径
// 一行 collection_run 也不写，于是界面显示「这个集群还没有过任何一次资产
// 采集」—— 与一个刚注册、采集器压根没被拉起来过的集群一模一样，
// 而操作者会去等一次因为权限配错而永远不会成功的采集。
//
// 落的是**中止运行**而不是一份空快照：后者会给每一类资源写一行 0，
// 读起来像一个空集群，而事实是我们根本没看过它。
func TestWriteAccessStopsTheRunButLeavesATrace(t *testing.T) {
	cs := fake.NewClientset()
	answerReviews(cs, true)
	store := &recordingStore{}

	_, err := collectOnce(t.Context(), testClusterID, cs, testFleet(), store, quietLogger())
	if !errors.Is(err, ErrNotProvenReadOnly) {
		t.Fatalf("collectOnce() error = %v, want %v", err, ErrNotProvenReadOnly)
	}
	if got := firstList(cs.Actions()); got >= 0 {
		t.Errorf("the collector listed cluster resources (action %d) after failing its read-only self-check", got)
	}
	if len(store.runs) != 0 {
		t.Errorf("store received %d full runs, want 0 — nothing was observed, so nothing may be counted", len(store.runs))
	}

	got := store.onlyAborted(t)
	if got.reason != snapshot.RunErrorReadOnlyUnproven {
		t.Errorf("aborted reason = %q, want %q", got.reason, snapshot.RunErrorReadOnlyUnproven)
	}
	if got.clusterID != testClusterID {
		t.Errorf("aborted clusterID = %q, want %q", got.clusterID, testClusterID)
	}
	if got.runID == "" {
		t.Error("aborted run has no run id; a run that cannot be joined is not a run")
	}
}

// apiserver 地址被出站守卫拒绝，不得报成"没能证明只读"。
//
// 这条来自 2026-08-17 对真实集群的实测：把 kubeconfig 指到
// https://169.254.169.254:6443，采集器答的是"could not prove read-only
// access"，于是操作者去查 RBAC，而真正的原因是地址被本平台的守卫拒了。
//
// 成因在时序上：kubeclient.New 只解析 kubeconfig、不拨号，守卫要到第一次
// 真实 API 调用才发力 —— 而那次调用恰好是自证本身。两件完全不同的事
// 因此落在同一个分支里。RunErrorClientUnavailable 这个枚举值正是为它
// 准备的，此前不可达。
//
// 三个方向一起断言，少一条都不够：错误要能与只读自证失败区分开、
// 落库原因要是 CLIENT_UNAVAILABLE、且错误文本里不得出现 apiserver 地址
// （规范 §19、§22 —— 这个进程的输出终点是集群日志）。
func TestABlockedAPIServerIsNotReportedAsUnprovenReadOnly(t *testing.T) {
	const blocked = "https://169.254.169.254:6443/apis/authorization.k8s.io/v1"

	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			// 形状照抄真实链路：client-go 把拨号错误包进 url.Error，
			// 实测 errors.Is 能穿透它。
			return true, nil, fmt.Errorf("Get %q: %w", blocked, kubeclient.ErrBlockedDestination)
		})
	store := &recordingStore{}

	_, err := collectOnce(t.Context(), testClusterID, cs, testFleet(), store, quietLogger())
	if !errors.Is(err, ErrBlockedAPIServer) {
		t.Fatalf("collectOnce() error = %v, want %v", err, ErrBlockedAPIServer)
	}
	if errors.Is(err, ErrNotProvenReadOnly) {
		t.Error("a rejected address is reported as an unproven read-only check; " +
			"the operator will go and audit RBAC for an address problem")
	}
	if got := store.onlyAborted(t); got.reason != snapshot.RunErrorClientUnavailable {
		t.Errorf("aborted reason = %q, want %q", got.reason, snapshot.RunErrorClientUnavailable)
	}
	if strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("error text = %q, carries the apiserver address", err.Error())
	}
}

// 自证失败的错误不得带 apiserver 的原文（规范 §19、§22）：这个进程的
// 输出终点是集群日志，而那段文本里有 apiserver 地址。
func TestTheSelfCheckFailureCarriesNoAPIServerDetail(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New(`Post "https://10.9.8.7:6443/apis/authorization.k8s.io/v1": dial tcp timeout`)
		})

	_, err := collectOnce(t.Context(), testClusterID, cs, testFleet(), &recordingStore{}, quietLogger())
	if !errors.Is(err, ErrNotProvenReadOnly) {
		t.Fatalf("collectOnce() error = %v, want %v", err, ErrNotProvenReadOnly)
	}
	if got := err.Error(); got != ErrNotProvenReadOnly.Error() {
		t.Errorf("error text = %q, want exactly %q — the apiserver address must not travel with it",
			got, ErrNotProvenReadOnly.Error())
	}
}

// 一次部分成功的采集必须原样落成 PARTIAL。
//
// 拍平成 OK 的后果不是少一个标签：下游会把一份缺了 NetworkPolicy 的快照
// 当作完整事实，于是"这个集群没有任何策略"与"我们没被授权看策略"变得
// 无法区分，而前者会让平台推荐一份 default-deny（spec §4.2）。
func TestAPartialCollectionIsStoredAsPartial(t *testing.T) {
	cs := readOnlyCluster()
	forbidList(cs, "networkpolicies")
	store := &recordingStore{}

	result, err := collectOnce(t.Context(), testClusterID, cs, testFleet(), store, quietLogger())
	if err != nil {
		t.Fatalf("collectOnce() error = %v, want a stored partial run — one refused resource kind must not abandon the whole run", err)
	}

	saved := store.only(t)
	if saved.Status != snapshot.RunPartial {
		t.Errorf("stored status = %q, want %q", saved.Status, snapshot.RunPartial)
	}
	if result.Status != saved.Status {
		t.Errorf("returned status %q but stored %q: assembly must not relabel the collector's verdict",
			result.Status, saved.Status)
	}
	if len(saved.Failures) != 1 || saved.Failures[0].Resource != snapshot.ResourceNetworkPolicy {
		t.Fatalf("stored failures = %+v, want exactly the network policy one", saved.Failures)
	}
	if got := saved.Failures[0].Reason; got != snapshot.FailureForbidden {
		t.Errorf("stored failure reason = %q, want %q — FORBIDDEN is fixed by changing RBAC, not by retrying",
			got, snapshot.FailureForbidden)
	}
}

// 干净的一轮落成 OK，且带上 run id 与观测时刻 —— 落库的连接键。
func TestACleanCollectionIsStoredAsOK(t *testing.T) {
	store := &recordingStore{}

	result, err := collectOnce(t.Context(), testClusterID, readOnlyCluster(), testFleet(), store, quietLogger())
	if err != nil {
		t.Fatalf("collectOnce() error = %v", err)
	}

	saved := store.only(t)
	if saved.Status != snapshot.RunOK {
		t.Errorf("stored status = %q, want %q", saved.Status, snapshot.RunOK)
	}
	if saved.Observation.ClusterID != testClusterID {
		t.Errorf("stored cluster id = %q, want %q", saved.Observation.ClusterID, testClusterID)
	}
	if saved.Observation.RunID == "" || saved.Observation.RunID != result.Observation.RunID {
		t.Errorf("stored run id = %q, returned %q: a run that cannot be joined is not a run",
			saved.Observation.RunID, result.Observation.RunID)
	}
	if saved.Observation.ObservedAt.IsZero() {
		t.Error("stored observation has no observed_at: it is the join key of every observed_* row")
	}
}

// 采集被打断之后，已经采到的部分与"为什么不完整"仍然要落库。
//
// 复用那个已经取消的 ctx 会让它们随之消失，表现为一次完全没留下痕迹的
// 运行 —— 与"采集器根本没被拉起来"无法区分。
func TestACancelledRunStillReachesTheStore(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store := &recordingStore{}

	cs := readOnlyCluster()
	cs.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})

	if _, err := collectOnce(ctx, testClusterID, cs, testFleet(), store, quietLogger()); err != nil {
		t.Fatalf("collectOnce() error = %v", err)
	}
	store.only(t)
	if store.ctxErr[0] != nil {
		t.Errorf("the store was called with a context already in error (%v): "+
			"what was collected before the interruption is exactly what must survive it", store.ctxErr[0])
	}
}

// 落库失败必须变成一个错误，不得静默地当作采过了。
func TestAFailedStoreIsReported(t *testing.T) {
	want := errors.New("mysql is down")
	store := &recordingStore{err: want}

	_, err := collectOnce(t.Context(), testClusterID, readOnlyCluster(), testFleet(), store, quietLogger())
	if !errors.Is(err, want) {
		t.Fatalf("collectOnce() error = %v, want it to wrap %v", err, want)
	}
}

// 两次运行不得共用一个 ID：collection_run 的主键是 (cluster_id, run_id)，
// 撞上之后第二次采集整体失败，而不是两行并存。
func TestRunIDsDoNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for range 32 {
		id, err := newRunID()
		if err != nil {
			t.Fatalf("newRunID() error = %v", err)
		}
		if seen[id] {
			t.Fatalf("newRunID() returned %q twice", id)
		}
		seen[id] = true
	}
}
