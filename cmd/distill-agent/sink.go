package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// agentSchemaVersion 是摄入契约的版本，必须与平台侧那一个一致。
//
// 两侧各写一个常量而不是共享一个：agent 跑在别人的集群里，升级节奏不受
// 平台控制 —— 共享常量会让两侧看起来永远一致，而现实里它们可能差好几个
// 版本。显式声明、由平台判定认不认识，才谈得上协商（design doc §4）。
const agentSchemaVersion = 1

// pushTimeout 是一次上报的上限。
//
// 必填：一个卡住的上报会让 CronJob 里的这个 Pod 一直挂着，而下一次
// CronJob 又会拉起一个新的 —— 挂住的进程会累积（规范 §24）。
const pushTimeout = 60 * time.Second

// httpSink 把一次采集结果推给平台，实现 runStore。
//
// **它的存在就是为了让「采集器持有平台数据库口令」这条路径在 PUSH 模式下
// 不存在**（design doc 2026-08-18 §1.2）：今天的采集器直连状态库，把那个
// 二进制原样塞进客户集群，等于把口令发出去。
type httpSink struct {
	base   string
	token  tokenReader
	client *http.Client
}

// tokenReader 现取一把当前的 agent token。
//
// **是一个函数，不是一个字符串**：token 会被轮换（吊销一把泄漏的、换一把新
// 的），而一个在启动时抄下来的字符串要等整个 DaemonSet 重启才换得掉 ——
// 重启前那个 Pod 一直在用旧 token 推。现取则让轮换在 kubelet 同步完挂载的
// Secret 之后自然生效，不必重启。
type tokenReader func() (string, error)

// staticTokenReader 把一个固定的 token 包成 reader。测试与不需要轮换的调用
// 方用它。
func staticTokenReader(token string) tokenReader {
	return func() (string, error) { return token, nil }
}

// fileTokenReader 每次都从挂载的 Secret 文件现读。
//
// kubelet 更新 Secret 挂载是原子的（symlink 换名），读到的永远是完整的旧
// 值或完整的新值，不会是半截 —— 因此这里不必自己做原子性。空文件报错，
// 不当成一把空 token 发出去。错误里不带路径（规范 §19、§22）。
func fileTokenReader(path string) tokenReader {
	return func() (string, error) {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from this process's own flags, not a request.
		if err != nil {
			return "", errors.New("the agent token file could not be read")
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", errors.New("the agent token file is empty")
		}
		return token, nil
	}
}

// newHTTPSink 构造一个用系统根证书、固定 token 的推送 sink。
//
// 内部 CA 的部署走 newHTTPSinkWithClient —— 那条路把一个 pin 了 CA 的 client
// 注进来。默认这条留给公共 CA 与测试。
func newHTTPSink(base, token string) *httpSink {
	return newHTTPSinkWithClient(base, token, agentClient(nil))
}

// newHTTPSinkWithClient 用一个现成的 client、固定 token 构造 sink。
//
// client 由 run() 建一次、同时喂给 sink 与配置预检：两条路径带的是同一把
// token，必须信任同一组 CA、遵守同一条「不跟重定向」。各建各的会让「内部 CA
// 只配了一半」这种半通半不通的状态成为可能。
func newHTTPSinkWithClient(base, token string, client *http.Client) *httpSink {
	return newHTTPSinkReading(base, staticTokenReader(token), client)
}

// newHTTPSinkReading 用一个现取 token 的 reader 构造 sink。
//
// 常驻的 agent 走这条：token 从文件现读，轮换不必重启。
func newHTTPSinkReading(base string, token tokenReader, client *http.Client) *httpSink {
	return &httpSink{
		base:   strings.TrimSuffix(base, "/"),
		token:  token,
		client: client,
	}
}

// refuseRedirect 是共享 client 的 CheckRedirect。
//
// **不跟随重定向。** Go 默认会跟，而 https → http 的**同主机**降级重定向不会
// 剥掉 Authorization 头 —— 那把 token 于是明文发了出去。平台的两个端点都没有
// 任何理由重定向，因此干脆不跟：一次意外的重定向要以失败的形态出现，而不是
// 以一次成功但泄漏了凭据的请求出现。
//
// 错误里不带目标地址：那是部署拓扑信息，而这个进程的输出终点是被管集群的
// 日志（规范 §19、§22）。
func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("the platform answered with a redirect; refusing to follow it")
}

// agentClient 建一个 agent 用的 HTTP client。
//
// pool 为 nil 时用系统根证书；非 nil 时**只**信任 pool 里的 CA（RootCAs 一旦
// 设置就替换掉系统根，不是叠加）—— 这正是内部 CA 的部署要的：平台证书由内部
// CA 签，就该由内部 CA 验，不该因为系统根里恰好有别的东西而放宽。
//
// 从 http.DefaultTransport 克隆而不是新建一个空的：那样会丢掉 ProxyFromEnvironment
// 与各项连接超时，而集群里到平台常要过一个出口代理。MinVersion 明钉 TLS 1.2 ——
// 与 Go 当前默认一致，写出来是为了它不随某次依赖升级悄悄降下去。
func agentClient(pool *x509.CertPool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone() //nolint:errcheck // DefaultTransport is always *http.Transport
	if pool != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Timeout:       pushTimeout,
		Transport:     tr,
		CheckRedirect: refuseRedirect,
	}
}

// loadCAPool 把一份 PEM 证书包读成一个证书池。
//
// 空路径返回 (nil, nil)：不 pin，用系统根 —— 多数部署走公共 CA，-ca-file 可选。
//
// **认不出任何证书时报错，不静默退回系统根。** 静默退回的后果是运维以为自己
// 钉了内部 CA、实际走系统根 —— 内部 CA 签的证书于是被拒，而错误看起来像
// 「网络不通」，查错方向全错。错误里不带路径（规范 §19、§22）。
func loadCAPool(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(caFile) //nolint:gosec // G304: path comes from this process's own flags, not a request.
	if err != nil {
		return nil, errors.New("the CA bundle file could not be read")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("the CA bundle file contained no valid PEM certificate")
	}
	return pool, nil
}

// agentPodPayload 是上报的一个 Pod。
//
// **没有 ipScope 字段**：归属是平台的判定（design doc §3.4），这一侧连
// 声称它的语法都不该有。也**没有 clusterId**：归属只来自 token（§2）。
type agentPodPayload struct {
	Namespace      string            `json:"namespace"`
	Name           string            `json:"name"`
	UID            string            `json:"uid"`
	Phase          string            `json:"phase"`
	IP             string            `json:"ip"`
	Labels         map[string]string `json:"labels,omitempty"`
	HostNetwork    bool              `json:"hostNetwork,omitempty"`
	NodeName       string            `json:"nodeName,omitempty"`
	ServiceAccount string            `json:"serviceAccount,omitempty"`
	OwnerKind      string            `json:"ownerKind,omitempty"`
	OwnerName      string            `json:"ownerName,omitempty"`
	WorkloadKind   string            `json:"workloadKind,omitempty"`
	WorkloadName   string            `json:"workloadName,omitempty"`
}

// 以下各类与平台侧的报文结构一一对应，唯独**一律没有 ClusterID**：
// 归属只来自 token（design doc §2），带上会被平台按未知字段整体拒。

type agentNamespacePayload struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels,omitempty"`
	InMesh     bool              `json:"inMesh,omitempty"`
	MeshSource string            `json:"meshSource,omitempty"`
	MeshDetail string            `json:"meshDetail,omitempty"`
}

type agentNodePayload struct {
	Name        string   `json:"name"`
	PodCIDRs    []string `json:"podCidrs,omitempty"`
	InternalIPs []string `json:"internalIps,omitempty"`
}

type agentServicePortPayload struct {
	Name           string `json:"name,omitempty"`
	Port           int32  `json:"port"`
	TargetPort     int32  `json:"targetPort,omitempty"`
	TargetPortName string `json:"targetPortName,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
}

type agentServicePayload struct {
	Namespace string                    `json:"namespace"`
	Name      string                    `json:"name"`
	Type      string                    `json:"type,omitempty"`
	Selector  map[string]string         `json:"selector,omitempty"`
	ClusterIP string                    `json:"clusterIp,omitempty"`
	Ports     []agentServicePortPayload `json:"ports,omitempty"`
}

type agentEndpointsPayload struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
	Ports     []int32  `json:"ports,omitempty"`
}

type agentPolicyPayload struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Manifest  string `json:"manifest"`
}

type agentGatewayPayload struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	BackendService string `json:"backendService,omitempty"`
}

type agentWarningPayload struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail,omitempty"`
}

// agentObservationPayload 是一次采集观测到的全部资产。
//
// **每一类都要带上。** 少带一类不是「那一类没有数据」，而是平台落库时为它
// 写一个 0，而 0 的含义是「尝试了、集群里就是没有」—— 一个 push 集群会显示
// 成没有任何 NetworkPolicy，策略生成据此把它当作裸集群处理。
type agentObservationPayload struct {
	Namespaces []agentNamespacePayload `json:"namespaces,omitempty"`
	Pods       []agentPodPayload       `json:"pods,omitempty"`
	Nodes      []agentNodePayload      `json:"nodes,omitempty"`
	Services   []agentServicePayload   `json:"services,omitempty"`
	Endpoints  []agentEndpointsPayload `json:"endpoints,omitempty"`
	Policies   []agentPolicyPayload    `json:"policies,omitempty"`
	Gateways   []agentGatewayPayload   `json:"gateways,omitempty"`
	Warnings   []agentWarningPayload   `json:"warnings,omitempty"`
}

// agentRunPayload 是一次上报的报文，形状与平台侧一一对应。
type agentRunPayload struct {
	SchemaVersion int                     `json:"schemaVersion"`
	RunID         string                  `json:"runId"`
	Status        string                  `json:"status"`
	ErrorReason   string                  `json:"errorReason,omitempty"`
	StartedAt     time.Time               `json:"startedAt"`
	FinishedAt    time.Time               `json:"finishedAt"`
	ObservedAt    time.Time               `json:"observedAt"`
	Observation   agentObservationPayload `json:"observation"`
}

// Save 上报一次采集运行。
func (s *httpSink) Save(ctx context.Context, run snapshot.Run) error {
	payload := agentRunPayload{
		SchemaVersion: agentSchemaVersion,
		RunID:         run.Observation.RunID,
		Status:        string(run.Status),
		StartedAt:     run.StartedAt,
		FinishedAt:    run.FinishedAt,
		ObservedAt:    run.Observation.ObservedAt,
	}
	for _, n := range run.Observation.Namespaces {
		payload.Observation.Namespaces = append(payload.Observation.Namespaces, agentNamespacePayload{
			Name:       n.Name,
			Labels:     n.Labels,
			InMesh:     n.InMesh,
			MeshSource: string(n.MeshSource),
			MeshDetail: n.MeshDetail,
		})
	}
	for _, n := range run.Observation.Nodes {
		payload.Observation.Nodes = append(payload.Observation.Nodes, agentNodePayload{
			Name:        n.Name,
			PodCIDRs:    n.PodCIDRs,
			InternalIPs: n.InternalIPs,
		})
	}
	for _, svc := range run.Observation.Services {
		ports := make([]agentServicePortPayload, 0, len(svc.Ports))
		for _, sp := range svc.Ports {
			ports = append(ports, agentServicePortPayload{
				Name:           sp.Name,
				Port:           sp.Port,
				TargetPort:     sp.TargetPort,
				TargetPortName: sp.TargetPortName,
				Protocol:       sp.Protocol,
			})
		}
		payload.Observation.Services = append(payload.Observation.Services, agentServicePayload{
			Namespace: svc.Namespace,
			Name:      svc.Name,
			Type:      svc.Type,
			Selector:  svc.Selector,
			ClusterIP: svc.ClusterIP,
			Ports:     ports,
		})
	}
	for _, e := range run.Observation.Endpoints {
		payload.Observation.Endpoints = append(payload.Observation.Endpoints, agentEndpointsPayload{
			Namespace: e.Namespace,
			Name:      e.Name,
			Addresses: e.Addresses,
			Ports:     e.Ports,
		})
	}
	for _, pol := range run.Observation.Policies {
		payload.Observation.Policies = append(payload.Observation.Policies, agentPolicyPayload{
			Namespace: pol.Namespace,
			Name:      pol.Name,
			UID:       pol.UID,
			Manifest:  pol.Manifest,
		})
	}
	for _, g := range run.Observation.Gateways {
		payload.Observation.Gateways = append(payload.Observation.Gateways, agentGatewayPayload{
			Namespace:      g.Namespace,
			Name:           g.Name,
			Kind:           g.Kind,
			BackendService: g.BackendService,
		})
	}
	for _, w := range run.Observation.Warnings {
		payload.Observation.Warnings = append(payload.Observation.Warnings, agentWarningPayload{
			Kind:    string(w.Kind),
			Subject: w.Subject,
			Detail:  w.Detail,
		})
	}
	for _, p := range run.Observation.Pods {
		payload.Observation.Pods = append(payload.Observation.Pods, agentPodPayload{
			Namespace:      p.Namespace,
			Name:           p.Name,
			UID:            p.UID,
			Phase:          p.Phase,
			IP:             p.IP,
			Labels:         p.Labels,
			HostNetwork:    p.HostNetwork,
			NodeName:       p.NodeName,
			ServiceAccount: p.ServiceAccount,
			OwnerKind:      p.OwnerKind,
			OwnerName:      p.OwnerName,
			WorkloadKind:   p.WorkloadKind,
			WorkloadName:   p.WorkloadName,
		})
	}
	return s.post(ctx, payload)
}

// SaveAbortedRun 上报一次「还没开始采集就失败」的运行。
//
// clusterID 收下但不发出去：调用方按 runStore 的签名给它，而集群归属只能
// 来自 token（design doc §2）。发出去会被平台按未知字段整体拒绝。
func (s *httpSink) SaveAbortedRun(
	ctx context.Context, _, runID string,
	startedAt, finishedAt time.Time, reason snapshot.RunErrorReason,
) error {
	return s.post(ctx, agentRunPayload{
		SchemaVersion: agentSchemaVersion,
		RunID:         runID,
		// 中止的运行一律 FAILED 且不带任何资产：平台会校验这两者自洽。
		Status:      string(snapshot.RunFailed),
		ErrorReason: string(reason),
		StartedAt:   startedAt,
		FinishedAt:  finishedAt,
		ObservedAt:  finishedAt,
	})
}

// envelope 是平台响应的外壳，只取判定成败要用的两个字段。
type envelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// post 发出一次上报并判定成败。
//
// **非 0 的业务码是失败，不只是非 200 的 HTTP 状态。** 平台把业务失败写成
// HTTP 200 + code（response.WriteBusiness），只看状态码会让一次被拒的上报
// 看起来成功 —— 而那个 agent 会一直"正常工作"下去，它那个集群的数据停在
// 被拒的那一刻，页面上没有任何症状。
func (s *httpSink) post(ctx context.Context, payload agentRunPayload) error {
	return s.postTo(ctx, "/api/v1/agent/collection-runs", payload, "collection run")
}

// postTo 把一份报文 POST 到 agent 子树的某条路由上。
//
// 两条摄入路径（资产、流量）共用它，不是各写一份：这里的每一条守卫都是
// 「什么不该出现在集群日志里」—— 不回显地址、不包 net/http 的错误、
// 只读有限的一段响应。两份实现意味着两套守卫，而漏掉守卫的那一份会成为
// 一次凭据泄漏的入口。
func (s *httpSink) postTo(ctx context.Context, path string, payload any, what string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode the %s: %w", what, err)
	}

	// token 现取，不抄一份：轮换后的下一次上报就该带新 token。读不到就在
	// 这里失败，绝不带一个空 Authorization 发出去 —— 那会以一次「凭据无效」
	// 的形态到平台，把「Secret 挂载出了问题」误导成「这把 token 被吊销了」。
	token, err := s.token()
	if err != nil {
		return errors.New("the agent token could not be read")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.base+path, bytes.NewReader(body))
	if err != nil {
		// 地址不回显：它是部署拓扑信息，而这个进程的输出终点是集群日志。
		return errors.New("cannot build the ingest request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.client.Do(req)
	if err != nil {
		// **不包 err**：net/http 的错误文本里带着完整 URL，而 URL 之外
		// 还可能带上 Authorization 头的内容（重定向路径上尤其如此）。
		return errors.New("the platform could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()

	// 只读有限的一段：平台答什么都不该让这个进程按对端的意愿分配内存
	// （规范 §24）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.New("the platform's reply could not be read")
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// 答非 JSON 说明中间有东西（代理、门户）截了这次请求。
		return fmt.Errorf("the platform replied with something that is not a result (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || env.Code != 0 {
		// 带上业务码，不带 msg：msg 是平台写的文案，而这条错误会进集群
		// 日志。码是封闭枚举，足够定位。
		//
		// 少数几个码在这里翻成人话：这个进程的日志是运维在被管集群里唯一
		// 看得到的东西，而「code 20008」需要他去查平台才知道是什么意思。
		// 翻译用的是**这一侧自己写的句子**，不是把平台的 msg 转发出来。
		return fmt.Errorf("the platform refused this %s (HTTP %d, code %d)%s",
			what, resp.StatusCode, env.Code, hintFor(env.Code))
	}
	return nil
}

// hintFor 给少数几个码补一句这一侧写的说明。
//
// 只覆盖「运维照着这句话能做点什么」的那些。其余的留给码本身 —— 编一句
// 泛泛的解释，比只给一个码更糟：它看起来像结论。
func hintFor(code int) string {
	switch code {
	case int(response.CodeConcurrentCollection):
		return ": another collection is already in flight for this cluster; check that only one collector runs against it"
	case int(response.CodeAgentUnauthenticated):
		return ": this agent's token is unknown or has been revoked; it needs a new one"
	default:
		return ""
	}
}

// SaveFlowIngest 把一次流量摄入推给平台。
//
// 与 Save 走同一条出站链路（postTo）：那里的每一条守卫都是「什么不该出现在
// 集群日志里」—— 不回显地址、不包 net/http 的错误、只读有限的一段响应。
// 两条各自演化的出站路径意味着两套守卫，而漏掉守卫的那一条会成为一次凭据
// 泄漏的入口。
func (s *httpSink) SaveFlowIngest(ctx context.Context, p flowIngestPayload) error {
	return s.postTo(ctx, "/api/v1/agent/flow-ingests", p, "flow ingest")
}
