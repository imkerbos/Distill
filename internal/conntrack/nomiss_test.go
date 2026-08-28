package conntrack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/conntrack"
)

// good 是一组全部通过的证据。每个用例只破坏其中一条，因此哪一条把结论
// 翻过去是明确的。
func good() conntrack.Coverage {
	return conntrack.Coverage{
		PollInterval:          5 * time.Second,
		ShortestEntryLifetime: 10 * time.Second,
		PollsPlanned:          12,
		PollsSucceeded:        12,
		DropsDuringWindow:     0,
		TableCount:            4000,
		TableMax:              131072,
	}
}

func TestProvesNoMissAcceptsAFullyEvidencedWindow(t *testing.T) {
	ok, why := good().ProvesNoMiss()
	if !ok {
		t.Fatalf("三项证据齐全却没通过: %s", why)
	}
}

// **零值必须是"证明不了"。** 一个没被赋过值的 Coverage 绝不能读作完整——
// 那正是"完整度不是一个可以被填写的字段"这条纪律在这里的落点。
func TestProvesNoMissRejectsTheZeroValue(t *testing.T) {
	if ok, _ := (conntrack.Coverage{}).ProvesNoMiss(); ok {
		t.Fatal("零值 Coverage 被判成了没漏")
	}
}

func TestProvesNoMissNamesTheOneConditionThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*conntrack.Coverage)
		want   string
	}{
		{"存活时间不足两倍间隔", func(c *conntrack.Coverage) {
			c.ShortestEntryLifetime = 9 * time.Second
		}, "不足轮询间隔"},
		{"读不到超时配置", func(c *conntrack.Coverage) {
			c.ShortestEntryLifetime = 0
		}, "读不到内核"},
		{"有一次轮询失败", func(c *conntrack.Coverage) {
			c.PollsSucceeded = 11
		}, "没成功"},
		{"窗口被提前结束", func(c *conntrack.Coverage) {
			c.CutShort = true
		}, "提前结束"},
		{"读表撞上限", func(c *conntrack.Coverage) {
			c.Truncated = true
		}, "条数上限"},
		{"窗口内有丢弃", func(c *conntrack.Coverage) {
			c.DropsDuringWindow = 1
		}, "丢弃了 1 条"},
		{"表用量过线", func(c *conntrack.Coverage) {
			c.TableCount = 120000
		}, "已过 90%"},
		{"读不到表容量", func(c *conntrack.Coverage) {
			c.TableMax = 0
		}, "读不到 conntrack 表容量"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mutate(&c)
			ok, why := c.ProvesNoMiss()
			if ok {
				t.Fatalf("%s 却仍然判成没漏", tc.name)
			}
			if !strings.Contains(why, tc.want) {
				t.Errorf("理由 %q 里没有 %q —— 说不出是哪一条没过就没法去修", why, tc.want)
			}
		})
	}
}

// 边界：恰好两倍要过，差一纳秒就不过。这条线是整个判据的核心，不能靠
// "差不多"。
func TestProvesNoMissIsExactAtTheMargin(t *testing.T) {
	c := good()
	c.PollInterval = 5 * time.Second
	c.ShortestEntryLifetime = 10 * time.Second
	if ok, why := c.ProvesNoMiss(); !ok {
		t.Errorf("恰好两倍应当通过: %s", why)
	}
	c.ShortestEntryLifetime = 10*time.Second - time.Nanosecond
	if ok, _ := c.ProvesNoMiss(); ok {
		t.Error("差一纳秒不足两倍，却通过了")
	}
}
