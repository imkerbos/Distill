package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/collect"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// saveTimeout 是落库那一步的上限。
//
// 与采集的上限分开，见 collectOnce 里为什么落库不复用被取消的那个 ctx。
const saveTimeout = 30 * time.Second

// runIDBytes 是运行 ID 的随机字节数，128 位熵，十六进制后 32 个字符。
const runIDBytes = 16

// ErrNotProvenReadOnly 表示这次运行没能证明自己对目标集群只读。
//
// **不带底层错误。** AssertReadOnly 的失败文本会包住 apiserver 返回的
// 错误，而那段文本里有 apiserver 地址（规范 §19「内部服务地址」、§22）。
// 采集器的输出终点是集群日志，读它的人比能读平台库的人多得多。
//
// 代价是"我确实有写权限"与"我问不出来"这两种成因在这一层分不开 ——
// 已知缺口，收口的做法是让 AssertReadOnly 返回一个封闭枚举的结论，
// 那是 internal/collect 的改动，不在本次装配范围内。
var ErrNotProvenReadOnly = errors.New("could not prove read-only access to the target cluster")

// runStore 是一次采集运行的落库口。*snapshotstore.Store 满足它。
//
// 收窄成一个方法而不是直接依赖具体类型：本层最容易被悄悄改错的一步是
// "部分成功的采集有没有原样落成 PARTIAL"，而那件事必须有一个不需要
// MySQL 就能变红的用例。
type runStore interface {
	Save(ctx context.Context, run snapshot.Run) error
}

// collectOnce 跑完一次采集：先自证只读，再采集，最后落库。
//
// **顺序是这个函数存在的理由。** AssertReadOnly 必须跑在任何一次 List
// 之前（spec §3.3）：一个已经读完集群才来自证的进程，证的是"我接下来
// 不打算写"，而这条自检要回答的是"我到底有没有写的权限"—— 它的全部
// 价值在于它跑在什么时候。顺序由 collect_test.go 的动作序列断言钉住。
//
// 单类资源失败不中断整轮，且失败不改变落库这件事：一个 90% 可见的集群
// 值得存成 90% 可见并标上 PARTIAL。把它拍平成成功、或者因为 NetworkPolicy
// 被拒就整轮放弃，都会让下游拿到一份比现实好看的事实（spec §4.2）。
func collectOnce(
	ctx context.Context,
	clusterID string,
	client kubernetes.Interface,
	fleet *cluster.Registry,
	store runStore,
	logger *slog.Logger,
) (snapshot.Run, error) {
	if err := collect.AssertReadOnly(ctx, client); err != nil {
		logger.Error("the collector could not prove it holds no policy write access; not reading the cluster",
			"cluster", clusterID)
		return snapshot.Run{}, ErrNotProvenReadOnly
	}

	runID, err := newRunID()
	if err != nil {
		return snapshot.Run{}, err
	}

	run, err := collect.New(clusterID, client, fleet, time.Now).Collect(ctx, runID)
	if err != nil {
		return snapshot.Run{}, fmt.Errorf("collect cluster %s: %w", clusterID, err)
	}

	for _, f := range run.Failures {
		// 只记资源与封闭枚举的原因，**不记 Detail**：那段文本是 apiserver
		// 原样返回的，带内部地址（规范 §19、§22）。Detail 落进
		// collection_run_failure，那张表的读者是登录过平台的人。
		logger.Warn("a resource kind was not collected",
			"cluster", clusterID, "runId", runID,
			"resource", string(f.Resource), "reason", string(f.Reason))
	}

	// 落库另起一个不随采集一起被取消的上下文。超时或 Ctrl-C 之后，
	// "采到了哪些、为什么不完整"正是最该留下来的东西，而复用同一个
	// 已经取消的 ctx 会让它们随之消失 —— 表现为一次完全没有留下痕迹的
	// 运行，与"采集器根本没被拉起来"无法区分。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()

	if err := store.Save(saveCtx, run); err != nil {
		return snapshot.Run{}, fmt.Errorf("store the collection run: %w", err)
	}
	return run, nil
}

// newRunID 生成一次采集运行的标识。
//
// 随机而非时间戳：两次在同一秒里被拉起的运行会撞上 collection_run 的
// 主键，而那次冲突的表现是第二次采集整体失败，不是两行并存。
func newRunID() (string, error) {
	raw := make([]byte, runIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
