package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// AgentFlowSink 落库一次由集群 agent 推上来的流量摄入。
//
// 与 AgentSink 分成两个接口而不是并成一个：资产与流量是两条独立的链路，
// 各自会失败、各自重试。合成一个接口意味着「没有流量来源的部署」也必须
// 提供一个假的摄入端，而那个假实现迟早会有人接上真的。
//
// **实现必须对 (cluster_id, run_id) 幂等**：agent 跑在 CronJob 里，网络抖动
// 重试是常态。已经存过时返回 snapshotstore.ErrIngestRunExists，不要覆盖 ——
// 覆盖等于让后到的推送改写历史。
//
// *snapshotstore.Store 满足它。
type AgentFlowSink interface {
	SaveIngest(ctx context.Context, run snapshotstore.IngestRun) error
}

// agentWindowPayload 是一段左闭右开的时间区间。
type agentWindowPayload struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func (w agentWindowPayload) toWindow() flow.Window {
	return flow.Window{From: w.From, To: w.To}
}

// agentConnectionPayload 是观测到的一条连接。
//
// **两端只有 IP。** 没有 namespace / workload / podUid —— 主体由平台在读侧
// 从 pod_identity_interval 解析（collectstore.subjectAt，且窗口内换过手一律
// 答 AMBIGUOUS）。让 agent 报身份等于开第二条解析路径，而两条会漂；
// 字段不存在比"收下再覆盖"更强，后者依赖覆盖那一步不被谁删掉。
//
// conntrack 本来也只有 IP，这条形状对它天然成立。
type agentConnectionPayload struct {
	SrcIP    string `json:"srcIp"`
	DstIP    string `json:"dstIp"`
	Protocol string `json:"protocol"`
	Port     int32  `json:"port"`
	// ObservedCount 是这个窗口内看到它多少次。它**不是**完整度：看见 3 次
	// 只说明至少发生过 3 次。
	ObservedCount int `json:"observedCount"`
	// Verdict 是来源报告的实际放行/拒绝。空表示来源不报这件事（conntrack
	// 就不报），非空则必须在封闭枚举内。
	Verdict string `json:"verdict,omitempty"`
}

// agentFlowIngestPayload 是一次流量摄入的报文。
//
// **没有 clusterId**：归属只来自 token。带上它会被 DisallowUnknownFields
// 整份拒掉，而那正是期望行为 —— 忽略它会让一个装错 token 的 agent 静默地
// 把流量写进别的集群，而它自己以为写对了。
//
// **没有 completeness**：完整度不是一个可以被填写的字段，而是证据的函数
// （flow.IngestResult 连 setter 都没有）。那条纪律在 wire 上也必须成立，
// 否则 agent 可以直接宣称 COMPLETE 而没人给过依据，下游据此不降级，
// 于是一批没被看见的连接被当成不存在。
type agentFlowIngestPayload struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	// Source 是这批连接的来源，必须在 flow.SourceKind 的封闭枚举内。
	Source string `json:"source"`
	Status string `json:"status"`
	// ErrorReason 非空表示这次摄入没能拿到数据。此时不得带任何连接。
	ErrorReason string    `json:"errorReason,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`

	// RequestedWindow 是这次要的窗口，CoveredWindow 是来源说自己实际覆盖到
	// 的那段。两者分开，是因为"我要一小时"与"它只给了我 40 分钟"必须能区分。
	//
	// CoveredWindow 缺席表示来源说不出自己覆盖了多少 —— 那是未知，不是
	// 全覆盖。
	RequestedWindow agentWindowPayload  `json:"requestedWindow"`
	CoveredWindow   *agentWindowPayload `json:"coveredWindow,omitempty"`

	// SampleRate 与 Dropped 是**指针**，缺席与零值必须分得开：
	//
	//   - dropped: 0 是证据（"来源说这一批一条没丢"）
	//   - dropped 缺席是没有证据（"来源不报这件事"）
	//   - sampleRate 缺席**不等于 1.0** —— 填 1.0 等于宣称"一条没漏"，
	//     而那是一句没人说过的话
	//
	// 用值类型的话 JSON 的零值会把"没有证据"变成"证明了完整"，而完整度是
	// COMPLETE 时下游不降级。这是本文件里最容易写错、错了最危险的一处。
	SampleRate *float64 `json:"sampleRate,omitempty"`
	Dropped    *uint64  `json:"dropped,omitempty"`

	Connections []agentConnectionPayload `json:"connections,omitempty"`
}

// handleAgentFlowIngest 收下一次由集群自己推上来的流量摄入。
//
// 顺序与资产那条一致：先归属、再依赖、再解码校验、最后落库。任何一步不成立
// 都不进入下一步 —— 半份被接受的上报比整份被拒的上报难查得多。
func handleAgentFlowIngest(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID, ok := AgentClusterFrom(r.Context())
		if !ok {
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeAgentUnauthenticated)
			return
		}
		if d.AgentFlowSink == nil {
			// 没有摄入端的部署收不下流量。答"依赖不可用"而不是成功：
			// 答成功会让 agent 把这一轮当成已经交付，那批观测就此丢了。
			d.Logger.Error("an agent pushed flows but this deployment has no flow sink",
				"cluster", clusterID, "request_id", RequestIDFrom(r.Context()))
			response.WriteSystem(w, http.StatusServiceUnavailable, response.CodeDependencyUnavailable)
			return
		}

		payload, ok := decodeAgentFlowIngest(w, r, d, clusterID)
		if !ok {
			return
		}
		run, ok := payload.toIngestRun(w, r, d, clusterID)
		if !ok {
			return
		}

		if err := d.AgentFlowSink.SaveIngest(r.Context(), run); err != nil {
			switch {
			case errors.Is(err, snapshotstore.ErrIngestRunExists):
				// 同一次摄入又说了一遍。已存的那份不动，答成功 —— 答失败会让
				// agent 接着重试，而每一次都会得到同样的结果。
				d.Logger.Info("an agent re-sent a flow ingest that was already stored",
					"cluster", clusterID, "ingestRunId", payload.RunID,
					"request_id", RequestIDFrom(r.Context()))
			case errors.Is(err, snapshotstore.ErrTooManyConnections):
				// 调用方一次要得太多，不是平台故障。答业务码 —— 一个 5xx 会让
				// agent 原样重试，而每一次都会得到同样的结果。
				d.Logger.Warn("an agent pushed more connections than one ingest may carry",
					"cluster", clusterID, "ingestRunId", payload.RunID,
					"connections", len(payload.Connections),
					"request_id", RequestIDFrom(r.Context()))
				response.WriteInvalid(w, flowIngestTooLargeMsg)
				return
			default:
				d.Logger.Error("cannot store a pushed flow ingest",
					"err", err, "cluster", clusterID, "ingestRunId", payload.RunID,
					"request_id", RequestIDFrom(r.Context()))
				response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
				return
			}
		}
		response.WriteOK(w, map[string]string{"runId": payload.RunID})
	}
}

// flowIngestTooLargeMsg 说明一次摄入为什么太大，以及该怎么办。
//
// 末句是刻意的：缩短窗口减少的是观测次数，不一定减少去重后的条数。含糊过去
// 会让人反复缩窗口而毫无进展 —— 真到了这一步要加的是分片摄入协议，而那是
// 另一轮（design doc 2026-08-18-agent-flow-ingest §7）。
const flowIngestTooLargeMsg = "这次摄入带的连接条数超过了单次上限，整份被拒 —— " +
	"截断会让一部分连接凭空消失，而它们的缺席看起来与「这段时间没有流量」一模一样。" +
	"请缩短观测窗口后重试。若缩短窗口之后条数仍然不降（去重后的连接数不随时间线性变化），" +
	"说明这个集群需要分片摄入，请联系平台。"

// decodeAgentFlowIngest 解码并校验一次摄入；不通过时已经写好响应。
func decodeAgentFlowIngest(
	w http.ResponseWriter, r *http.Request, d Deps, clusterID string,
) (agentFlowIngestPayload, bool) {
	dec := json.NewDecoder(r.Body)
	// **未知字段一律拒绝。** 这是「报文里带 clusterId 要拒绝」与「带任何身份
	// 字段要拒绝」的落点：那些字段不存在，于是带上它们的报文整份被拒，
	// 而不是被悄悄忽略。
	dec.DisallowUnknownFields()

	var p agentFlowIngestPayload
	if err := dec.Decode(&p); err != nil {
		// 解码错误不外传：它会带上字段名与结构体类型名（安全规范 §22）。
		d.Logger.Warn("an agent sent an undecodable flow ingest",
			"err", err, "cluster", clusterID, "request_id", RequestIDFrom(r.Context()))
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentFlowIngestPayload{}, false
	}
	if p.SchemaVersion != agentSchemaVersion {
		d.Logger.Warn("an agent reported an unsupported flow schema version",
			"cluster", clusterID, "version", p.SchemaVersion,
			"request_id", RequestIDFrom(r.Context()))
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentFlowIngestPayload{}, false
	}
	// run_id 是幂等键：没有它，一次重试会变成第二份历史记录。
	if p.RunID == "" {
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentFlowIngestPayload{}, false
	}
	if !validIngestStatus(p.Status) {
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentFlowIngestPayload{}, false
	}
	// **来源与两个窗口不在这里校验。** `flow.NewIngestResult` 已经是那三项的
	// 权威判据（未登记的来源、不合法的请求窗口、不合法的覆盖窗口都报错），
	// 而 `validateIngestRun` 在落库前还会再查一次来源。在边界层抄第四份，
	// 得到的是一条没有任何测试分辨得出来的分支 —— 摘掉它行为一个字节不变，
	// 而那正是"两处判据慢慢漂开"的起点。
	//
	// 这里校验的是 NewIngestResult **不管**的那些：状态、失败原因、协议、
	// 判定 —— 它们落进库里都只是一列 VARCHAR，封闭性只由 Go 侧保证
	// （CLAUDE.md §3），而取值来自别人集群里的进程。
	if p.CoveredWindow != nil && p.CoveredWindow.toWindow() == (flow.Window{}) {
		// 传一个全零的覆盖窗口与压根不传是两回事：后者是"来源说不出自己
		// 覆盖了多少"，前者是报文坏了。收下它会让"说不出"多一个成因不明的
		// 来源，而 NewIngestResult 对零值窗口是放行的（它把零值读作"没传"）。
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentFlowIngestPayload{}, false
	}
	if p.ErrorReason != "" {
		// **原因必须在封闭枚举内**：它落进 flow_ingest_run.error_reason，
		// 那一列的封闭性只由 Go 侧保证（CLAUDE.md §3），而这个取值来自
		// 别人集群里的进程。
		if !validIngestErrorReason(p.ErrorReason) {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return agentFlowIngestPayload{}, false
		}
		// **一次没能拿到数据的摄入不可能带着连接。** 两者同时出现说明报文
		// 自相矛盾，收下它会让一份「失败但有数据」的运行进库 —— 之后没有
		// 任何一屏知道该信哪一半（PULL 侧 ingestOnce 也是整份丢掉）。
		if len(p.Connections) != 0 || p.Status != string(snapshotstore.IngestFailed) {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return agentFlowIngestPayload{}, false
		}
	}
	if p.Status == string(snapshotstore.IngestFailed) && p.ErrorReason == "" {
		// FAILED 而没有原因的运行，在界面上与一次"摄入成功、这段时间确实
		// 没有流量"的运行长得一模一样。
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentFlowIngestPayload{}, false
	}
	for _, c := range p.Connections {
		if !flow.Protocol(c.Protocol).Valid() {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return agentFlowIngestPayload{}, false
		}
		// 空 verdict 是"来源不报"，合法；非空但未登记说明 agent 坏了。
		// 悄悄按"不报"处理会让一个坏掉的 agent 一直静默地少报判定。
		if c.Verdict != "" && !flow.Verdict(c.Verdict).Valid() {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return agentFlowIngestPayload{}, false
		}
	}
	return p, true
}

// toIngestRun 把报文变成一次摄入运行；不通过时已经写好响应。
//
// clusterID 由调用方从 token 取，**不从报文取**：这是整条摄入路径上唯一一处
// 写下集群归属的地方。
func (p agentFlowIngestPayload) toIngestRun(
	w http.ResponseWriter, r *http.Request, d Deps, clusterID string,
) (snapshotstore.IngestRun, bool) {
	conns := make([]flow.Connection, 0, len(p.Connections))
	for _, in := range p.Connections {
		c := flow.Connection{
			// 两端只带地址。主体在读侧解析 —— 见 agentConnectionPayload。
			Source:        flow.Endpoint{IP: in.SrcIP},
			Dest:          flow.Endpoint{IP: in.DstIP},
			Protocol:      flow.Protocol(in.Protocol),
			Port:          in.Port,
			ObservedCount: in.ObservedCount,
		}
		if in.Verdict != "" {
			c = c.WithVerdict(flow.Verdict(in.Verdict))
		}
		conns = append(conns, c)
	}

	var covered flow.Window
	if p.CoveredWindow != nil {
		covered = p.CoveredWindow.toWindow()
	}
	result, err := flow.NewIngestResult(
		flow.SourceKind(p.Source), p.RequestedWindow.toWindow(), covered, conns)
	if err != nil {
		// 这是来源与两个窗口的判据所在（见 decodeAgentFlowIngest 的说明）：
		// 未登记的来源、不合法的请求窗口、不合法的覆盖窗口都落在这里。
		// 报参数错误而不是 500 —— 错的是报文，不是平台。
		//
		// source 进日志：一个装错来源名的 agent，运维要能从日志里直接看出
		// 它报的是哪个词。err 也进日志但不进响应 —— 它带着字段名与类型名
		// （安全规范 §22）。
		d.Logger.Warn("an agent sent a flow ingest that could not be constructed",
			"err", err, "cluster", clusterID, "ingestRunId", p.RunID,
			"source", p.Source, "request_id", RequestIDFrom(r.Context()))
		response.WriteBusiness(w, response.CodeInvalidParam)
		return snapshotstore.IngestRun{}, false
	}

	// **只在来源真的报了的时候才附上证据。** 指针为 nil 就什么都不做 ——
	// 那正是"来源不报这件事"，而 WithSampleRate / WithDropped 一旦被调用，
	// 完整度的判定就多了一项依据。
	if p.SampleRate != nil {
		result = result.WithSampleRate(*p.SampleRate)
		if _, known := result.SampleRate(); !known {
			// WithSampleRate 对 (0,1] 之外的取值保持未知。**不拒绝整份摄入**：
			// 拒绝会让这段窗口一行都不落，于是它读起来是"没有流量"——
			// 正是这条路径最该避免的那件事。降级保住连接，完整度诚实地落到
			// UNKNOWN。但坏取值要留痕，否则它会一直静默降级下去。
			d.Logger.Warn("an agent reported a sample rate outside (0,1]; treating it as unknown",
				"cluster", clusterID, "ingestRunId", p.RunID, "sampleRate", *p.SampleRate,
				"request_id", RequestIDFrom(r.Context()))
		}
	}
	if p.Dropped != nil {
		result = result.WithDropped(*p.Dropped)
	}

	return snapshotstore.IngestRun{
		ClusterID:   clusterID,
		RunID:       p.RunID,
		StartedAt:   p.StartedAt,
		FinishedAt:  p.FinishedAt,
		Status:      snapshotstore.IngestStatus(p.Status),
		ErrorReason: snapshotstore.IngestErrorReason(p.ErrorReason),
		Result:      result,
	}, true
}

// validIngestStatus 判断上报的摄入结果是否在封闭枚举内。
//
// 用显式 switch 而非查表，理由同 validRunStatus：新增取值却忘了加进这里，
// switch 让这处遗漏在 review 时是看得见的一行。
func validIngestStatus(s string) bool {
	switch snapshotstore.IngestStatus(s) {
	case snapshotstore.IngestOK, snapshotstore.IngestPartial, snapshotstore.IngestFailed:
		return true
	default:
		return false
	}
}

// validIngestErrorReason 判断上报的失败原因是否在封闭枚举内。
//
// 空串不算合法取值：调用方用「非空」判断这是不是一次失败摄入，让空串从
// 这里通过等于让那个判断多一个含义。
func validIngestErrorReason(s string) bool {
	switch snapshotstore.IngestErrorReason(s) {
	case snapshotstore.IngestErrorUnreachable,
		snapshotstore.IngestErrorUnauthorized,
		snapshotstore.IngestErrorQuotaExhausted,
		snapshotstore.IngestErrorTimeout,
		snapshotstore.IngestErrorOther:
		return true
	default:
		return false
	}
}
