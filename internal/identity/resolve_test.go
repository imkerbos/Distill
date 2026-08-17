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
