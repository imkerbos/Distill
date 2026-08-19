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

// fetchAgentConfig 问平台「我是哪个集群、该按哪个契约上报」。
//
// 集群归属由平台按 token 反查，agent 不自己声明（design doc §2）。这一步
// 同时是一次连通性与凭据的预检：token 被吊销时这里就失败，而不是采完
// 一整轮资产之后在最后一步失败。
func fetchAgentConfig(ctx context.Context, base, token string) (agentConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/api/v1/agent/config", nil)
	if err != nil {
		return agentConfig{}, errors.New("cannot build the config request")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: pushTimeout}
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
	if resp.StatusCode != http.StatusOK || envelope.Code != 0 {
		return agentConfig{}, fmt.Errorf("the platform refused this agent (HTTP %d, code %d)", resp.StatusCode, envelope.Code)
	}
	if envelope.Data.ClusterID == "" {
		return agentConfig{}, errors.New("the platform did not say which cluster this agent belongs to")
	}
	// 版本不认识就停在这里，不采：采完再发现对不上，等于白读一遍别人的
	// 集群（design doc §4）。
	if envelope.Data.SchemaVersion != agentSchemaVersion {
		return agentConfig{}, fmt.Errorf(
			"the platform speaks ingest schema %d and this collector speaks %d",
			envelope.Data.SchemaVersion, agentSchemaVersion)
	}
	return envelope.Data, nil
}

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

	// 预检用一个有限的超时；常驻循环本身不设总超时 —— 那是一个 DaemonSet，
	// 它的生命周期由 SIGTERM 结束，不由一个计时器结束。timeout 管的是
	// **单轮**（见下）。
	preflight, cancelPre := context.WithTimeout(ctx, timeout)
	cfg, err := fetchAgentConfig(preflight, opts.platformURL, token)
	cancelPre()
	if err != nil {
		return err
	}
	logger.Info("agent starting", "cluster", cfg.ClusterID)

	sink := newHTTPSink(opts.platformURL, token)

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
	var gate leaderGate
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		return errors.New("this pod has no hostname to use as a leader-election identity")
	}
	go runLeaderElection(ctx, client,
		leaseNS, opts.leaseName, identity, &gate, logger)

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
		assetsEvery: opts.assetsEvery,
		leaderFor:   gate.isLeader,
		logger:      logger,
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
