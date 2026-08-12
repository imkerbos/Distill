package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/store"
)

func TestTimeWindowValid(t *testing.T) {
	t0 := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		w    store.TimeWindow
		want bool
	}{
		{"零值", store.TimeWindow{}, false},
		{"缺 From", store.TimeWindow{To: t0}, false},
		{"缺 To", store.TimeWindow{From: t0}, false},
		{"From 等于 To", store.TimeWindow{From: t0, To: t0}, false},
		{"From 晚于 To", store.TimeWindow{From: t0.Add(time.Hour), To: t0}, false},
		{"正常", store.TimeWindow{From: t0, To: t0.Add(time.Hour)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.w.Valid(); got != c.want {
				t.Errorf("Valid() = %v, want %v", got, c.want)
			}
		})
	}
}

// 半开区间：起点算在内，终点不算。两端都算会让相邻窗口重复计入同一条 flow，
// 而对账的分母正是靠窗口切分累加出来的。
func TestTimeWindowContainsIsHalfOpen(t *testing.T) {
	t0 := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	w := store.TimeWindow{From: t0, To: t0.Add(time.Minute)}

	if !w.Contains(t0) {
		t.Error("起点应包含在窗口内")
	}
	if w.Contains(t0.Add(time.Minute)) {
		t.Error("终点不应包含在窗口内")
	}
	if w.Contains(t0.Add(-time.Second)) {
		t.Error("起点之前不应包含")
	}
}

// 缺失窗口必须报错，而不是安静地返回全部。spec §5.1 要求事实层
// require_partition_filter = true —— 一个没带时间条件却照样返回结果的查询，
// 接上真实存储时会变成一次全表扫描，而账单要到月底才可见。
func TestFlowsRejectsMissingWindow(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load(), fixtureSource())

	_, err := r.Flows(context.Background(), store.FlowFilter{})
	if err == nil {
		t.Fatal("缺失时间窗时应返回错误，实际返回 nil")
	}
	if !errors.Is(err, store.ErrWindowRequired) {
		t.Errorf("err = %v, want %v", err, store.ErrWindowRequired)
	}
}

func TestFlowsFiltersByWindow(t *testing.T) {
	f := fixture.Load()
	r := store.NewFixtureReader(f, fixtureSource())
	full := r.DataWindow()

	all, err := r.Flows(context.Background(), store.FlowFilter{Window: full})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if all.Total != len(f.Flows) {
		t.Fatalf("全窗口 Total = %d, want %d", all.Total, len(f.Flows))
	}

	// 取前十秒：fixture 每条 flow 间隔一秒，因此应恰好落进十条。
	narrow := store.TimeWindow{From: full.From, To: full.From.Add(10 * time.Second)}
	got, err := r.Flows(context.Background(), store.FlowFilter{Window: narrow})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if got.Total != 10 {
		t.Errorf("十秒窗口 Total = %d, want 10", got.Total)
	}
	for _, it := range got.Items {
		if !narrow.Contains(it.Timestamp) {
			t.Errorf("flow %s 的时间戳 %v 落在窗口 %v 之外", it.ID, it.Timestamp, narrow)
		}
	}
}

// 窗口必须回显。一个按时间筛过的列表若不告知筛的是哪段，界面无法把它
// 与全量列表区分开 —— 与 §17.3 "截断必可见" 是同一条要求。
func TestFlowsEchoesEffectiveWindow(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load(), fixtureSource())
	w := store.TimeWindow{
		From: r.DataWindow().From,
		To:   r.DataWindow().From.Add(time.Minute),
	}

	page, err := r.Flows(context.Background(), store.FlowFilter{Window: w})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if !page.Window.From.Equal(w.From) || !page.Window.To.Equal(w.To) {
		t.Errorf("回显窗口 = %v, want %v", page.Window, w)
	}
}

// DataWindow 必须真正覆盖数据集，否则以它为默认窗口的装配方
// 会在没人改代码的情况下漏掉边界上的 flow。
func TestDataWindowCoversAllFlows(t *testing.T) {
	f := fixture.Load()
	w := store.NewFixtureReader(f, fixtureSource()).DataWindow()

	for _, fl := range f.Flows {
		if !w.Contains(fl.Flow.Timestamp) {
			t.Fatalf("flow %s (%v) 落在 fixture.Window() %v 之外", fl.ID, fl.Flow.Timestamp, w)
		}
	}
}
