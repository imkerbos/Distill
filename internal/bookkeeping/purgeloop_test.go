package bookkeeping_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/bookkeeping"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

// fakePurger 记下被要求删了什么、水位推到哪。
type fakePurger struct {
	remaining int
	batch     int
	before    time.Time
	advanced  []time.Time
	failAfter int
	calls     int
}

func (p *fakePurger) PurgeConnectionsBefore(
	_ context.Context, _ string, before time.Time,
) (int, error) {
	p.calls++
	p.before = before
	if p.failAfter > 0 && p.calls >= p.failAfter {
		return 0, errors.New("boom")
	}
	n := p.batch
	if n > p.remaining {
		n = p.remaining
	}
	p.remaining -= n
	return n, nil
}

func (p *fakePurger) AdvanceRetainedFrom(_ context.Context, _ string, at time.Time) error {
	p.advanced = append(p.advanced, at)
	return nil
}

// **删干净了才推进水位。**
func TestTheWatermarkAdvancesOnlyAfterTheBacklogIsGone(t *testing.T) {
	p := &fakePurger{remaining: 30, batch: 10}
	runPurge(t, p, purgeAccounted, 24*time.Hour)
	if len(p.advanced) != 1 {
		t.Fatalf("水位推进了 %d 次，want 1", len(p.advanced))
	}
	if p.remaining != 0 {
		t.Errorf("还剩 %d 行没删", p.remaining)
	}
}

// **删失败时水位不动。** 那段数据还在，说它没了就是在编造事实：
// 读取端会据此拒绝一次本来答得出的查询。
func TestAFailedPurgeLeavesTheWatermarkPutt(t *testing.T) {
	p := &fakePurger{remaining: 100, batch: 10, failAfter: 2}
	runPurge(t, p, purgeAccounted, 24*time.Hour)
	if len(p.advanced) != 0 {
		t.Errorf("删失败却推进了水位 %v —— 那段数据还在", p.advanced)
	}
}

// **一轮删不完就不推进水位**，下一轮从同一个位置接着删。
func TestAnUnfinishedRoundDoesNotAdvanceTheWatermark(t *testing.T) {
	p := &fakePurger{remaining: 1_000_000, batch: 10}
	runPurge(t, p, purgeAccounted, 24*time.Hour)
	if len(p.advanced) != 0 {
		t.Errorf("没删完却推进了水位 %v", p.advanced)
	}
	if p.calls == 0 {
		t.Error("一批都没删")
	}
}

// **记账落后时，水位跟着记账走。**
// 这是整套机制的头一条：还没汇成 rule_evidence 的连接删掉就永久没了。
func TestThePurgeNeverPassesTheAccountingWatermark(t *testing.T) {
	p := &fakePurger{remaining: 5, batch: 10}
	stalled := purgeNow.Add(-13 * time.Hour)
	runPurge(t, p, stalled, time.Hour)
	if !p.before.Equal(stalled) {
		t.Errorf("删到 %s，而记账只记到 %s —— 越过记账等于删掉还没汇总的证据",
			p.before, stalled)
	}
}

// 没接 purger 的部署一行都不删：清理是删数据，不该在升级时悄悄开始跑。
func TestNoPurgerMeansNoDeletion(t *testing.T) {
	a := newPurgeAccountant(t, nil, 24*time.Hour, purgeAccounted)
	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once() error = %v", err)
	}
}

var (
	purgeNow       = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	purgeAccounted = purgeNow
)

func runPurge(t *testing.T, p *fakePurger, accounted time.Time, retention time.Duration) {
	t.Helper()
	a := newPurgeAccountant(t, p, retention, accounted)
	if err := a.Once(context.Background()); err != nil {
		t.Fatalf("Once() error = %v", err)
	}
}

func retentionOf(d time.Duration) func(registry.Cluster) time.Duration {
	return func(registry.Cluster) time.Duration { return d }
}

// newPurgeAccountant 造一个只做清理的调度器：记账本身用一件已经记到
// accounted 的空任务，这样这组用例验的只是清理那一段。
func newPurgeAccountant(
	t *testing.T, p *fakePurger, retention time.Duration, accounted time.Time,
) bookkeeping.Accountant {
	t.Helper()
	from, to := purgeNow.Add(-time.Minute), purgeNow
	s := &fakeStore{lastEnd: map[string]time.Time{"c1": accounted}}
	a := bookkeeping.NewAccountant(
		fakeClusters{{ID: "c1", DataSource: registry.DataSourceCollected}},
		fakeWindows{windows: map[string]store.TimeWindow{"c1": {From: from, To: to}}},
		testLogger(t),
		bookkeeping.Task{Name: "evidence",
			Recorder: bookkeeping.NewRecorder(
				&fakePreviewer{preview: previewWith(flow.CompletenessUnknown)}, s, testLogger(t)),
			Accounted: s})
	if p == nil {
		return a
	}
	return a.WithPurge(p, retentionOf(retention)).WithClock(func() time.Time { return purgeNow })
}
