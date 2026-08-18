package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/snapshot"
	"k8s.io/client-go/kubernetes"
)

// 采集本身搬去了 internal/collectrun：推送式接入是一个独立的二进制
// （cmd/distill-agent），而两个二进制不该有两份采集逻辑
// （design doc 2026-08-18 §1.1）。这里留别名与转发，本包的调用点与测试
// 按这些名字写。
type runStore = collectrun.Store

const saveTimeout = collectrun.SaveTimeout

var (
	// ErrNotProvenReadOnly 见 collectrun。
	ErrNotProvenReadOnly = collectrun.ErrNotProvenReadOnly
	// ErrBlockedAPIServer 见 collectrun。
	ErrBlockedAPIServer = collectrun.ErrBlockedAPIServer
)

func collectOnce(
	ctx context.Context, clusterID string, client kubernetes.Interface,
	fleet *cluster.Registry, store runStore, logger *slog.Logger,
) (snapshot.Run, error) {
	return collectrun.Once(ctx, clusterID, client, fleet, store, logger)
}

func recordAbortedRun(
	ctx context.Context, store runStore, clusterID string,
	startedAt time.Time, reason snapshot.RunErrorReason, logger *slog.Logger,
) {
	collectrun.RecordAborted(ctx, store, clusterID, startedAt, reason, logger)
}

func newRunID() (string, error) { return collectrun.NewRunID() }
