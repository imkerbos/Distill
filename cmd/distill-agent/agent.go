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
	// mode 决定这一次采什么。
	mode collectMode
	// tablePath / polls / pollInterval 只在 conntrack 模式下有意义。
	tablePath    string
	polls        int
	pollInterval time.Duration
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

// collectMode 是这个二进制的采集模式，封闭枚举。
//
// 显式 switch 校验而不是「认不出就当默认」：一个拼错了 -mode 的 DaemonSet
// 会安安静静地去采资产，而运维以为它在采流量 —— 那个集群于是永远显示
// 「还没有任何流量观测」，而没有任何东西说得出为什么。
type collectMode string

const (
	// modeAssets 采本集群资产，按集群一份（CronJob）。
	modeAssets collectMode = "assets"
	// modeConntrack 轮询本节点的 conntrack 表，按节点一份（DaemonSet）。
	modeConntrack collectMode = "conntrack"
)

func (m collectMode) valid() bool {
	switch m {
	case modeAssets, modeConntrack:
		return true
	default:
		return false
	}
}

func (o options) validate() error {
	if !o.mode.valid() {
		return fmt.Errorf("-mode must be %q or %q", modeAssets, modeConntrack)
	}
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

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg, err := fetchAgentConfig(ctx, opts.platformURL, token)
	if err != nil {
		return err
	}
	if opts.allowPlaintext {
		// 打开了明文就要一直吵。一个静默接受明文的 agent，在生产上没有
		// 任何东西会提醒你那把 token 正在裸奔。
		logger.Warn("this agent is talking to the platform over plaintext http; " +
			"its token crosses the network in the clear")
	}
	logger.Info("starting a push collection",
		"cluster", cfg.ClusterID, "mode", string(opts.mode))

	sink := newHTTPSink(opts.platformURL, token)

	if opts.mode == modeConntrack {
		// conntrack 模式不建 Kubernetes 客户端：它读的是本节点的
		// /proc/net/nf_conntrack，一个 API 都不调。不建也就不需要那份
		// ServiceAccount —— 权限面小一圈。
		return conntrackOnce(ctx, conntrackOptions{
			clusterID: cfg.ClusterID, tablePath: opts.tablePath,
			polls: opts.polls, interval: opts.pollInterval,
		}, sink, logger)
	}

	client, err := inClusterClient()
	if err != nil {
		// 客户端建不起来同样要留下痕迹：不上报的话，界面显示「这个集群还
		// 没有过任何一次资产采集」，与一个 agent 压根没被拉起来的集群
		// 一模一样。
		reportAborted(ctx, sink, cfg.ClusterID, snapshot.RunErrorClientUnavailable, logger)
		return err
	}

	// fleet 传 nil：**归属由平台判定**（design doc §3.4）。把 fleet 网段
	// 下发给每一个被管集群，等于把整个 fleet 的拓扑发出去。
	_, err = collectrun.Once(ctx, cfg.ClusterID, client, nil, sink, logger)
	return err
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
