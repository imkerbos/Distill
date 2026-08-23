package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// options 是这个二进制的全部参数。
type options struct {
	// platformURL 是平台的地址。
	platformURL string
	// tablePath / polls / pollInterval 是 conntrack 那条循环的参数。
	tablePath    string
	polls        int
	pollInterval time.Duration
	// flowEvery / assetsEvery 是两条循环各自的间隔。
	//
	// 分开是因为两件事的基数不同：conntrack 按节点（每个 Pod 都跑），
	// 资产按集群（只有 leader 跑）。一个共用的间隔只能取两者里更频繁的
	// 那个，而那会让每三十分钟才需要一次的整集群 list 变成每分钟一次。
	flowEvery   time.Duration
	assetsEvery time.Duration
	// leaseNamespace / leaseName 是 leader 选举用的那个 Lease。
	leaseNamespace string
	leaseName      string
	// allowPlaintext 显式允许 http:// 的平台地址。
	//
	// **默认关闭，且只该在本机开发时打开。** Authorization 头里那把 token
	// 等价于一把能往平台写这个集群全部数据的钥匙，走明文就是让它在集群
	// 网络里裸奔。做成显式开关而不是默认允许：一个默认允许明文的实现，
	// 生产上没有任何东西会提醒你忘了配 TLS。
	allowPlaintext bool
	// tokenFile 是挂进来的 agent token 文件路径。
	//
	// **从文件读，不从环境变量读**（规范 §33）：环境变量会出现在
	// /proc/<pid>/environ、崩溃转储与一部分运维工具的进程列表里，而挂载
	// 的 Secret 文件不会。
	tokenFile string
	// caFile 是一份 PEM 证书包的路径，用来验证平台的 TLS 证书。可选。
	//
	// 平台证书由内部 CA 签发时必填：系统根里没有它，agent 每一次请求都会以
	// 证书校验失败结束，而这个进程在别人的集群里，没有别的出口。空着就用
	// 系统根（多数部署走公共 CA）。
	caFile string
	// heartbeatFile 是 flow 循环每转一圈写一次时间戳的文件。可选。
	//
	// 存活探针（-healthcheck 模式）读它判断这个 Pod 卡没卡死。空着就不写，
	// 也就没有存活探针 —— 一个卡死的 agent 不会被重启，静默停摆。
	heartbeatFile string
}

// validate 在做任何事之前检查推送参数。
// errTimeoutNotPositive 是一个写错了的超时配置。
var errTimeoutNotPositive = errors.New("-timeout must be positive: an unbounded collection is one nobody can cancel")

func (o options) validate() error {
	if o.platformURL == "" {
		return errors.New("-platform-url is required in push mode")
	}
	// 按解析出来的 scheme 判，不按字符串前缀：前缀比对对大小写与畸形输入
	// 的处理各家不同，而这里判错的方向是放行一次明文推送。
	u, err := url.Parse(o.platformURL)
	if err != nil || u.Host == "" {
		return errors.New("-platform-url must be an http(s) URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !o.allowPlaintext {
			return errors.New(
				"-platform-url is plaintext http: the agent token would cross the network in " +
					"the clear. Use https, or pass -allow-plaintext if this is a local test")
		}
	default:
		return errors.New("-platform-url must be an http(s) URL")
	}
	if o.tokenFile == "" {
		return errors.New("-token-file is required in push mode")
	}
	return nil
}

// readToken 读出挂进来的 agent token。
//
// 读完即用，不落第二份：这个值等价于一把能往平台写这个集群数据的钥匙。
// 错误里不带路径 —— 那是部署布局信息（规范 §19、§22）。
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path comes from this process's own flags, not from a request.
	if err != nil {
		return "", errors.New("the agent token file could not be read")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("the agent token file is empty")
	}
	return token, nil
}

// inClusterClient 用 Pod 自己的 ServiceAccount 建一个客户端。
//
// **推送模式下根本不存在 kubeconfig**：凭据是 kubelet 投进来的那份 token，
// 平台从来没有见过它，也就无从泄漏（design doc §1）。
func inClusterClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// 不包 err：它会带上环境变量名与挂载路径。
		return nil, errors.New("this process is not running inside a cluster: push mode needs an in-cluster ServiceAccount")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.New("cannot build a Kubernetes client from the in-cluster credentials")
	}
	return client, nil
}

// agentConfig 是平台答复的采集配置。
type agentConfig struct {
	ClusterID     string `json:"clusterId"`
	SchemaVersion int    `json:"schemaVersion"`
}

// errAgentRefused 表示平台**明确拒绝**了这个 agent —— 401/403。
//
// 与「平台暂时不可达」分得开是这条错误存在的全部理由：前者是致命的，token
// 被吊销或无效，重试一万次都不会变好，正确的处置是响亮退出、让人去重签一把
// （CrashLoopBackOff 就是那个可见信号）；后者是暂态，平台在重部署、网络在
// 抖，退避重试就能过去。塌成一个，一次平台重部署会让全体 agent 崩，而一把
// 真被吊销的 token 会被无限重试、永远看不见（上一轮实测：预检硬依赖导致
// CrashLoop）。
var errAgentRefused = errors.New("the platform refused this agent")

// fetchAgentConfig 问平台「我是哪个集群、该按哪个契约上报」。
//
// 集群归属由平台按 token 反查，agent 不自己声明（design doc §2）。这一步
// 同时是一次连通性与凭据的预检：token 被吊销时这里就失败，而不是采完
// 一整轮资产之后在最后一步失败。
//
// **认证失败（401/403）包 errAgentRefused，其余失败不包** —— 调用方
// resolveConfig 据此决定「立刻退出」还是「退避重试」。
func fetchAgentConfig(ctx context.Context, client *http.Client, base, token string) (agentConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/api/v1/agent/config", nil)
	if err != nil {
		return agentConfig{}, errors.New("cannot build the config request")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return agentConfig{}, errors.New("the platform could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return agentConfig{}, errors.New("the platform's reply could not be read")
	}
	var envelope struct {
		Code int         `json:"code"`
		Data agentConfig `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return agentConfig{}, fmt.Errorf("the platform replied with something that is not a result (HTTP %d)", resp.StatusCode)
	}
	// 401/403 是认证被拒 —— 致命。包 errAgentRefused。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return agentConfig{}, fmt.Errorf("%w (HTTP %d, code %d)", errAgentRefused, resp.StatusCode, envelope.Code)
	}
	// 其余非 200 / code!=0 当暂态：5xx 是平台自己的毛病、会好，交给重试。
	if resp.StatusCode != http.StatusOK || envelope.Code != 0 {
		return agentConfig{}, fmt.Errorf("the platform did not accept the config request (HTTP %d, code %d)", resp.StatusCode, envelope.Code)
	}
	if envelope.Data.ClusterID == "" {
		return agentConfig{}, errors.New("the platform did not say which cluster this agent belongs to")
	}
	// 版本不认识就停在这里，不采：采完再发现对不上，等于白读一遍别人的
	// 集群（design doc §4）。**版本不匹配也是致命的**：重试不会让平台改口，
	// 该升级的是这个二进制。包 errAgentRefused 走同一条「立刻退出」。
	if envelope.Data.SchemaVersion != agentSchemaVersion {
		return agentConfig{}, fmt.Errorf(
			"%w: it speaks ingest schema %d and this collector speaks %d",
			errAgentRefused, envelope.Data.SchemaVersion, agentSchemaVersion)
	}
	return envelope.Data, nil
}

// resolveConfig 反复预检，直到拿到配置、被明确拒绝、或预算耗尽。
//
// **认证被拒（errAgentRefused）立刻返回，不重试**：token 无效或版本不匹配，
// 重试只是拿噪音换不来结果，CrashLoop 才是让人去处理的信号。**暂态失败退避
// 重试**：平台重部署、网络抖，退避几秒就过去了，不该让 agent 一崩了之。
//
// 预算是调用方给的 ctx —— 复用 run() 那个 preflight 超时，不另设旋钮。预算
// 耗尽仍失败就返回错误、由 run() 退出：一个**持续**的故障仍要以退出的形态
// 浮现（restart 计数看得见），不能让 Pod 显示 Running 却永远什么都不做。
func resolveConfig(
	ctx context.Context, client *http.Client, base, token string,
	backoff time.Duration, logger *slog.Logger,
) (agentConfig, error) {
	if backoff <= 0 {
		backoff = defaultPreflightBackoff
	}
	const backoffCap = 30 * time.Second
	for {
		cfg, err := fetchAgentConfig(ctx, client, base, token)
		if err == nil {
			return cfg, nil
		}
		if errors.Is(err, errAgentRefused) {
			return agentConfig{}, err
		}
		// 暂态：记一句、退避、重试；期间 ctx 结束就带着最后那个错误返回。
		logger.Warn("preflight could not reach the platform yet; will retry", "err", err)
		select {
		case <-ctx.Done():
			return agentConfig{}, fmt.Errorf("gave up reaching the platform: %w", err)
		case <-time.After(backoff):
		}
		if backoff < backoffCap {
			backoff *= 2
			if backoff > backoffCap {
				backoff = backoffCap
			}
		}
	}
}

// defaultPreflightBackoff 是预检重试的首个退避间隔，之后翻倍到 30s 封顶。
const defaultPreflightBackoff = 3 * time.Second

// run 跑一次推送式采集。
//
// **这条路径不触达平台状态库**：没有 mysqlregistry、没有 snapshotstore、
// 没有 DSN（design doc §1.2）。它拿得到的只有自己集群的只读凭据与一把
// 只能往一个集群写数据的 token。
func run(ctx context.Context, opts options, timeout time.Duration, logger *slog.Logger) error {
	if err := opts.validate(); err != nil {
		return err
	}
	token, err := readToken(opts.tokenFile)
	if err != nil {
		return err
	}

	if opts.allowPlaintext {
		// 打开了明文就要一直吵。一个静默接受明文的 agent，在生产上没有
		// 任何东西会提醒你那把 token 正在裸奔。
		logger.Warn("this agent is talking to the platform over plaintext http; " +
			"its token crosses the network in the clear")
	}

	// CA 池建一次，同时喂给配置预检与 sink：两条路径带的是同一把 token，
	// 必须信任同一组 CA。读不到或不是 PEM 就在这里失败 —— 早于任何一次带
	// token 的请求，而不是让它以「证书校验失败」的形态在每一轮里反复出现。
	pool, err := loadCAPool(opts.caFile)
	if err != nil {
		return err
	}
	httpClient := agentClient(pool)

	// 预检用一个有限的超时当**重试预算**；常驻循环本身不设总超时 —— 那是一个
	// DaemonSet，它的生命周期由 SIGTERM 结束，不由一个计时器结束。timeout
	// 管的是单轮（见下），这里借它当「预检最多重试多久」。
	//
	// resolveConfig 而不是 fetchAgentConfig：平台在这一刻不可达（重部署、
	// 网络抖）时退避重试，不让 agent 一崩了之；只有平台**明确拒绝**（token
	// 无效、版本不匹配）才立刻退出（上一轮实测：一次平台抖动让全体 agent
	// CrashLoop，正是这条要消除的）。
	preflight, cancelPre := context.WithTimeout(ctx, timeout)
	cfg, err := resolveConfig(preflight, httpClient, opts.platformURL, token, defaultPreflightBackoff, logger)
	cancelPre()
	if err != nil {
		return err
	}
	logger.Info("agent starting", "cluster", cfg.ClusterID)

	// sink 现读 token（fileTokenReader），不抄启动时那一份：一把泄漏的 token
	// 被吊销、换上新的之后，下一次上报就该带新的，不必重启整个 DaemonSet。
	// 启动时的 readToken 仍然做一次 fail-fast（上面），预检也用那一份 ——
	// 那是一次性的，与轮换无关。
	sink := newHTTPSinkReading(opts.platformURL, fileTokenReader(opts.tokenFile), httpClient)

	client, err := inClusterClient()
	if err != nil {
		// 客户端建不起来同样要留下痕迹：不上报的话，界面显示「这个集群还
		// 没有过任何一次资产采集」，与一个 agent 压根没被拉起来的集群
		// 一模一样。
		reportAborted(ctx, sink, cfg.ClusterID, snapshot.RunErrorClientUnavailable, logger)
		return err
	}

	// leader 选举跑在后台，结果反映到 gate 上。**每一轮现问**，不抄一份：
	// leadership 会变，而抄下来的答案会让刚接手的 Pod 永远不采资产、
	// 或让刚失去 leadership 的 Pod 继续采（两份会撞 observed_at 主键）。
	leaseNS, err := resolveNamespace(opts.leaseNamespace)
	if err != nil {
		return err
	}
	gate := newLeaderGate()
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		return errors.New("this pod has no hostname to use as a leader-election identity")
	}
	go runLeaderElection(ctx, client,
		leaseNS, opts.leaseName, identity, gate, logger)

	<-runLoops(ctx, loops{
		// conntrack：**每个 Pod 都跑**。少跑一个节点就是少一个节点的盲区，
		// 而盲区在库里与「这段时间没有流量」长得一模一样。
		flow: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return conntrackOnce(ctx, conntrackOptions{
				clusterID: cfg.ClusterID, tablePath: opts.tablePath,
				polls: opts.polls, interval: opts.pollInterval,
			}, sink, logger)
		},
		flowEvery: opts.flowEvery,
		// 资产：**只有 leader 跑**。每个节点都跑会让 650 Pod × N 节点的
		// API 压力落到 apiserver 上，且 N 份数据撞 observed_at 主键。
		assets: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			// fleet 传 nil：**归属由平台判定**（design doc §3.4）。把 fleet
			// 网段下发给每一个被管集群，等于把整个 fleet 的拓扑发出去。
			_, err := collectrun.Once(ctx, cfg.ClusterID, client, nil, sink, logger)
			return err
		},
		assetsEvery:   opts.assetsEvery,
		leaderFor:     gate.isLeader,
		leaderChanged: gate.changed,
		// 心跳写失败只记日志、不停循环：卷写不动是个真问题，但让它顺着
		// 心跳把整条采集也停掉，是拿一个小故障换一个大盲区。写不动会自然
		// 表现为心跳变旧 → liveness 重启，路径本来就通。
		flowHeartbeat: func() {
			if err := writeHeartbeat(opts.heartbeatFile); err != nil {
				logger.Warn("could not write the liveness heartbeat", "err", err)
			}
		},
		logger: logger,
	})
	logger.Info("agent stopped", "cluster", cfg.ClusterID)
	return nil
}

// reportAborted 尽力上报一次没能开始的运行；失败只记日志。
//
// 上报失败不改变调用方要返回的那个错误：中止的成因才是操作者要看的东西，
// 把它换成一句「上报失败」等于用记账的失败盖住被记的那件事。
func reportAborted(
	ctx context.Context, sink *httpSink, clusterID string,
	reason snapshot.RunErrorReason, logger *slog.Logger,
) {
	runID, err := collectrun.NewRunID()
	if err != nil {
		logger.Warn("cannot mint a run id for an aborted push", "err", err)
		return
	}
	now := time.Now()
	if err := sink.SaveAbortedRun(ctx, clusterID, runID, now, now, reason); err != nil {
		logger.Warn("cannot report an aborted run to the platform", "err", err)
	}
}
