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

	startedAt := time.Now().UTC()
	counts := map[connKey]int{}
	var order []connKey
	var dropped uint64
	truncated := false
	var readErr error

	for i := range polls {
		if i > 0 {
			select {
			case <-ctx.Done():
				// 上下文结束不算失败：已经轮询过的那几次是真的观测。
				// 窗口如实收在这一刻。
				readErr = nil
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
		for _, c := range tbl.Connections {
			k := connKey{src: c.Source.IP, dst: c.Dest.IP, proto: string(c.Protocol), port: c.Port}
			if _, seen := counts[k]; !seen {
				order = append(order, k)
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
			ObservedCount: counts[k],
		})
	}
	// **只在真的截断时才报 dropped。** 报 0 等于宣称"一条没漏"，而轮询
	// conntrack 永远说不出那句话（design doc §5）。
	if truncated {
		payload.Dropped = &dropped
		logger.Warn("the conntrack table did not fit in one ingest",
			"cluster", opts.clusterID, "runId", runID, "dropped", dropped)
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
