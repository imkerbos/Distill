package identity_test

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/identity"
)

// closedAPIInterval 是 10.4.1.7 归 payment/api 的一段已关闭区间 [tSeen, tLater)。
func closedAPIInterval() identity.Interval {
	iv := openAPIInterval()
	iv.ValidTo = tLater
	iv.LastRunID = "run-2"
	iv.ClosedReason = identity.ClosedAbsentInLaterRun
	return iv
}

// laterWorkerInterval 是同一个 IP 在一段空档之后归 batch/worker 的区间。
func laterWorkerInterval() identity.Interval {
	return identity.Interval{
		ClusterID:    "cluster-a",
		PodIP:        "10.4.1.7",
		ValidFrom:    tLater.Add(10 * time.Minute),
		ValidTo:      tLater.Add(20 * time.Minute),
		Identity:     workerIdentity(),
		FirstRunID:   "run-4",
		LastRunID:    "run-6",
		ClosedReason: identity.ClosedAbsentInLaterRun,
	}
}

func TestResolveReturnsTheCoveringIdentity(t *testing.T) {
	ivs := []identity.Interval{closedAPIInterval()}

	cases := []struct {
		name string
		at   time.Time
		want identity.Outcome
	}{
		{"at the opening moment", tSeen, identity.OutcomeResolved},
		{"inside", tSeen.Add(time.Minute), identity.OutcomeResolved},
		// 半开区间：关闭时刻已经不属于它。那一刻有一次可信运行看到这个 IP
		// 空着，所以是 NOT_COVERED 而不是 NO_DATA。
		{"at the closing moment", tLater, identity.OutcomeNotCovered},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, out := identity.Resolve(ivs, c.at)
			if out != c.want {
				t.Fatalf("outcome = %s, want %s", out, c.want)
			}
			if c.want == identity.OutcomeResolved && id != apiIdentity() {
				t.Fatalf("identity = %+v, want %+v", id, apiIdentity())
			}
			if c.want != identity.OutcomeResolved && id != (identity.Identity{}) {
				t.Fatalf("identity = %+v, want zero on %s", id, out)
			}
		})
	}
}

// TestNotCoveredIsNotTheSameAsNoData —— 前者是「那一刻这个 IP 没有 Pod」，
// 后者是「平台那段时间没在看」。合成一个，会让「我们没数据」被读成
// 「那时确实没有 Pod」，而后者是下游会当作事实使用的结论。
func TestNotCoveredIsNotTheSameAsNoData(t *testing.T) {
	// 取值本身必须不同：只断言行为的话，把两个常量改成同一个字符串，
	// 下面每一条断言仍然全绿。
	if identity.OutcomeNotCovered == identity.OutcomeNoData {
		t.Fatalf("NOT_COVERED and NO_DATA share the value %q; collapsed together, "+
			"\"we have no data\" reads as \"there was genuinely no pod\"", identity.OutcomeNotCovered)
	}

	ivs := []identity.Interval{closedAPIInterval(), laterWorkerInterval()}

	cases := []struct {
		name string
		at   time.Time
		want identity.Outcome
	}{
		{"inside the gap between two intervals", tLater.Add(5 * time.Minute), identity.OutcomeNotCovered},
		{"before anything was ever observed", tSeen.Add(-time.Hour), identity.OutcomeNoData},
		{"after the last observation", tLater.Add(21 * time.Minute), identity.OutcomeNoData},
		{"no intervals at all", tSeen, identity.OutcomeNoData},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := ivs
			if c.name == "no intervals at all" {
				in = nil
			}
			id, out := identity.Resolve(in, c.at)
			if out != c.want {
				t.Fatalf("outcome = %s, want %s", out, c.want)
			}
			if id != (identity.Identity{}) {
				t.Fatalf("identity = %+v, want zero on %s", id, out)
			}
		})
	}
}

// TestResolveNeverFallsBackToTheNearestInterval —— 一个 5 分钟前结束的区间
// 对当前时刻没有解释力；返回它等于用当前状态解释历史数据的镜像错误。
func TestResolveNeverFallsBackToTheNearestInterval(t *testing.T) {
	cases := []struct {
		name string
		ivs  []identity.Interval
		at   time.Time
		want identity.Outcome
	}{
		{
			name: "five minutes after the only interval ended",
			ivs:  []identity.Interval{closedAPIInterval()},
			at:   tLater.Add(5 * time.Minute),
			want: identity.OutcomeNoData,
		},
		{
			name: "in the gap, with a neighbour on each side",
			ivs:  []identity.Interval{closedAPIInterval(), laterWorkerInterval()},
			at:   tLater.Add(2 * time.Minute),
			want: identity.OutcomeNotCovered,
		},
		{
			name: "before the first interval began",
			ivs:  []identity.Interval{closedAPIInterval()},
			at:   tSeen.Add(-time.Minute),
			want: identity.OutcomeNoData,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, out := identity.Resolve(c.ivs, c.at)
			if out == identity.OutcomeResolved {
				t.Fatalf("resolved to %+v at %v, where no interval covers that moment", id, c.at)
			}
			if id != (identity.Identity{}) {
				t.Fatalf("identity = %+v, want zero — the nearest interval explains nothing about %v", id, c.at)
			}
			if out != c.want {
				t.Fatalf("outcome = %s, want %s", out, c.want)
			}
		})
	}
}

// TestOverlappingIntervalsAreAmbiguous —— 同一 IP 同一时刻属于两个 Pod 在
// 正常集群里不可能，出现即意味着采集乱序、时钟漂移或回收快于采集间隔。
// 任选一个仍然能查出结果、仍然不报错，而错的那次没有任何症状。
func TestOverlappingIntervalsAreAmbiguous(t *testing.T) {
	overlapping := laterWorkerInterval()
	overlapping.ValidFrom = tSeen.Add(time.Minute)
	overlapping.ValidTo = tLater

	id, out := identity.Resolve([]identity.Interval{closedAPIInterval(), overlapping}, tSeen.Add(2*time.Minute))
	if out != identity.OutcomeAmbiguous {
		t.Fatalf("outcome = %s, want %s", out, identity.OutcomeAmbiguous)
	}
	if id != (identity.Identity{}) {
		t.Fatalf("identity = %+v, want zero — AMBIGUOUS must not pick one of the two", id)
	}
}

func TestOutcomeValidRejectsUnregisteredValues(t *testing.T) {
	for _, o := range []identity.Outcome{
		identity.OutcomeResolved, identity.OutcomeAmbiguous,
		identity.OutcomeNotCovered, identity.OutcomeNoData,
	} {
		if !o.Valid() {
			t.Fatalf("Valid(%q) = false, want true", o)
		}
	}
	for _, o := range []identity.Outcome{"", "RESOLVE", "resolved", "UNKNOWN"} {
		if o.Valid() {
			t.Fatalf("Valid(%q) = true, want false", o)
		}
	}
}

// hostNetIV 造一段 hostNetwork Pod 的区间。它们共用节点 IP。
func hostNetIV(name string) identity.Interval {
	return identity.Interval{
		ClusterID: "cluster-a",
		PodIP:     "10.170.48.99",
		ValidFrom: tSeen,
		Identity: identity.Identity{
			Namespace: "kube-system", PodName: name, PodUID: "uid-" + name,
			WorkloadKind: "DaemonSet", WorkloadName: name,
			HostNetwork: true,
		},
		FirstRunID: "run-1",
	}
}

// 多个 hostNetwork Pod 共用节点 IP，不是归属歧义。
//
// 这条来自真集群：一个节点上跑着 10 个 hostNetwork 的 DaemonSet Pod
// （calico-node、kube-proxy、各种 agent），全都用节点 IP。按 AMBIGUOUS
// 处理会把这件事报成「我不知道它是谁」，而真相是一个确定的结论 ——
// 这一端不在 Pod 网络里，NetworkPolicy 选不中它。
//
// 实测那个集群 26.3% 的 UNKNOWN 里，这一类占了绝大部分。把确定的事实
// 计进「无法判定」，会让那个比例失去意义。
func TestResolveTreatsSharedNodeIPAsHostNetworkNotAmbiguous(t *testing.T) {
	ivs := []identity.Interval{
		hostNetIV("calico-node-abc"), hostNetIV("kube-proxy-def"), hostNetIV("distill-agent-ghi"),
	}
	got, outcome := identity.Resolve(ivs, tSeen.Add(time.Minute))

	if outcome != identity.OutcomeHostNetwork {
		t.Errorf("Outcome = %q, want HOST_NETWORK —— 共用节点 IP 是常态，不是歧义", outcome)
	}
	// 具体是哪一个 Pod 既答不出也不重要：返回其中一个仍然能查出结果、
	// 仍然不报错，而错的那次没有任何症状（同 AMBIGUOUS 的理由）。
	if got.PodName != "" {
		t.Errorf("PodName = %q, want empty —— 多个共用时不该挑一个", got.PodName)
	}
}

// 只跑着一个 hostNetwork Pod 的节点 IP，同样是 HOST_NETWORK。
//
// 下游关心的是「这一端受不受策略管控」，而答案与有几个区间无关。
// 两处给出不同的 Outcome，会让同一个事实在节点上只有一个 hostNetwork Pod
// 时表现成另一回事。
func TestResolveReportsHostNetworkEvenForASingleInterval(t *testing.T) {
	got, outcome := identity.Resolve([]identity.Interval{hostNetIV("solo")}, tSeen.Add(time.Minute))

	if outcome != identity.OutcomeHostNetwork {
		t.Errorf("Outcome = %q, want HOST_NETWORK", outcome)
	}
	// 这一支解得出具体是谁，照常给出来：界面上一条通往特权组件的连接
	// 应当看得见是哪个组件。
	if got.PodName != "solo" {
		t.Errorf("PodName = %q, want solo —— 唯一覆盖时身份是确定的", got.PodName)
	}
}

// 混合仍然是真歧义。
//
// 既有 hostNetwork 又有普通 Pod 时，确实答不出这条连接的另一端是不是
// 那个受管控的。放宽到 HOST_NETWORK 会把一个真正不知道的事说成确定结论。
func TestResolveKeepsAmbiguousWhenIntervalsAreMixed(t *testing.T) {
	mixed := hostNetIV("calico-node-abc")
	normal := mixed
	normal.Identity = identity.Identity{
		Namespace: "payment", PodName: "api-1", PodUID: "uid-api",
		WorkloadKind: "Deployment", WorkloadName: "api",
	}
	_, outcome := identity.Resolve([]identity.Interval{mixed, normal}, tSeen.Add(time.Minute))

	if outcome != identity.OutcomeAmbiguous {
		t.Errorf("Outcome = %q, want AMBIGUOUS —— 混合时确实归属不唯一", outcome)
	}
}

// 多个普通 Pod 覆盖同一时刻，仍然是歧义：那是真正的 IP 复用。
func TestResolveKeepsAmbiguousForRealIPReuse(t *testing.T) {
	a := hostNetIV("a")
	a.Identity.HostNetwork = false
	b := a
	b.Identity.PodName = "b"
	_, outcome := identity.Resolve([]identity.Interval{a, b}, tSeen.Add(time.Minute))

	if outcome != identity.OutcomeAmbiguous {
		t.Errorf("Outcome = %q, want AMBIGUOUS", outcome)
	}
}

// HOST_NETWORK 必须登记进封闭枚举。
func TestHostNetworkOutcomeIsRegistered(t *testing.T) {
	if !identity.OutcomeHostNetwork.Valid() {
		t.Error("HOST_NETWORK 不在已登记的 Outcome 里")
	}
}
