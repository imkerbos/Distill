package conntrack_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/conntrack"
)

const statsFile = `entries  clashres found new invalid ignore delete delete_list insert insert_failed drop early_drop icmp_error expect_new expect_create expect_delete search_restart
00000cc4  00000000 0000000a 00000000 00000003 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000
00000cc4  00000000 00000005 00000000 00000001 00000000 00000000 00000000 00000000 00000002 00000003 00000004 00000000 00000000 00000000 00000000 00000000
`

func TestParseStatsSumsEveryCPURow(t *testing.T) {
	got, err := conntrack.ParseStats(strings.NewReader(statsFile))
	if err != nil {
		t.Fatalf("ParseStats() error = %v", err)
	}
	if got.Drop != 3 || got.EarlyDrop != 4 || got.InsertFailed != 2 {
		t.Fatalf("got %+v, want drop=3 early_drop=4 insert_failed=2", got)
	}
	if got.Total() != 9 {
		t.Errorf("Total() = %d, want 9", got.Total())
	}
}

// 列按名字定位。内核版本之间这个文件增删过列，按下标取值在列变动之后不会
// 报错，只会把另一列的数字当成丢弃数——而那是朝"没有丢弃"方向错的读数。
func TestParseStatsLocatesColumnsByName(t *testing.T) {
	// 把 delete_list 那一列去掉（5.x 之后的形状），drop/early_drop 因此左移。
	shifted := `entries clashres found new invalid ignore delete insert insert_failed drop early_drop
00000cc4 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000002 00000003 00000004
`
	got, err := conntrack.ParseStats(strings.NewReader(shifted))
	if err != nil {
		t.Fatalf("ParseStats() error = %v", err)
	}
	if got.Drop != 3 || got.EarlyDrop != 4 || got.InsertFailed != 2 {
		t.Errorf("列少了一个之后读错了: %+v", got)
	}
}

// 认不出的形状要报错，**不能返回零值**：零丢弃是"证明了没丢"，
// 而读不懂这个文件是"不知道丢没丢"。
func TestParseStatsRefusesAnUnrecognisedShape(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"没有表头", ""},
		{"缺 early_drop 列", "entries drop insert_failed\n00000001 00000000 00000000\n"},
		{"只有表头没有数据行", "entries drop early_drop insert_failed\n"},
		{"数值不是十六进制", "entries drop early_drop insert_failed\n1 zz 0 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := conntrack.ParseStats(strings.NewReader(tc.in)); err == nil {
				t.Error("认不出的形状被当成了一份可信的零丢弃")
			}
		})
	}
}

func TestParseTimeoutSecondsRejectsNonPositive(t *testing.T) {
	if d, err := conntrack.ParseTimeoutSeconds(" 10 \n"); err != nil || d.Seconds() != 10 {
		t.Fatalf("got %v %v, want 10s", d, err)
	}
	for _, in := range []string{"", "0", "-5", "abc"} {
		if _, err := conntrack.ParseTimeoutSeconds(in); err == nil {
			t.Errorf("%q 应当被拒 —— 一个读不出的超时不得当成足够长", in)
		}
	}
}
