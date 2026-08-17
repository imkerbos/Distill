package identity_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/identity"
)

var (
	// tSeen 是第一次看到 payment/api 的时刻，tLater 是下一次采集。
	tSeen  = time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	tLater = tSeen.Add(5 * time.Minute)
)

func apiIdentity() identity.Identity {
	return identity.Identity{
		Namespace:    "payment",
		PodName:      "api-7d9f-abcde",
		PodUID:       "uid-api-1",
		WorkloadKind: "Deployment",
		WorkloadName: "api",
	}
}

func workerIdentity() identity.Identity {
	return identity.Identity{
		Namespace:    "batch",
		PodName:      "worker-5c2a-fghij",
		PodUID:       "uid-worker-1",
		WorkloadKind: "Deployment",
		WorkloadName: "worker",
	}
}

// openAPIInterval 是 10.4.1.7 归 payment/api 的一个未关闭区间。
func openAPIInterval() identity.Interval {
	return identity.Interval{
		ClusterID:  "cluster-a",
		PodIP:      "10.4.1.7",
		ValidFrom:  tSeen,
		Identity:   apiIdentity(),
		FirstRunID: "run-1",
		LastRunID:  "run-1",
	}
}

// trustworthyRun 是一次整体成功、且 Pod 这一类确实采到的运行。
func trustworthyRun() identity.RunFacts {
	return identity.RunFacts{
		ClusterID:     "cluster-a",
		RunID:         "run-2",
		ObservedAt:    tLater,
		Status:        identity.RunOK,
		PodsCollected: true,
	}
}

// TestAnIncompleteRunClosesNothing 是全轮最重要的一条。
//
// 一次没采到 Pod 的运行，绝不能被读成「这些 Pod 消失了」。把 FORBIDDEN
// 读成 Pod 下线，会让平台认为一批工作负载已经消失，于是它们的策略被判成
// 「无流量、可收紧」—— 一次权限故障变成一次生产阻断建议。
func TestAnIncompleteRunClosesNothing(t *testing.T) {
	untrusted := []struct {
		name string
		run  identity.RunFacts
	}{
		{
			name: "status PARTIAL",
			run: identity.RunFacts{
				ClusterID: "cluster-a", RunID: "run-2", ObservedAt: tLater,
				Status: identity.RunPartial, PodsCollected: true,
			},
		},
		{
			name: "status FAILED",
			run: identity.RunFacts{
				ClusterID: "cluster-a", RunID: "run-2", ObservedAt: tLater,
				Status: identity.RunFailed, PodsCollected: false,
			},
		},
		{
			name: "status OK but pod collection failed",
			run: identity.RunFacts{
				ClusterID: "cluster-a", RunID: "run-2", ObservedAt: tLater,
				Status: identity.RunOK, PodsCollected: false,
			},
		},
		// 未登记的取值：枚举加了一个状态、这里没跟上时就是这个形状。
		// 它必须什么都关不了 —— "读不懂的状态"不是"可信的运行"。
		{
			name: "status unregistered",
			run: identity.RunFacts{
				ClusterID: "cluster-a", RunID: "run-2", ObservedAt: tLater,
				Status: identity.RunStatus("DEGRADED"), PodsCollected: true,
			},
		},
		// 零值：RunFacts 少填一个字段就编译得过，而它同样不得关任何区间。
		{
			name: "status left unset",
			run: identity.RunFacts{
				ClusterID: "cluster-a", RunID: "run-2", ObservedAt: tLater,
				PodsCollected: true,
			},
		},
	}
	for _, c := range untrusted {
		t.Run(c.name, func(t *testing.T) {
			got := identity.Derive([]identity.Interval{openAPIInterval()}, nil, c.run)
			if len(got) != 1 {
				t.Fatalf("Derive returned %d intervals, want 1", len(got))
			}
			if !got[0].Open() {
				t.Fatalf("an untrustworthy run (status=%s podsCollected=%t) closed the interval at %v, reason %q; "+
					"a run that could not read pods must never be read as those pods having disappeared",
					c.run.Status, c.run.PodsCollected, got[0].ValidTo, got[0].ClosedReason)
			}
			if got[0].ClosedReason != identity.ClosedNone {
				t.Fatalf("closed reason = %q, want empty on an open interval", got[0].ClosedReason)
			}
		})
	}

	// 对照组：没有它，上面三条在一个「永远不关任何区间」的实现上同样全绿。
	t.Run("control: a trustworthy run does close it", func(t *testing.T) {
		got := identity.Derive([]identity.Interval{openAPIInterval()}, nil, trustworthyRun())
		if len(got) != 1 {
			t.Fatalf("Derive returned %d intervals, want 1", len(got))
		}
		if got[0].Open() {
			t.Fatal("a trustworthy run left the interval of an unseen pod open; " +
				"then the three assertions above prove nothing")
		}
		if !got[0].ValidTo.Equal(tLater) {
			t.Fatalf("valid_to = %v, want %v (the moment of the closing run)", got[0].ValidTo, tLater)
		}
		if got[0].ClosedReason != identity.ClosedAbsentInLaterRun {
			t.Fatalf("closed reason = %q, want %q", got[0].ClosedReason, identity.ClosedAbsentInLaterRun)
		}
		// 关闭动作必须记下是哪一次运行关的，事后能追。
		if got[0].LastRunID != "run-2" {
			t.Fatalf("last_run_id = %q, want %q", got[0].LastRunID, "run-2")
		}
	})
}

// TestDeriveIsIdempotent 覆盖两个方向：同一入参连跑两次结果相同，以及把
// 上一次的产物再喂一遍不产生第二条区间 —— 重试、补跑与并发都会这样跑。
func TestDeriveIsIdempotent(t *testing.T) {
	prev := []identity.Interval{openAPIInterval()}
	obs := []identity.ObservedPod{{PodIP: "10.4.2.9", Identity: workerIdentity()}}
	run := trustworthyRun()

	first := identity.Derive(prev, obs, run)
	second := identity.Derive(prev, obs, run)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two runs over the same input differ:\n first  = %+v\n second = %+v", first, second)
	}

	third := identity.Derive(first, obs, run)
	if !reflect.DeepEqual(first, third) {
		t.Fatalf("re-deriving over its own output changed the result (%d intervals → %d):\n first = %+v\n third = %+v",
			len(first), len(third), first, third)
	}
}

// TestDeriveOpensANewIntervalWhenTheIPChangesHands 是引入区间的理由本身：
// 同一个地址今天属于 payment/api，下一次采集可能属于 batch/worker。
func TestDeriveOpensANewIntervalWhenTheIPChangesHands(t *testing.T) {
	prev := []identity.Interval{openAPIInterval()}
	obs := []identity.ObservedPod{{PodIP: "10.4.1.7", Identity: workerIdentity()}}

	got := identity.Derive(prev, obs, trustworthyRun())
	if len(got) != 2 {
		t.Fatalf("Derive returned %d intervals, want 2 (the old one closed, a new one opened)", len(got))
	}
	if got[0].Identity != apiIdentity() || !got[0].ValidTo.Equal(tLater) {
		t.Fatalf("first interval = %+v, want payment/api closed at %v", got[0], tLater)
	}
	if got[1].Identity != workerIdentity() || !got[1].ValidFrom.Equal(tLater) || !got[1].Open() {
		t.Fatalf("second interval = %+v, want batch/worker open from %v", got[1], tLater)
	}
	if got[1].FirstRunID != "run-2" {
		t.Fatalf("first_run_id = %q, want %q", got[1].FirstRunID, "run-2")
	}

	// 交接不得在那一刻变成 AMBIGUOUS：区间是半开的，
	// 而这一步同时证明 Derive 的产物真的能被 Resolve 用。
	if id, out := identity.Resolve(got, tSeen.Add(time.Minute)); out != identity.OutcomeResolved || id != apiIdentity() {
		t.Fatalf("before the handover: (%+v, %s), want payment/api RESOLVED", id, out)
	}
	if id, out := identity.Resolve(got, tLater); out != identity.OutcomeResolved || id != workerIdentity() {
		t.Fatalf("at the handover: (%+v, %s), want batch/worker RESOLVED", id, out)
	}
}

// degradedAPIIdentity 是 ReplicaSet 那一步没采到时，同一个 Pod 的样子：
// ownerRef 链走不到 Deployment，归属退化成那个 ReplicaSet 自己。
func degradedAPIIdentity() identity.Identity {
	id := apiIdentity()
	id.WorkloadKind = "ReplicaSet"
	id.WorkloadName = "api-7d9f"
	return id
}

// partialRun 是一次 ReplicaSet 采集失败的运行：Pod 采到了，整体是 PARTIAL。
func partialRun(runID string, at time.Time) identity.RunFacts {
	return identity.RunFacts{
		ClusterID:     "cluster-a",
		RunID:         runID,
		ObservedAt:    at,
		Status:        identity.RunPartial,
		PodsCollected: true,
	}
}

// TestADegradedWorkloadLabelDoesNotForkAPod —— 一个 Pod 的主体是它的 UID。
//
// workload_kind / workload_name 是采集当时解出来的属性，ReplicaSet 这一类采
// 不到时它们会退化。把退化算成"另一个主体"，同一个 Pod 会在同一个地址上多
// 出一个区间，于是这个地址从此答 AMBIGUOUS；而权限一直不恢复时每次运行都是
// PARTIAL，没有一次运行有资格关掉它们，退化就成了永久的。
func TestADegradedWorkloadLabelDoesNotForkAPod(t *testing.T) {
	pod := func(id identity.Identity) []identity.ObservedPod {
		return []identity.ObservedPod{{PodIP: "10.4.1.7", Identity: id}}
	}

	// 权限缺口从一开始就在：第一次运行就是退化的那次，区间带着 ReplicaSet 开出来。
	t.Run("a later trustworthy run corrects the label in place", func(t *testing.T) {
		opened := identity.Derive(nil, pod(degradedAPIIdentity()), partialRun("run-1", tSeen))
		if len(opened) != 1 {
			t.Fatalf("a degraded run opened %d intervals, want 1: %+v", len(opened), opened)
		}

		restored := identity.Derive(opened, pod(apiIdentity()), trustworthyRun())
		if len(restored) != 1 {
			t.Fatalf("after ReplicaSet access came back the pod holds %d intervals, want 1 "+
				"(a degraded label is not a second subject): %+v", len(restored), restored)
		}
		iv := restored[0]
		if !iv.ValidFrom.Equal(tSeen) || !iv.Open() || iv.FirstRunID != "run-1" {
			t.Fatalf("interval = %+v, want the original one still open from %v, first_run_id run-1", iv, tSeen)
		}
		if iv.Identity != apiIdentity() {
			t.Fatalf("workload = %s/%s, want Deployment/api: a trustworthy run resolved it, "+
				"and leaving the RBAC gap's answer in place would pin this pod to a ReplicaSet for its whole life",
				iv.Identity.WorkloadKind, iv.Identity.WorkloadName)
		}
		if id, out := identity.Resolve(restored, tLater); out != identity.OutcomeResolved || id != apiIdentity() {
			t.Fatalf("Resolve at %v = (%+v, %s), want payment/api RESOLVED", tLater, id, out)
		}
	})

	// 反方向：不可信的那次运行不得把更准的归属覆盖成退化的那个。
	t.Run("a degraded run never overwrites a resolved label", func(t *testing.T) {
		got := identity.Derive([]identity.Interval{openAPIInterval()},
			pod(degradedAPIIdentity()), partialRun("run-2", tLater))
		if len(got) != 1 {
			t.Fatalf("a degraded run forked the pod into %d intervals, want 1: %+v", len(got), got)
		}
		if got[0].Identity != apiIdentity() {
			t.Fatalf("workload = %s/%s, want Deployment/api unchanged: a run that could not read "+
				"ReplicaSets must not write its degraded answer over a better one",
				got[0].Identity.WorkloadKind, got[0].Identity.WorkloadName)
		}
		if !got[0].Open() || got[0].LastRunID != "run-2" {
			t.Fatalf("interval = %+v, want it still open and last seen by run-2", got[0])
		}
	})
}

// TestDeriveNeverClosesAnotherClustersInterval —— 不同集群 Pod CIDR 可能重叠，
// 让一个集群的运行去关另一个集群的区间，正是「join 到错误的 Pod 上且不报错」。
func TestDeriveNeverClosesAnotherClustersInterval(t *testing.T) {
	other := openAPIInterval()
	other.ClusterID = "cluster-b"

	got := identity.Derive([]identity.Interval{other}, nil, trustworthyRun())
	if len(got) != 1 {
		t.Fatalf("Derive returned %d intervals, want 1", len(got))
	}
	if !got[0].Open() {
		t.Fatalf("a run on cluster-a closed cluster-b's interval at %v", got[0].ValidTo)
	}
}

// TestDeriveNeverClosesAnIntervalThatBeganAfterTheRun —— 并发与补跑会让推导
// 拿到一次比现有区间还老的运行；关掉它会得到一个 ValidTo 不晚于 ValidFrom
// 的区间，那是一段谁也解释不了的时间。
func TestDeriveNeverClosesAnIntervalThatBeganAfterTheRun(t *testing.T) {
	late := openAPIInterval()
	late.ValidFrom = tLater.Add(time.Minute)

	got := identity.Derive([]identity.Interval{late}, nil, trustworthyRun())
	if len(got) != 1 {
		t.Fatalf("Derive returned %d intervals, want 1", len(got))
	}
	if !got[0].Open() {
		t.Fatalf("closed an interval that began at %v with a run observed at %v (valid_to = %v)",
			late.ValidFrom, tLater, got[0].ValidTo)
	}
}
