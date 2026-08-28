package main

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// accountant 是记账循环唯一需要的那一样能力。
//
// 收窄成一个方法而不是直接吃 evidence.Accountant：循环要验证的是"周期在走、
// 上下文一结束就退出"，而那与记账内部做了什么无关。
type accountant interface {
	Once(ctx context.Context) error
}

// runBookkeeping 按周期给每个集群记一次证据账，直到上下文结束。
//
// **推送式接入没有别的地方可以挂它。** 资产与流量都由被管集群自己推上来，
// 落库即返回；拉取式采集器那一轮循环在这种部署里根本不存在。于是证据表停在
// 最后一次采集器运行留下的那个数（实测 UAT：2609 次推送之后 windows 仍然是
// 1、observations 仍然是 0），而界面上每条规则都显示成"刚观察到"——
// 操作者因此判断不了哪条规则值得确认。
//
// **不挂在推送的处理函数上**：15 个节点各推各的，一分钟就是 15 次，而
// windows 是 `windows + 1`。挂上去之后，一个 15 节点的集群会比单节点集群快
// 15 倍地"积累证据"，那个数字随即失去意义。周期必须是集群级的。
//
// 启动时先跑一次，之后才进周期：等满一个周期才第一次记账意味着每次重启都
// 丢掉一段观测。重复记同一个窗口由 Accountant 自己的守卫挡住。
//
// interval 为 0 表示关掉，直接返回 —— 部署里已经有拉取式采集器在记账时用。
func runBookkeeping(
	ctx context.Context, a accountant, interval time.Duration, logger *slog.Logger,
) {
	if interval <= 0 {
		logger.Info("periodic bookkeeping is off; rule evidence and the agreement-rate " +
			"trend will stay at whatever another writer records")
		return
	}
	logger.Info("periodic bookkeeping started", "interval", interval)

	once := func() {
		if err := a.Once(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			// 一轮记账失败只让这一轮的窗口从证据里缺席，下一轮照常。
			// 不退出循环：退出等于此后再也不记，而没有任何东西说得出为什么。
			logger.Warn("a round of bookkeeping failed", "err", err)
		}
	}

	once()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("periodic bookkeeping stopped")
			return
		case <-t.C:
			once()
		}
	}
}
