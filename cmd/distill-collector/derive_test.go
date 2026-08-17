package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/hubble"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// derivedRun 是一次推导收到的参数。
type derivedRun struct {
	clusterID string
	runID     string
}

// recordingDeriveStore 记下推导侧收到的东西，以及每一次推导发生的那一刻
// 互斥是不是真的握在手里。
//
// derivedUnlocked 是这组用例里唯一一个"并发"能被证伪的地方：真的起两个进程
// 去撞是不可复现的，而"推导时手里没有锁"是同一件事的可判定形式 —— 拿掉串行化
// 之后它立刻非零。
type recordingDeriveStore struct {
	lockedFor  []string
	held       bool
	releases   int
	lockErr    error
	releaseErr error

	derived         []derivedRun
	derivedUnlocked int
	deriveErr       error

	deriveRuns   []snapshotstore.DeriveRun
	deriveRunErr error
}

func (s *recordingDeriveStore) LockCluster(
	_ context.Context, clusterID string,
) (func(context.Context) error, error) {
	s.lockedFor = append(s.lockedFor, clusterID)
	if s.lockErr != nil {
		return nil, s.lockErr
	}
	s.held = true
	return func(context.Context) error {
		s.held = false
		s.releases++
		return s.releaseErr
	}, nil
}

func (s *recordingDeriveStore) DeriveIdentityIntervals(_ context.Context, clusterID, runID string) error {
	if !s.held {
		s.derivedUnlocked++
	}
	s.derived = append(s.derived, derivedRun{clusterID: clusterID, runID: runID})
	return s.deriveErr
}

func (s *recordingDeriveStore) SaveDeriveRun(_ context.Context, run snapshotstore.DeriveRun) error {
	s.deriveRuns = append(s.deriveRuns, run)
	return s.deriveRunErr
}

// onlyDeriveRun 返回唯一一次推导落库；不是恰好一次即致命。
func (s *recordingDeriveStore) onlyDeriveRun(t *testing.T) snapshotstore.DeriveRun {
	t.Helper()
	if len(s.deriveRuns) != 1 {
		t.Fatalf("store received %d derive runs, want exactly 1", len(s.deriveRuns))
	}
	return s.deriveRuns[0]
}

// contended 是一次"这个集群另有推导在跑"的取锁失败，形状照抄 snapshotstore。
func contended() error {
	return fmt.Errorf("cluster %s: %w", testClusterID, snapshotstore.ErrDeriveInProgress)
}

// 一次落库成功的采集必须真的把这次运行交给身份推导。
//
// **这条用例钉的是调用点本身，不是推导对不对。** DeriveIdentityIntervals 早就
// 实现完、测完了，缺的只是没人调它 —— 于是 pod_identity_interval 一直是空表，
// 而事实层里每条连接的两端都靠它解析主体，表空着就意味着一条也归属不了
// （spec §2）。把 collectAndIngest 里那一行 deriveOnce 删掉，第一个断言变红。
//
// 断言里带上 run id 相等：推错运行与不推同样致命 —— 拿另一次运行的观测去关
// 这次的区间，等于把一批仍在跑的 Pod 判成下线。
func TestACollectionHandsTheRunToIdentityDerivation(t *testing.T) {
	store := newCollectorStore()

	if err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testFleet(),
		store, nil, testWindow(), quietLogger()); err != nil {
		t.Fatalf("collectAndIngest() error = %v", err)
	}

	collection := store.only(t)
	if len(store.derived) != 1 {
		t.Fatalf("identity derivation was invoked %d times, want exactly 1 — "+
			"an uncalled resolver leaves pod_identity_interval empty, and every connection "+
			"then resolves to nothing (spec §2)", len(store.derived))
	}
	if got := store.derived[0]; got.clusterID != testClusterID || got.runID != collection.Observation.RunID {
		t.Errorf("derivation was asked for (%s, %s), want (%s, %s) — deriving another run's "+
			"observation closes the intervals of pods that are still running",
			got.clusterID, got.runID, testClusterID, collection.Observation.RunID)
	}
}

// 推导必须在拿到该集群的互斥之后才发生，且结束后释放。
//
// **这条用例钉的是串行化。** 拿掉 deriveOnce 里的取锁，derivedUnlocked 立刻
// 非零。缺了这一层的后果不是"偶尔慢一点"：同一集群两次不同运行并发推导走的是
// UPDATE 路径，只在行锁上排队，后写的赢且**不报任何错** —— 一个仍在运行的 Pod
// 的区间被关在错的时刻，之后它的地址解析成 NOT_COVERED，或者解析成后来复用
// 这个地址的另一个工作负载。归属到错误的主体，且不报错（spec §2.1）。
func TestDerivationIsSerialisedPerCluster(t *testing.T) {
	store := newCollectorStore()

	if err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testFleet(),
		store, nil, testWindow(), quietLogger()); err != nil {
		t.Fatalf("collectAndIngest() error = %v", err)
	}

	if store.derivedUnlocked != 0 {
		t.Errorf("derivation ran %d time(s) without holding the per-cluster lock; "+
			"two concurrent derivations queue on row locks, the later writer wins, and nothing "+
			"reports an error — the interval is closed at the wrong moment (spec §2.1)",
			store.derivedUnlocked)
	}
	if len(store.lockedFor) != 1 || store.lockedFor[0] != testClusterID {
		t.Errorf("the derivation lock was taken for %v, want exactly [%s] — the mutual exclusion "+
			"is per cluster, and a lock taken for the wrong one excludes nothing",
			store.lockedFor, testClusterID)
	}
	if store.releases != 1 || store.held {
		t.Errorf("the derivation lock was released %d time(s) and held=%v at the end, want 1 and false",
			store.releases, store.held)
	}
}

// 拿不到互斥必须是一次明确的、被记下来的失败，绝不是跳过、更不是并发跑。
func TestAContendedDerivationFailsAndIsRecorded(t *testing.T) {
	store := newCollectorStore()
	store.lockErr = contended()

	err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testFleet(),
		store, nil, testWindow(), quietLogger())
	if !errors.Is(err, snapshotstore.ErrDeriveInProgress) {
		t.Fatalf("collectAndIngest() error = %v, want it to wrap %v — a swallowed contention "+
			"makes a run with no identities exit zero", err, snapshotstore.ErrDeriveInProgress)
	}
	if len(store.derived) != 0 {
		t.Errorf("derivation ran %d time(s) after failing to take the lock, want 0 — "+
			"failing to acquire must never fall through to a concurrent derivation", len(store.derived))
	}

	got := store.onlyDeriveRun(t)
	if got.Status != snapshotstore.DeriveFailed {
		t.Errorf("recorded derive status = %q, want %q", got.Status, snapshotstore.DeriveFailed)
	}
	if got.ErrorReason != snapshotstore.DeriveErrorLockUnavailable {
		t.Errorf("recorded derive reason = %q, want %q — LOCK_UNAVAILABLE says «rerun it», "+
			"any other reason sends the operator to inspect a database that is fine",
			got.ErrorReason, snapshotstore.DeriveErrorLockUnavailable)
	}
	if store.only(t).Status != snapshot.RunOK {
		t.Error("the stored collection no longer reads OK; the assets are already a fact and " +
			"a derivation that never ran must not rewrite them")
	}
}

// 推导与采集的成败必须分开记（spec §2.2）。
//
// **这条用例钉的是两份记录不合并。** 把推导失败写进采集那一行（或者干脆不写），
// 后面几个断言会变红。分开不是整洁：资产已经落库是既成事实，推导失败不能把它
// 抹掉；反过来推导失败也不能被吞掉 —— 那会让一次"资产有了、身份没有"的运行
// 看起来完全正常，而下游的表现是这个集群的每一条连接都归属不了。
func TestDerivationAndCollectionAreRecordedApart(t *testing.T) {
	store := newCollectorStore()
	store.deriveErr = errors.New("the observation carries two subjects at one instant")

	err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testFleet(),
		store, nil, testWindow(), quietLogger())
	if !errors.Is(err, store.deriveErr) {
		t.Fatalf("collectAndIngest() error = %v, want it to wrap the derivation failure %v",
			err, store.deriveErr)
	}

	collection := store.only(t)
	if collection.Status != snapshot.RunOK {
		t.Errorf("stored collection status = %q, want %q — the assets are worth keeping",
			collection.Status, snapshot.RunOK)
	}
	if len(store.aborted) != 0 {
		t.Errorf("store received %d aborted collection runs, want 0 — a derivation failure is "+
			"not a collection that never read the cluster", len(store.aborted))
	}

	got := store.onlyDeriveRun(t)
	if got.Status != snapshotstore.DeriveFailed {
		t.Errorf("recorded derive status = %q, want %q", got.Status, snapshotstore.DeriveFailed)
	}
	if got.ErrorReason != snapshotstore.DeriveErrorOther {
		t.Errorf("recorded derive reason = %q, want %q", got.ErrorReason, snapshotstore.DeriveErrorOther)
	}
	if got.ClusterID != testClusterID {
		t.Errorf("recorded derive cluster id = %q, want %q — every identity row is keyed by cluster",
			got.ClusterID, testClusterID)
	}
	// 推导记录带的是**被推导的那次采集运行**的 id，不是一个新造的 id：这张表
	// 要回答"这次采集的身份推出来了吗"，认不出是哪次采集就答不出。这与摄入
	// 恰好相反 —— 摄入是一件独立的事，因此它有自己的 run id。
	if got.RunID != collection.Observation.RunID {
		t.Errorf("recorded derive run id = %q, want the collection run id %q",
			got.RunID, collection.Observation.RunID)
	}
}

// 一次不完整的采集照样交给推导，由推导自己判断能不能关区间。
//
// 在这里自己加一条"只有 OK 才推"的前置条件，会把 identity.RunFacts 已经守着的
// 那个判断复制到一个没有测试盯着的地方，两处口径迟早分叉。而 PARTIAL 的运行
// 仍然要**开出**区间：那些 Pod 是真的看见了的。
func TestAnIncompleteCollectionIsStillHandedToDerivation(t *testing.T) {
	cs := readOnlyCluster()
	forbidList(cs, "networkpolicies")
	store := newCollectorStore()

	if err := collectAndIngest(t.Context(), testClusterID, cs, testFleet(),
		store, nil, testWindow(), quietLogger()); err != nil {
		t.Fatalf("collectAndIngest() error = %v", err)
	}

	collection := store.only(t)
	if collection.Status != snapshot.RunPartial {
		t.Fatalf("stored collection status = %q, want %q", collection.Status, snapshot.RunPartial)
	}
	if len(store.derived) != 1 || store.derived[0].runID != collection.Observation.RunID {
		t.Errorf("a PARTIAL run was handed to derivation %d time(s) (%v), want exactly the run itself — "+
			"whether it may close intervals is identity.RunFacts's judgement, not this layer's",
			len(store.derived), store.derived)
	}
}

// 采集根本没读到集群时不得推导，也不得留下任何一行推导记录。
//
// 反方向的同一条要求：一行"推导成功"配着一次没读到集群的采集，读起来像是
// 这个集群的身份是最新的，而事实是这一轮什么都没看过。
func TestAFailedCollectionDerivesNothing(t *testing.T) {
	cs := fake.NewClientset()
	answerReviews(cs, true)
	store := newCollectorStore()

	err := collectAndIngest(t.Context(), testClusterID, cs, testFleet(),
		store, nil, testWindow(), quietLogger())
	if !errors.Is(err, ErrNotProvenReadOnly) {
		t.Fatalf("collectAndIngest() error = %v, want %v", err, ErrNotProvenReadOnly)
	}
	if len(store.derived) != 0 || len(store.deriveRuns) != 0 {
		t.Errorf("a collection that never read the cluster produced %d derivations and %d records, want 0 and 0",
			len(store.derived), len(store.deriveRuns))
	}
	if len(store.lockedFor) != 0 {
		t.Errorf("the derivation lock was taken %d time(s) for a run that never read the cluster, want 0",
			len(store.lockedFor))
	}
}

// 推导失败不得让这一轮的流量窗口白白丢掉。
//
// **三者失败的可挽回性不同，顺序按它排（spec §2.2）**：资产可以重采，推导可以
// 重跑（它读的是一次已经落库的采集运行），而流量不能重来 —— Hubble 从环形
// 缓冲区供数，一个没摄到的窗口就是没了，而"没观测到的流量"恰恰会被这个平台
// 读成"这条规则没有流量、可以收紧"。让可挽回的那次失败毁掉不可挽回的证据是反的。
//
// **三个断言缺一不可**，各自挡住一种照样能过的错误实现：
//   - 摄入落了库 —— 挡住"把短路加回来"（推导失败就提前返回，摄入根本没跑）；
//   - 错误里仍然带着推导失败 —— 挡住"摄入成功就把推导的错吞掉"；
//   - 推导那一行落成 FAILED —— 挡住"错误返回了、但库里没有任何痕迹"，
//     那会让一次身份没推出来的运行在界面上完全正常。
func TestADerivationFailureDoesNotCostTheFlowWindow(t *testing.T) {
	store := newCollectorStore()
	store.deriveErr = errors.New("the observation carries two subjects at one instant")
	src := hubbleSource(hubbleResult(t, testWindow(), oneConnection()), nil)

	err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testFleet(),
		store, src, testWindow(), quietLogger())

	got := store.onlyIngest(t)
	if got.Status != snapshotstore.IngestOK {
		t.Errorf("stored ingest status = %q, want %q — a derivation failure is re-runnable, "+
			"a missed flow window is not: Hubble serves from a ring buffer and that traffic is gone",
			got.Status, snapshotstore.IngestOK)
	}
	if conns, _ := got.Result.Connections(); len(conns) != 1 {
		t.Errorf("stored ingest carries %d connections, want 1 — traffic that was never observed "+
			"reads downstream as «this rule has no traffic, safe to tighten»", len(conns))
	}
	if !errors.Is(err, store.deriveErr) {
		t.Fatalf("collectAndIngest() error = %v, want it to wrap the derivation failure %v — "+
			"a successful ingestion must not swallow it", err, store.deriveErr)
	}
	if row := store.onlyDeriveRun(t); row.Status != snapshotstore.DeriveFailed {
		t.Errorf("recorded derive status = %q, want %q", row.Status, snapshotstore.DeriveFailed)
	}
}

// 摄入与推导同时失败时，两个都要浮上来，两行记录都要落下。
//
// 只报先到的那个，会让一次"摄入挂了、推导也挂了"的运行看起来只坏了一半，
// 于是另一半无人处置。反过来，摄入失败也不得跳过推导 —— 两者互不为前置条件，
// 提前返回等于把一个故障变成两个。
func TestBothIngestionAndDerivationFailuresSurface(t *testing.T) {
	store := newCollectorStore()
	store.deriveErr = errors.New("the observation carries two subjects at one instant")
	src := hubbleSource(flow.IngestResult{}, hubble.ErrRelayUnavailable)

	err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testFleet(),
		store, src, testWindow(), quietLogger())
	if !errors.Is(err, hubble.ErrRelayUnavailable) {
		t.Errorf("collectAndIngest() error = %v, want it to still carry the ingest failure %v",
			err, hubble.ErrRelayUnavailable)
	}
	if !errors.Is(err, store.deriveErr) {
		t.Errorf("collectAndIngest() error = %v, want it to still carry the derivation failure %v",
			err, store.deriveErr)
	}

	if got := store.onlyIngest(t); got.Status != snapshotstore.IngestFailed {
		t.Errorf("stored ingest status = %q, want %q", got.Status, snapshotstore.IngestFailed)
	}
	if len(store.derived) != 1 {
		t.Errorf("derivation ran %d time(s) after a failed ingestion, want 1 — the two are not "+
			"preconditions for each other, and returning early turns one fault into two",
			len(store.derived))
	}
	if row := store.onlyDeriveRun(t); row.Status != snapshotstore.DeriveFailed {
		t.Errorf("recorded derive status = %q, want %q", row.Status, snapshotstore.DeriveFailed)
	}
}

// 推导记录落不下去，必须变成错误，不得静默地当作推过了。
func TestAFailedDeriveRecordIsReported(t *testing.T) {
	want := errors.New("mysql is down")
	store := newCollectorStore()
	store.deriveRunErr = want

	err := collectAndIngest(t.Context(), testClusterID, readOnlyCluster(), testFleet(),
		store, nil, testWindow(), quietLogger())
	if !errors.Is(err, want) {
		t.Fatalf("collectAndIngest() error = %v, want it to wrap %v", err, want)
	}
}

// 推导失败的原因是封闭枚举，且认不出的形态落 OTHER 而不是猜一个。
//
// 原因决定的是处置：LOCK_UNAVAILABLE 的处置是重跑，其余的处置是去查库。
func TestDeriveErrorReasonsAreClosed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want snapshotstore.DeriveErrorReason
	}{
		{"a contended cluster", contended(), snapshotstore.DeriveErrorLockUnavailable},
		{"a deadline", context.DeadlineExceeded, snapshotstore.DeriveErrorTimeout},
		{"a wrapped contention", errors.Join(errors.New("gave up"), snapshotstore.ErrDeriveInProgress),
			snapshotstore.DeriveErrorLockUnavailable},
		{"an unrecognised failure", errors.New("something else"), snapshotstore.DeriveErrorOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveErrorReason(tc.err); got != tc.want {
				t.Errorf("deriveErrorReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
