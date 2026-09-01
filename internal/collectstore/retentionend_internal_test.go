package collectstore

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
)

var retentionAnchor = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// **没有流量窗口时，累积规则的回溯起点取锚点，不取零值。**
//
// 零值减去保留期是公元前，MySQL 直接拒掉整条查询
// （"year is not in the range [1, 9999]: 0"），而调用它的是 PolicyPreview ——
// 于是一个采到了资产、还没有流量的集群连一份纯 Baseline 的候选策略都拿不到。
// 那正是操作者问「那你推荐我加什么策略」时要看的那一屏。
func TestRetentionStartFallsBackToTheAnchorWithoutATrafficWindow(t *testing.T) {
	const retention = 7 * 24 * time.Hour
	got, ok := retentionStart(flow.Window{}, retentionAnchor, retention)
	if !ok {
		t.Fatal("有锚点却算不出回溯起点")
	}
	if want := retentionAnchor.Add(-retention); !got.Equal(want) {
		t.Errorf("起点 = %s, want %s", got, want)
	}
	if got.Year() < 1 {
		t.Errorf("起点年份 %d 落在合法范围之外 —— 查询会被数据库整条拒掉", got.Year())
	}
}

// 有流量窗口时按窗口末端往回算，不按当下：回看一段很久以前的窗口时，
// 按当下算会把那之后学到的规则一并并进去，而那是拿现在解释过去。
func TestRetentionStartCountsBackFromTheWindowEnd(t *testing.T) {
	const retention = 7 * 24 * time.Hour
	to := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	got, ok := retentionStart(flow.Window{From: to.Add(-time.Hour), To: to}, retentionAnchor, retention)
	if !ok {
		t.Fatal("有窗口却算不出回溯起点")
	}
	if want := to.Add(-retention); !got.Equal(want) {
		t.Errorf("起点 = %s, want %s —— 用锚点算等于拿现在解释过去", got, want)
	}
}

// 窗口与锚点都没有：没有任何时刻可以往回算。返回 false 让调用方跳过这次
// 读取，而不是拿一个公元 0 年的时间去查库。
func TestRetentionStartRefusesWithNeitherWindowNorAnchor(t *testing.T) {
	if _, ok := retentionStart(flow.Window{}, time.Time{}, time.Hour); ok {
		t.Error("没有任何时刻可依据却算出了起点")
	}
}
