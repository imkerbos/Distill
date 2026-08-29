package store_test

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/store"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// 记账跟得上摄入时不算停摆。
func TestEvidenceLagIsFreshWhenAccountingKeepsUp(t *testing.T) {
	l := store.EvidenceLag{
		AccountedTo: ts("2026-08-29T13:57:00Z"),
		IngestedTo:  ts("2026-08-29T13:58:00Z"),
	}
	if l.Stale() {
		t.Errorf("落后 1 分钟被判成停摆，落后 %s", l.Behind())
	}
}

// **这是 2026-08-29 那次事故的形状。** 摄入一直在走，记账停在 13 小时前。
func TestEvidenceLagCatchesAStalledRecorder(t *testing.T) {
	l := store.EvidenceLag{
		AccountedTo: ts("2026-08-29T00:06:56Z"),
		IngestedTo:  ts("2026-08-29T13:21:00Z"),
	}
	if !l.Stale() {
		t.Fatal("记账停了 13 小时却没判成停摆 —— 这正是那次没人发现的原因")
	}
	if got := l.Behind(); got < 13*time.Hour {
		t.Errorf("落后 %s，want ≥13h", got)
	}
}

// **还没有摄入的集群不算停摆。**
//
// 那时证据不更新是因为没有东西可记，与"记账坏了"是两件事。混成一个，
// 一个刚接入、还没跑起流量的集群会一上来就报一条不存在的故障——而报错
// 太灵敏的下场是被忽略，那会连真的那次一起淹掉。
func TestEvidenceLagIsNotStaleBeforeAnyIngest(t *testing.T) {
	if (store.EvidenceLag{}).Stale() {
		t.Error("一个还没有流量的集群被报成记账停摆")
	}
	l := store.EvidenceLag{AccountedTo: ts("2026-08-29T13:00:00Z")}
	if l.Stale() || l.Behind() != 0 {
		t.Errorf("摄入还没开始时不该有落后: stale=%v behind=%s", l.Stale(), l.Behind())
	}
}

// 摄入在走而一次都没记过，是停摆的一种——不是"刚开始"。
func TestEvidenceLagCatchesNeverAccounted(t *testing.T) {
	l := store.EvidenceLag{IngestedTo: ts("2026-08-29T13:21:00Z")}
	if !l.Stale() {
		t.Error("摄入已经在走、证据一条没记过，却没判成停摆")
	}
}

// 记账跑在摄入前面（补记一段更晚的窗口）不算落后，也不许出现负数。
func TestEvidenceLagNeverGoesNegative(t *testing.T) {
	l := store.EvidenceLag{
		AccountedTo: ts("2026-08-29T14:00:00Z"),
		IngestedTo:  ts("2026-08-29T13:00:00Z"),
	}
	if l.Behind() != 0 {
		t.Errorf("落后 %s —— 记账跑在前面时不该是负数", l.Behind())
	}
	if l.Stale() {
		t.Error("记账跑在摄入前面被判成停摆")
	}
}

// 阈值边界：刚好 15 分钟不算，超过才算。偶尔慢一拍不该报警——
// 报警太灵敏的下场是被忽略。
func TestEvidenceLagToleratesOneMissedCycle(t *testing.T) {
	base := ts("2026-08-29T13:00:00Z")
	for _, tc := range []struct {
		lag  time.Duration
		want bool
	}{
		{5 * time.Minute, false},
		{15 * time.Minute, false},
		{15*time.Minute + time.Second, true},
		{2 * time.Hour, true},
	} {
		l := store.EvidenceLag{AccountedTo: base, IngestedTo: base.Add(tc.lag)}
		if got := l.Stale(); got != tc.want {
			t.Errorf("落后 %s: Stale() = %v, want %v", tc.lag, got, tc.want)
		}
	}
}
