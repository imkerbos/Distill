package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/hubble"
	"github.com/imkerbos/Distill/internal/kubeclient"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
	"github.com/imkerbos/Distill/internal/settings"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// maxIngestWindow 是一次摄入允许请求的时间窗上限（安全规范 §24）。
//
// 窗口越长，relay 那一端要流回来的东西越多，而那些东西是别人的集群产生的，
// 不是我们能约束的输入。上限取一天：spec §7 的量级估算按小时窗口做，
// 一天已经比它宽一个数量级，再宽就不是"一次摄入"而是一次归档任务了。
const maxIngestWindow = 24 * time.Hour

// ingestStore 是一次流量摄入的落库口。*snapshotstore.Store 满足它。
//
// 与 runStore 分成两个接口而不是并到一起：摄入运行与采集运行分开记
// （spec §6）—— 资产采集失败多半是 RBAC，流量摄入失败多半是 relay 或配额，
// 两者的处置人都可能不同。分开的接口让"顺手把摄入结果塞进采集那张表"
// 这件事在类型层面就没有落脚点。
type ingestStore interface {
	SaveIngest(ctx context.Context, run snapshotstore.IngestRun) error
}

// collectorStore 是采集器要的全部落库能力。*snapshotstore.Store 满足它。
type collectorStore interface {
	runStore
	ingestStore
	deriveStore
	reconcileStore
	evidenceStore
}

// flowSource 把一个摄入器与它的来源种类绑在一起。
//
// 种类跟着摄入器走而不是写死在落库那一步：完整度元数据是按来源解释的
// （spec §4），一批说不出自己从哪来的连接也就说不出它该按什么解释。
// flow.Source 接口刻意不带这个方法 —— 它是摄入的契约，不是自我描述。
type flowSource struct {
	source flow.Source
	kind   flow.SourceKind
}

// ingestOptions 是流量摄入的命令行选项。
//
// relayAddress 为空表示这次运行不摄入流量，只采资产 —— 那是一个合法的
// 部署形态（还没接上 relay 的集群），因此不是错误。
type ingestOptions struct {
	relayAddress  string
	credentialRef string
	window        time.Duration
}

// enabled 报告这次运行是否要摄入流量。
func (o ingestOptions) enabled() bool { return o.relayAddress != "" }

// validate 检查摄入选项自身是否可用。
func (o ingestOptions) validate() error {
	if !o.enabled() {
		return nil
	}
	// 非正窗口不是"不限时间"，是一个写错了的编排文件；上限则是 §24。
	// 两者都在启动时拒绝，而不是等到 relay 那边吐回一堆东西才发现。
	if o.window <= 0 {
		return errors.New("-flow-window must be positive: an ingest window with no length observes nothing")
	}
	if o.window > maxIngestWindow {
		return fmt.Errorf("-flow-window must not exceed %v", maxIngestWindow)
	}
	return nil
}

// newFlowSource 按选项装配一个直连 Hubble relay 的摄入器。
//
// 凭据与 apiserver 同一套机制（spec §5）：库里存短名引用，值由**采集器**
// 解析 —— 主服务不得持有这条路径，理由与 kubeconfig 完全相同。因此这里
// 复用 newSecretResolver，而不是让 internal/hubble 自己去碰后端。
//
// 没给凭据引用时不构造解析器：那条路径连一次都不需要走，而少读一次设置
// 就少一处失败面。明文连接只在 relay 位于集群内、或经端口转发到本机时
// 成立 —— 这一点由 internal/hubble 的 Config.CredentialRef 注释持有。
func newFlowSource(
	ctx context.Context, reg registry.Store, opts ingestOptions,
) (*flowSource, error) {
	var resolver secrets.Resolver
	if opts.credentialRef != "" {
		setting, err := settings.New(reg).Current(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the platform setting: %w", err)
		}
		resolver, err = newSecretResolver(ctx, setting)
		if err != nil {
			return nil, err
		}
		if resolver == nil {
			// 后端是 NONE：操作者明确选了"不解析凭据"，却又给了一个凭据
			// 引用。静默降级成明文是这里最坏的落法 —— 症状是"它还在工作"，
			// 而实际上已经没有任何东西在验证对端是不是 relay。
			return nil, errors.New("the secrets backend is NONE: the collector cannot resolve the relay credential")
		}
	}

	// 地址不回显到任何错误里：它是内网拓扑信息（安全规范 §19、§22），
	// 而 hubble.New 已经按这条规则写过一遍，这里只是不要把它抵消掉。
	src, err := hubble.New(hubble.Config{
		Address:       opts.relayAddress,
		CredentialRef: opts.credentialRef,
		Secrets:       resolver,
	})
	if err != nil {
		return nil, err
	}
	return &flowSource{source: src, kind: flow.SourceHubble}, nil
}

// collectAndIngest 跑完一次采集、摄一次流量、再推一次身份区间，三者各自落
// 自己的运行记录。
//
// **顺序与失败传播是这个函数存在的理由。**
//
// 采集失败就不往下走：那一轮连集群都没读到，一行"摄入成功"落进库里会让
// 界面显示出一份有流量、没有资产的半张图，而那半张图看起来像是资产侧
// "这次什么都没采到"，不像"这次根本没采成"。
//
// **摄入排在推导之前，按三者失败的可挽回性排（spec §2.2）：**
//
//   - 资产可以重采 —— 再跑一次采集器就拿到当前状态；
//   - 推导可以重跑 —— 它读的是一次**已经落库**的采集运行，原料还在库里；
//   - 流量不能重来 —— Hubble 从环形缓冲区供数，一个没摄到的窗口就是没了，
//     而"没观测到的流量"恰恰会被这个平台读成"这条规则没有流量、可以收紧"。
//
// 让可挽回的那次失败毁掉不可挽回的证据是反的，因此推导排在最后，且**摄入
// 失败不跳过推导** —— 两者互不为前置条件（推导读的是库里的 Pod 观测，
// 与流量无关；摄入不读区间表，见 spec §2.2 的核对）。
//
// 两个结果用 errors.Join 一起返回，**先到的失败不得盖住后到的**：只报第一个
// 会让一次"摄入挂了、推导也挂了"的运行在日志里看起来只坏了一半，于是另一半
// 无人处置。errors.Join(nil, nil) 是 nil，因此全成功仍然返回 nil。
//
// 摄入与推导失败都**不撤销**已经落库的采集：一次采成功、后续挂掉是一个真实
// 且有用的结果，资产值得留下。但它们不得读成一次完整的运行 —— 各自那一行落成
// FAILED 带封闭枚举的原因，且这个函数返回错误，于是进程以非零码退出。两件事
// 都要：只落行不返错，编排系统会把这一轮当成功；只返错不落行，界面上
// 这个集群看起来"从来没有摄入过流量"，与一个还没接 relay 的集群一模一样。
func collectAndIngest(
	ctx context.Context,
	clusterID string,
	client kubernetes.Interface,
	fleet *cluster.Registry,
	store collectorStore,
	src *flowSource,
	window flow.Window,
	// rec 算一次一致率。允许为 nil —— 没有读栈的调用方（测试）不做这一步，
	// 而 nil 表示"这一轮不对账"，**不是**"对账结果是好的"。
	rec reconciler,
	// planes 是第二平面的覆盖范围，随这一次快照落库。零值即"范围不完整"，
	// 下游据此整片降级 —— 没填过这个参数的调用路径不该悄悄拿到精确降级。
	planes collectrun.ForeignPlanes,
	logger *slog.Logger,
) error {
	result, err := collectOnce(ctx, clusterID, client, fleet, store, planes, logger)
	if err != nil {
		return err
	}
	logger.Info("collection stored",
		"cluster", clusterID,
		"runId", result.Observation.RunID,
		"status", string(result.Status),
		"failures", len(result.Failures),
		"warnings", len(result.Observation.Warnings))

	// 摄入的错误先接住而不是立刻返回：推导不依赖这一步，而提前返回会让一次
	// relay 故障顺带取消掉一次本来跑得成的推导 —— 那是把一个故障变成两个。
	var ingestErr error
	if src == nil {
		// 没配置摄入是一个合法的部署形态，不是一次失败。说出来，是因为
		// 一次"只采了资产"的运行与一次"摄入器坏了"的运行在库里长得不一样，
		// 但在日志里很容易被读成同一件事。
		logger.Info("flow ingestion is not configured; only assets were collected", "cluster", clusterID)
	} else {
		run, err := ingestOnce(ctx, clusterID, src, window, store, logger)
		ingestErr = err
		if err == nil {
			logger.Info("flow ingestion stored",
				"cluster", clusterID,
				"ingestRunId", run.RunID,
				"status", string(run.Status),
				"connections", ingestedConnections(run),
				"completeness", string(ingestedCompleteness(run)))
		}
	}

	deriveErr := deriveOnce(ctx, clusterID, result.Observation.RunID, store, logger)
	if deriveErr == nil {
		logger.Info("identity intervals derived",
			"cluster", clusterID, "runId", result.Observation.RunID)
	}

	// 对账排在身份推导之后：判定要靠区间表把地址解成主体，推导没跑完就
	// 对账，算出来的会是一屏 PLATFORM_UNKNOWN —— 一个看起来"平台什么都
	// 答不出"的假象。
	//
	// 只在摄入与推导都成功时做：拿一份残缺的窗口去算一致率，得到的数字
	// 会被记进趋势，而趋势里一个没有来由的凹陷比没有数据更糟。
	if ingestErr == nil && deriveErr == nil && rec != nil {
		reconcileOnce(ctx, clusterID, window.From, window.To, rec, store, logger)
		// 证据与对账同一批前提：两者都要求这一轮的观测是完整的。
		// 拿一份残缺窗口去记证据，会把"这条规则观察了 N 个窗口"算多。
		recordEvidenceOnce(ctx, clusterID, window.From, window.To, rec, store, logger)
	}

	return errors.Join(ingestErr, deriveErr)
}

// ingestOnce 跑完一次摄入并落库，返回落下的那一行。
//
// 失败也落库，且落的是 FAILED 加一个封闭枚举的原因：一次连不上 relay 的
// 摄入若什么都不留下，这个集群在界面上看起来就是"这段时间没有流量"，
// 而下游会把"没有流量"读成"覆盖这些连接的规则可以收紧"（spec §2）。
//
// 落库用一个不随上游一起被取消的上下文，理由同 collectOnce：超时或 Ctrl-C
// 之后，"这一轮为什么没摄到"正是最该留下来的东西。
func ingestOnce(
	ctx context.Context,
	clusterID string,
	src *flowSource,
	window flow.Window,
	store ingestStore,
	logger *slog.Logger,
) (snapshotstore.IngestRun, error) {
	startedAt := time.Now()
	runID, err := newRunID()
	if err != nil {
		return snapshotstore.IngestRun{}, err
	}

	result, ingestErr := src.source.Ingest(ctx, clusterID, window)
	run := snapshotstore.IngestRun{
		ClusterID:  clusterID,
		RunID:      runID,
		StartedAt:  startedAt,
		FinishedAt: time.Now(),
		Result:     result,
		Status:     ingestStatus(result),
	}
	if ingestErr != nil {
		// 失败时**丢掉来源交回来的那份结果**：一次半途失败的摄入可能带回
		// 一部分连接，而它们的完整度证据是残缺的。落一份"看见了这些"的
		// 记录会让读取方拿它当窗口的事实，而这一轮真正确定的只有"我们没
		// 看全"。空结果的完整度是 UNKNOWN，配上 FAILED 才读得对。
		empty, err := flow.NewIngestResult(src.kind, window, flow.Window{}, nil)
		if err != nil {
			return snapshotstore.IngestRun{}, err
		}
		run.Result = empty
		run.Status = snapshotstore.IngestFailed
		run.ErrorReason = ingestErrorReason(ingestErr)
	}

	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()
	if err := store.SaveIngest(saveCtx, run); err != nil {
		return snapshotstore.IngestRun{}, fmt.Errorf("store the flow ingest run: %w", err)
	}

	if ingestErr != nil {
		// 只记封闭枚举的原因，不记底层错误文本：internal/hubble 的错误刻意
		// 不带 relay 地址与传输层细节，而这个进程的输出终点是集群日志
		// （安全规范 §19、§22）。落库成功不等于摄入成功 —— 错误照样往上走。
		logger.Error("flow ingestion failed", "cluster", clusterID,
			"ingestRunId", runID, "reason", string(run.ErrorReason))
		return run, fmt.Errorf("ingest flows for cluster %s: %w", clusterID, ingestErr)
	}
	return run, nil
}

// ingestStatus 把摄入结果的完整度翻译成运行状态。
//
// **状态与完整度是两个字段，这里不合并它们**（CLAUDE.md §3 的 verdict /
// confidence 同一条规则）：状态回答"这次跑成了吗"，完整度回答"这段观测
// 漏了多少"，且完整度根本不落列 —— 库里只存证据，读取时由 flow 包重算。
//
// 因此 UNKNOWN 落 OK：Hubble 给不出采样率，它的常态就是"跑完了，但证明不了
// 一条没漏"。把它记成 PARTIAL 会让每一次正常摄入都带着一个失败色的状态，
// 于是真正跑漏了的那些反而没人看。而 DEGRADED 是**已知**漏了（窗口没覆盖满、
// relay 报了丢弃），那正是 PARTIAL 要表达的东西。
func ingestStatus(res flow.IngestResult) snapshotstore.IngestStatus {
	if _, completeness := res.Connections(); completeness == flow.CompletenessDegraded {
		return snapshotstore.IngestPartial
	}
	return snapshotstore.IngestOK
}

// ingestErrorReason 把摄入失败翻译成封闭枚举的原因（CLAUDE.md §3）。
//
// 一律用 errors.Is 判定，**不匹配错误文本**：internal/hubble 刻意把地址与
// 传输层细节挡在错误文本之外，靠字符串反推原因等于把那层遮挡当成契约。
// 认不出的形态落 OTHER —— 这一层的失败方向是"说不出来"，不是猜一个看起来
// 合理的原因，因为原因决定的是谁去查、往哪儿查。
func ingestErrorReason(err error) snapshotstore.IngestErrorReason {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return snapshotstore.IngestErrorTimeout
	case errors.Is(err, kubeclient.ErrBlockedDestination),
		errors.Is(err, hubble.ErrRelayUnavailable):
		return snapshotstore.IngestErrorUnreachable
	default:
		return snapshotstore.IngestErrorOther
	}
}

// ingestedConnections 返回这一行记下了多少条连接，仅供日志。
func ingestedConnections(run snapshotstore.IngestRun) int {
	conns, _ := run.Result.Connections()
	return len(conns)
}

// ingestedCompleteness 返回这一行的完整度，仅供日志。
func ingestedCompleteness(run snapshotstore.IngestRun) flow.Completeness {
	_, completeness := run.Result.Connections()
	return completeness
}
