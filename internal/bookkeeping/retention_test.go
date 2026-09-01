package bookkeeping_test

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/bookkeeping"
)

var retNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// **记账停摆时水位不前进。**
//
// 只看时钟的话，记账坏掉的那段时间里连接照旧被删，而它们还没被汇成
// rule_evidence —— 证据永久没了，候选策略里那条放行再也产生不出来，
// 而症状是策略集少一条规则，不报任何错。2026-08-29 记账停过 13 小时。
func TestTheWatermarkStopsWhenAccountingStops(t *testing.T) {
	stalled := retNow.Add(-13 * time.Hour)
	got, ok := bookkeeping.RetentionWatermark(retNow, stalled, 2*time.Hour)
	if !ok {
		t.Fatal("记账落后就完全不删了 —— 那会让表无限涨")
	}
	if !got.Equal(stalled) {
		t.Errorf("水位 = %s, want %s —— 不得越过记账记到的地方", got, stalled)
	}
}

// 记账跟得上时，水位由可查询期决定：刚摄入的连接还要给预览、流量列表
// 与写回读。
func TestTheWatermarkKeepsTheQueryableWindow(t *testing.T) {
	got, ok := bookkeeping.RetentionWatermark(retNow, retNow, 48*time.Hour)
	if !ok {
		t.Fatal("want a watermark")
	}
	if want := retNow.Add(-48 * time.Hour); !got.Equal(want) {
		t.Errorf("水位 = %s, want %s —— 可查询期内的连接不得删", got, want)
	}
}

// **一次都没记过时不删任何东西。**
// "记账记到哪"还没有答案，而没有答案不等于答案是"记到现在"。
func TestNothingIsPurgedBeforeTheFirstAccounting(t *testing.T) {
	if _, ok := bookkeeping.RetentionWatermark(retNow, time.Time{}, time.Hour); ok {
		t.Error("一次都没记账过却算出了水位 —— 那会删掉全部还没汇总的证据")
	}
}

// 可查询期说不出来时同样不删：让一次配置遗漏变成一次数据销毁是最坏的方向。
func TestNothingIsPurgedWithoutARetention(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Hour} {
		if _, ok := bookkeeping.RetentionWatermark(retNow, retNow, d); ok {
			t.Errorf("保留期 %s 却算出了水位", d)
		}
	}
}
