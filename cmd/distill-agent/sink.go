package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
	token  string
	client *http.Client
}

// newHTTPSink 构造一个推送 sink。
func newHTTPSink(base, token string) *httpSink {
	return &httpSink{
		base:   strings.TrimSuffix(base, "/"),
		token:  token,
		client: &http.Client{Timeout: pushTimeout},
	}
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
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode the collection run: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.base+"/api/v1/agent/collection-runs", bytes.NewReader(body))
	if err != nil {
		// 地址不回显：它是部署拓扑信息，而这个进程的输出终点是集群日志。
		return errors.New("cannot build the ingest request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

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
		// 日志。码足够定位，且它是封闭枚举。
		return fmt.Errorf("the platform refused this run (HTTP %d, code %d)", resp.StatusCode, env.Code)
	}
	return nil
}
