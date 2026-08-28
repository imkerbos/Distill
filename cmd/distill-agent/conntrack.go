package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/conntrack"
	"github.com/imkerbos/Distill/internal/flow"
)

// defaultTablePath 是 conntrack 表在 Linux 上的位置。
//
// 这个进程必须跑在 hostNetwork 里才读得到**宿主机**那张表：nf_conntrack 是
// 按 network namespace 的，共享 host netns 之后 /proc/net/nf_conntrack 就是
// 宿主机的那张（design doc 2026-08-19-conntrack-source §9）。
const defaultTablePath = "/proc/net/nf_conntrack"

// 轮询的默认节奏。
//
// **刻意不"聪明"。** 一个自适应的间隔要有人验证它对，而验证不了的自适应会在
// 采不到东西的时候让人怀疑集群而不是怀疑参数（design doc §7）。
const (
	defaultPolls        = 12
	defaultPollInterval = 5 * time.Second
)

// conntrackOptions 是一次 conntrack 采集的全部参数。
type conntrackOptions struct {
	clusterID string
	tablePath string
	polls     int
	interval  time.Duration
	// maxConnections 是去重后允许携带的上限；非正时用 conntrack 包的默认。
	maxConnections int
	// procRoot 是 /proc 的位置，供完整度取证读内核计数与超时配置。
	// 空表示 defaultProcRoot；可替换只为测试。
	procRoot string
}

// flowSink 是这一侧要的那一个方法。
//
// 收窄成一个接口而不是直接依赖 *httpSink：用例要能在不起 HTTP 服务的前提下
// 断言"交上去的那份报文长什么样"，而那份报文的形状正是本轮全部纪律所在。
type flowSink interface {
	SaveFlowIngest(ctx context.Context, p flowIngestPayload) error
}

// windowPayload 是报文里的一段时间区间。
type windowPayload struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// connectionPayload 是观测到的一条连接。
//
// **只有地址，没有主体。** 身份由平台在读侧从 pod_identity_interval 解析
// （flow-ingest spec §2.2）—— conntrack 本来也只有地址，这条形状对它天然成立。
type connectionPayload struct {
	SrcIP         string `json:"srcIp"`
	DstIP         string `json:"dstIp"`
	Protocol      string `json:"protocol"`
	Port          int32  `json:"port"`
	ObservedCount int    `json:"observedCount"`
	// Verdict 是**来源报告的**判定，不是平台算的。缺席表示来源没报这件事，
	// 与报了 ALLOWED 是两句话：前者让这条连接不参与一致率，后者是一条可以
	// 推翻平台判定的证据（internal/reconcile）。
	//
	// conntrack 只在 TCP 上报得出：握手完成过就是执行平面放行过。
	Verdict string `json:"verdict,omitempty"`
}

// flowIngestPayload 是 POST /api/v1/agent/flow-ingests 的报文。
//
// 字段与平台侧的 agentFlowIngestPayload 一一对应。**没有 clusterId、没有
// 身份、没有 completeness** —— 归属来自 token，身份在读侧解析，完整度是证据
// 的函数。这三样在这里连字段都不存在。
//
// CoveredWindow / SampleRate / Dropped 是**指针**：缺席与零值必须分得开。
// `dropped: 0` 是"来源说一条没丢"，缺席是"来源不报这件事"；而
// `sampleRate` 缺席不等于 1.0。用值类型的话 JSON 的零值会把"没有证据"变成
// "证明了完整"，而完整度是 COMPLETE 时下游不降级。
type flowIngestPayload struct {
	SchemaVersion   int                 `json:"schemaVersion"`
	RunID           string              `json:"runId"`
	Source          string              `json:"source"`
	Status          string              `json:"status"`
	ErrorReason     string              `json:"errorReason,omitempty"`
	StartedAt       time.Time           `json:"startedAt"`
	FinishedAt      time.Time           `json:"finishedAt"`
	RequestedWindow windowPayload       `json:"requestedWindow"`
	CoveredWindow   *windowPayload      `json:"coveredWindow,omitempty"`
	SampleRate      *float64            `json:"sampleRate,omitempty"`
	Dropped         *uint64             `json:"dropped,omitempty"`
	Connections     []connectionPayload `json:"connections,omitempty"`
}

// conntrackOnce 轮询一遍 conntrack 表并把结果推给平台。
//
// **失败也推。** 一次读不到表的运行若什么都不留下，这个集群在界面上就是
// "这段时间没有流量"，而下游把"没有流量"读成"覆盖这些连接的规则可以收紧"
// （flow-ingest spec §4）。因此读不到就推一份 FAILED + UNREACHABLE，
// 且不带任何连接。
func conntrackOnce(
	ctx context.Context, opts conntrackOptions, sink flowSink, logger *slog.Logger,
) error {
	runID, err := collectrun.NewRunID()
	if err != nil {
		return err
	}
	path := opts.tablePath
	if path == "" {
		path = defaultTablePath
	}
	polls := opts.polls
	if polls <= 0 {
		polls = defaultPolls
	}
	interval := opts.interval
	if interval <= 0 {
		interval = defaultPollInterval
	}

	root := opts.procRoot
	if root == "" {
		root = defaultProcRoot
	}
	// 窗口开始前先采一次丢弃计数：完整度看的是**这个窗口内新增**的丢弃，
	// 不是开机以来的累计。拿累计数作答，一个跑了一周、早期丢过一次的节点
	// 会永远说自己不完整。
	statsBefore, statsBeforeOK := readStats(root)
	shortestLifetime := shortestEntryLifetime(root)

	startedAt := time.Now().UTC()
	counts := map[connKey]int{}
	verdicts := map[connKey]string{}
	var order []connKey
	var dropped uint64
	truncated := false
	var readErr error
	succeeded := 0
	cutShort := false

	for i := range polls {
		if i > 0 {
			select {
			case <-ctx.Done():
				// 上下文结束不算失败：已经轮询过的那几次是真的观测。
				// 窗口如实收在这一刻。
				readErr = nil
				cutShort = true
				goto done
			case <-time.After(interval):
			}
		}
		tbl, err := readTable(path, opts.maxConnections)
		if err != nil {
			// 第一次就读不到 → 整次失败。已经读到过 → 保留读到的那些，
			// 这一次的失败只记日志：一个节点某一刻读不到，不该让前几次
			// 真实的观测跟着消失。
			if len(order) == 0 {
				readErr = err
				goto done
			}
			logger.Warn("a conntrack poll failed after earlier ones succeeded",
				"cluster", opts.clusterID, "poll", i)
			continue
		}
		if tbl.Truncated {
			truncated = true
			dropped += tbl.Dropped
		}
		if tbl.SkippedProtocol > 0 {
			// ICMP 落在这里。default-deny 会拦住它，而 NetworkPolicy v1
			// 表达不了它 —— 这是 NetworkPolicy 本身的边界，但它必须有个
			// 数字（design doc §6.1）。
			logger.Info("conntrack entries whose protocol NetworkPolicy cannot express",
				"cluster", opts.clusterID, "count", tbl.SkippedProtocol)
		}
		if tbl.SkippedMalformed > 0 {
			logger.Warn("conntrack lines that could not be read",
				"cluster", opts.clusterID, "count", tbl.SkippedMalformed)
		}
		succeeded++
		for _, c := range tbl.Connections {
			k := connKey{src: c.Source.IP, dst: c.Dest.IP, proto: string(c.Protocol), port: c.Port}
			if _, seen := counts[k]; !seen {
				order = append(order, k)
				// 判定由协议决定，同一个键每次都一样；取第一次见到的那个。
				if v, ok := c.Verdict(); ok {
					verdicts[k] = string(v)
				}
			}
			counts[k] += c.ObservedCount
		}
	}

done:
	finishedAt := time.Now().UTC()
	payload := flowIngestPayload{
		SchemaVersion: agentSchemaVersion,
		RunID:         runID,
		Source:        string(flow.SourceNodeConntrack),
		Status:        "OK",
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
		// **只报请求窗口，不报覆盖窗口。** conntrack 是当前被跟踪连接的
		// 快照，不是日志：轮询之间起止的连接从来不出现在任何一次快照里，
		// 而条目上没有可靠的起始时刻。说自己覆盖了这段时间就是编的，
		// 而编出来的那句话会让下游不降级（design doc §4）。
		RequestedWindow: windowPayload{From: startedAt, To: finishedAt},
	}
	if readErr != nil {
		// 读不到表与"表是空的"是两件事：前者是"没看到"，后者是"看过了，
		// 这一刻没有连接"。塌成一个会让数据面绕开 netfilter 这件事看起来
		// 像集群很安静（design doc §8）。
		payload.Status = "FAILED"
		payload.ErrorReason = "UNREACHABLE"
		logger.Error("the conntrack table could not be read",
			"cluster", opts.clusterID, "runId", runID)
		if err := sink.SaveFlowIngest(ctx, payload); err != nil {
			logger.Warn("cannot report a failed conntrack run", "err", err)
		}
		return readErr
	}

	payload.Connections = make([]connectionPayload, 0, len(order))
	for _, k := range order {
		payload.Connections = append(payload.Connections, connectionPayload{
			SrcIP: k.src, DstIP: k.dst, Protocol: k.proto, Port: k.port,
			ObservedCount: counts[k], Verdict: verdicts[k],
		})
	}
	if truncated {
		payload.Dropped = &dropped
		logger.Warn("the conntrack table did not fit in one ingest",
			"cluster", opts.clusterID, "runId", runID, "dropped", dropped)
	}

	// **"这个窗口没漏"要么被证明，要么不说。**
	//
	// 轮询 conntrack 本身说不出这句话——表是当前连接的快照、不是日志，两次
	// 轮询之间来了又走的连接不出现在任何一次快照里。但这不是一个永远无法
	// 回答的问题，而是一个有前提的问题：只要表项的最短存活时间长过轮询
	// 间隔，任何一条连接都必然落在至少一次轮询的视野里，"没漏"就是可证的。
	//
	// 于是这里把那几条前提逐个去内核里取实测值，全部成立才填这三个字段。
	// 任一条不成立——读不到配置、超时太短、有一次轮询没成功、窗口内有丢弃、
	// 表快满了——就照旧什么都不说，完整度落回 UNKNOWN，与这段代码不存在时
	// 完全一样。**默认答案仍然是"证明不了"，改变的只是它现在可以被推翻。**
	statsAfter, statsAfterOK := readStats(root)
	count, max := readTableUsage(root)
	cov := conntrack.Coverage{
		PollInterval:          interval,
		ShortestEntryLifetime: shortestLifetime,
		PollsPlanned:          polls,
		PollsSucceeded:        succeeded,
		CutShort:              cutShort,
		Truncated:             truncated,
		TableCount:            count,
		TableMax:              max,
	}
	if statsBeforeOK && statsAfterOK {
		cov.DropsDuringWindow = statsAfter.Total() - statsBefore.Total()
	} else {
		// 读不到计数就当作"有丢弃"：说不出丢没丢，与说得出没丢，在可信度上
		// 不是一档。给一个非零值让 ProvesNoMiss 走到那一条上。
		cov.DropsDuringWindow = 1
	}
	if proven, why := cov.ProvesNoMiss(); proven {
		covered := windowPayload{From: startedAt, To: finishedAt}
		rate := 1.0
		var none uint64
		payload.CoveredWindow = &covered
		// 轮询整张表，不抽样。
		payload.SampleRate = &rate
		payload.Dropped = &none
		logger.Info("this window is provably complete",
			"cluster", opts.clusterID, "runId", runID,
			"pollInterval", interval, "shortestEntryLifetime", shortestLifetime,
			"tableUsage", fmt.Sprintf("%d/%d", count, max))
	} else {
		logger.Info("this window cannot be proven complete",
			"cluster", opts.clusterID, "runId", runID, "reason", why)
	}

	if err := sink.SaveFlowIngest(ctx, payload); err != nil {
		// 不吞：agent 跑在 DaemonSet 里，一次吞掉的失败会让这一轮观测悄悄
		// 消失，而界面上与"这段时间没有流量"一模一样。
		return fmt.Errorf("push the conntrack observations: %w", err)
	}
	logger.Info("conntrack observations pushed",
		"cluster", opts.clusterID, "runId", runID,
		"connections", len(payload.Connections), "polls", polls)
	return nil
}

// connKey 是跨轮询的去重键。与 conntrack 包内那个同形，但这里跨的是多次
// 快照 —— 同一个服务对在十二次轮询里出现十二次，交上去的是一条、计数十二。
type connKey struct {
	src   string
	dst   string
	proto string
	port  int32
}

// readTable 读一次 conntrack 表。
//
// 错误不包原因也不带路径：这个进程的输出终点是被管集群的日志，而路径是
// 部署布局信息（规范 §19、§22）。调用方只需要知道"这次读不到"。
func readTable(path string, max int) (conntrack.Table, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path comes from this process's own flags.
	if err != nil {
		return conntrack.Table{}, errors.New("the conntrack table could not be opened")
	}
	defer func() { _ = f.Close() }()
	return conntrack.Parse(f, conntrack.Limit{Max: max})
}
