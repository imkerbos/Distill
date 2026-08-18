// Package collectrun 跑一次资产采集并把结果交给一个落库口。
//
// 单独成包，是因为它有两个消费方：cmd/distill-collector（拉取式，平台持有
// kubeconfig、直连状态库）与 cmd/distill-agent（推送式，跑在被管集群里、
// 把结果 POST 回平台）。**两个二进制、同一段采集逻辑**——拆二进制是为了让
// 「装进客户集群的那个不得链接状态库」成为一条机械可查的性质，而不是让
// 采集本身分叉成两份（design doc 2026-08-18 §1.1、§1.2）。
package collectrun

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
	"github.com/imkerbos/Distill/internal/kubeclient"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// SaveTimeout 是落库那一步的上限。
//
// 与采集的上限分开，见 Once 里为什么落库不复用被取消的那个 ctx。
// SaveTimeout 见下方注释。
const SaveTimeout = 30 * time.Second

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
//
// **地址被守卫拒绝已经从这个缺口里拆了出来**，见 ErrBlockedAPIServer。
var ErrNotProvenReadOnly = errors.New("could not prove read-only access to the target cluster")

// ErrBlockedAPIServer 表示 apiserver 地址被本平台的出站守卫拒绝。
//
// 与 ErrNotProvenReadOnly 分开是必需的，不是细分：两者的处置方向相反，
// 一个是改 RBAC，一个是改地址。它们此前落在同一个分支里，成因在时序上 ——
// kubeclient.New 只解析 kubeconfig、不拨号，守卫要到第一次真实 API 调用
// 才发力，而那次调用恰好是只读自证本身。于是一次被拒的地址会答"没能证明
// 只读"，操作者据此去审计一个完全正常的 RBAC。
//
// 同样不带底层错误：那段文本里有 apiserver 地址（规范 §19、§22）。
var ErrBlockedAPIServer = errors.New("the apiserver address was refused by the outbound guard")

// Store 是一次采集运行的落库口。*snapshotstore.Store 满足它。
//
// 收窄成一个方法而不是直接依赖具体类型：本层最容易被悄悄改错的一步是
// "部分成功的采集有没有原样落成 PARTIAL"，而那件事必须有一个不需要
// MySQL 就能变红的用例。
// Store 是一次采集的落库口。
type Store interface {
	Save(ctx context.Context, run snapshot.Run) error
	// SaveAbortedRun 记下一次在读到集群之前就失败的运行。
	//
	// 与 Save 分开，理由见 snapshotstore.SaveAbortedRun：一份"全 0 计数"
	// 的空快照读起来像一个空集群，而事实是我们根本没看过它。
	SaveAbortedRun(
		ctx context.Context, clusterID, runID string,
		startedAt, finishedAt time.Time, reason snapshot.RunErrorReason,
	) error
}

// RecordAborted 把一次"还没开始采集就失败"的运行落进历史。
//
// 落库失败只记日志、不改变调用方要返回的那个错误：中止的成因才是操作者
// 要看的东西，把它换成一句"落库失败"等于用记账的失败盖住被记的那件事。
// RecordAborted 把一次「还没开始采集就失败」的运行落进历史。
func RecordAborted(
	ctx context.Context,
	store Store,
	clusterID string,
	startedAt time.Time,
	reason snapshot.RunErrorReason,
	logger *slog.Logger,
) {
	runID, err := NewRunID()
	if err != nil {
		logger.Error("cannot record the aborted run", "cluster", clusterID, "reason", string(reason))
		return
	}

	// 另起一个不随上游一起被取消的上下文，理由同 Once 的落库：
	// 超时或 Ctrl-C 之后，"这一轮为什么没能开始"正是最该留下来的东西。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SaveTimeout)
	defer cancel()

	if err := store.SaveAbortedRun(saveCtx, clusterID, runID, startedAt, time.Now(), reason); err != nil {
		// 底层错误不外传到调用方，但要进日志：落不下这一行，界面就会把
		// 一次配置错误显示成"这个集群还没有被采集过"。
		logger.Error("cannot record the aborted run",
			"err", err, "cluster", clusterID, "runId", runID, "reason", string(reason))
	}
}

// Once 跑完一次采集：先自证只读，再采集，最后落库。
//
// **顺序是这个函数存在的理由。** AssertReadOnly 必须跑在任何一次 List
// 之前（spec §3.3）：一个已经读完集群才来自证的进程，证的是"我接下来
// 不打算写"，而这条自检要回答的是"我到底有没有写的权限"—— 它的全部
// 价值在于它跑在什么时候。顺序由 collect_test.go 的动作序列断言钉住。
//
// 单类资源失败不中断整轮，且失败不改变落库这件事：一个 90% 可见的集群
// 值得存成 90% 可见并标上 PARTIAL。把它拍平成成功、或者因为 NetworkPolicy
// 被拒就整轮放弃，都会让下游拿到一份比现实好看的事实（spec §4.2）。
// Once 跑一次采集并落库。
func Once(
	ctx context.Context,
	clusterID string,
	client kubernetes.Interface,
	fleet *cluster.Registry,
	store Store,
	logger *slog.Logger,
) (snapshot.Run, error) {
	startedAt := time.Now()

	if err := collect.AssertReadOnly(ctx, client); err != nil {
		// 先问地址：自证是第一次真实 API 调用，所以出站守卫的拒绝会从这里
		// 冒出来。不先分开的话，一次被拒的地址会答"没能证明只读"，
		// 而那句话会把操作者送去审计一个完全正常的 RBAC。
		if errors.Is(err, kubeclient.ErrBlockedDestination) {
			logger.Error("the apiserver address was refused by the outbound guard; not reading the cluster",
				"cluster", clusterID)
			RecordAborted(ctx, store, clusterID, startedAt, snapshot.RunErrorClientUnavailable, logger)
			return snapshot.Run{}, ErrBlockedAPIServer
		}

		logger.Error("the collector could not prove it holds no policy write access; not reading the cluster",
			"cluster", clusterID)
		// 一个资源都没读，但这一轮必须在历史里留下痕迹：不留的话界面显示
		// "这个集群还没有过任何一次资产采集"，与一个采集器压根没被拉起来
		// 过的集群一模一样，操作者会去等一次永远不会成功的采集。
		RecordAborted(ctx, store, clusterID, startedAt, snapshot.RunErrorReadOnlyUnproven, logger)
		return snapshot.Run{}, ErrNotProvenReadOnly
	}

	runID, err := NewRunID()
	if err != nil {
		return snapshot.Run{}, err
	}

	run, err := collect.New(clusterID, client, time.Now).Collect(ctx, runID)
	if err != nil {
		return snapshot.Run{}, fmt.Errorf("collect cluster %s: %w", clusterID, err)
	}

	// 归属判定紧跟在采集之后，用全 fleet 的登记（design doc 2026-08-18 §3.4）。
	//
	// **fleet 为 nil 表示这一步不归这里做**，那是推送模式：采集器跑在被管
	// 集群里，看不见别的集群的网段，判定由平台在收下推送时完成 —— 调的是
	// 同一个函数，不是两份实现。
	//
	// PULL 模式下少了这一行，落库的 Pod 一律没有归属，而症状要到求值阶段
	// 才会以「这个 IP 属于哪个集群答不出来」的形式冒出来。
	if fleet != nil {
		run = collect.Classify(run, fleet)
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
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SaveTimeout)
	defer cancel()

	if err := store.Save(saveCtx, run); err != nil {
		return snapshot.Run{}, fmt.Errorf("store the collection run: %w", err)
	}
	return run, nil
}

// NewRunID 生成一次采集运行的标识。
//
// 随机而非时间戳：两次在同一秒里被拉起的运行会撞上 collection_run 的
// 主键，而那次冲突的表现是第二次采集整体失败，不是两行并存。
// NewRunID 生成一个新的运行 ID。
func NewRunID() (string, error) {
	raw := make([]byte, runIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
