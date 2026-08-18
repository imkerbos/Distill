package main

import (
	"context"
	"log/slog"

	"github.com/imkerbos/Distill/internal/identityderive"
)

// deriveStore 是本包对推导落库口的别名。
//
// 实现搬去了 internal/identityderive：PUSH 模式下同一次推导只能发生在平台侧
// （agent 读不到整张区间表），两个消费方就不该有两份实现（design doc
// 2026-08-18）。这里留一个别名与一层转发，本包的调用点与测试按这个名字写。
type deriveStore = identityderive.Store

// deriveOnce 转发到 internal/identityderive.Once。
func deriveOnce(ctx context.Context, clusterID, runID string, store deriveStore, logger *slog.Logger) error {
	return identityderive.Once(ctx, clusterID, runID, store, logger)
}
