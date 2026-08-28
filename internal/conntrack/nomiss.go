package conntrack

import (
	"fmt"
	"time"
)

// Coverage 是一次轮询窗口的三项证据，用来回答一个问题：这个窗口里，有没有
// 一条连接可能出现过又消失了、而我们一次都没看见？
//
// **默认答案是"可能有"。** conntrack 是当前被跟踪连接的快照、不是日志，
// 所以"没漏"这句话必须被证明，不能被假定。这个类型存在的意义就是把那份
// 证明写成一组可以逐条检验的事实——而不是一个可以被填成 true 的字段。
type Coverage struct {
	// PollInterval 是两次轮询之间的间隔。
	PollInterval time.Duration
	// ShortestEntryLifetime 是内核配置里最短的表项存活时间。一条连接进表
	// 之后至少活这么久，因此只要它 >= 轮询间隔，任何一条连接都必然落在
	// 至少一次轮询的视野里。
	ShortestEntryLifetime time.Duration
	// PollsPlanned / PollsSucceeded 是计划与实际成功的轮询次数。
	PollsPlanned   int
	PollsSucceeded int
	// CutShort 表示窗口被上下文提前结束，没跑满计划的轮询。
	CutShort bool
	// DropsDuringWindow 是窗口内新增的丢弃计数（窗口末减窗口初）。
	DropsDuringWindow uint64
	// Truncated 表示这一轮读表时撞到了条数上限。
	Truncated bool
	// TableCount / TableMax 是窗口末的表用量。
	TableCount, TableMax int
}

// lifetimeMargin 是存活时间相对轮询间隔要求的倍数。
//
// 严格地说 1 倍就够：表项活 L、每 I 秒看一次，L >= I 时任何一条连接都至少
// 被看见一次。要 2 倍是给时钟抖动、读表本身的耗时、以及轮询被调度延迟留的
// 余量——这些都会让实际间隔大于名义间隔，而一旦实际间隔超过 L，漏掉的连接
// 不会有任何迹象。
const lifetimeMargin = 2

// tableHeadroom 是表用量的上限。接近满表时内核会 early_drop 老条目，而那
// 恰好发生在流量最大、最不该漏的时候。
const tableHeadroom = 0.9

// ProvesNoMiss 报告这组证据是否足以证明"这个窗口一条都没漏"，
// 以及否的时候是**哪一条**没过。
//
// 返回原因而不只是 false：一个说不出理由的"不完整"没法让人去修。这几条
// 各自对应一个具体的动作——调轮询间隔、调 conntrack 超时、扩表、查节点。
func (c Coverage) ProvesNoMiss() (bool, string) {
	switch {
	case c.PollInterval <= 0:
		return false, "轮询间隔未知"
	case c.ShortestEntryLifetime <= 0:
		return false, "读不到内核的 conntrack 超时配置"
	case c.ShortestEntryLifetime < time.Duration(lifetimeMargin)*c.PollInterval:
		return false, fmt.Sprintf(
			"表项最短存活 %s 不足轮询间隔 %s 的 %d 倍，两次轮询之间可能有连接来了又走",
			c.ShortestEntryLifetime, c.PollInterval, lifetimeMargin)
	case c.PollsPlanned <= 0:
		return false, "这个窗口没有计划任何轮询"
	case c.CutShort:
		return false, "窗口被提前结束，没跑满计划的轮询"
	case c.PollsSucceeded < c.PollsPlanned:
		return false, fmt.Sprintf("%d 次轮询里有 %d 次没成功",
			c.PollsPlanned, c.PollsPlanned-c.PollsSucceeded)
	case c.Truncated:
		return false, "这一轮读表撞到了条数上限，读到的不是整张表"
	case c.DropsDuringWindow > 0:
		return false, fmt.Sprintf("窗口内内核丢弃了 %d 条连接记录", c.DropsDuringWindow)
	case c.TableMax <= 0:
		return false, "读不到 conntrack 表容量"
	case float64(c.TableCount) >= tableHeadroom*float64(c.TableMax):
		return false, fmt.Sprintf("conntrack 表用量 %d/%d 已过 %.0f%%，随时可能开始丢条目",
			c.TableCount, c.TableMax, tableHeadroom*100)
	}
	return true, ""
}
