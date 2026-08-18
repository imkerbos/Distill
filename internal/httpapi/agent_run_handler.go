package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/collect"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// AgentSink 落库一次由 agent 推上来的采集运行。
//
// 收窄成一个方法而不是直接依赖 *snapshotstore.Store：摄入端点要做的只有
// 「把这一次运行存下来」，而那个 Store 还带着流量摄入、身份推导与一整套
// 读方法。边界层拿不到它们，就不存在「某次改动顺手让 API 推导了一次身份」
// 这条形状。
//
// **实现必须对 (cluster_id, run_id) 幂等**：agent 跑在 CronJob 里，网络
// 抖动重试是常态。已经存过时返回 snapshotstore.ErrRunExists，不要覆盖 ——
// 覆盖等于让后到的推送改写历史。
type AgentSink interface {
	Save(ctx context.Context, run snapshot.Run) error
}

// FleetSource 现读全 fleet 的网段登记，供归属判定使用。
//
// 每次请求现读而不是装配时抄一份：登记随时会变（新集群接入、网段改了），
// 抄一份等于把判定钉在进程启动那一刻，而那之后接入的集群会让每一个落在
// 它网段里的地址被判成 EXTERNAL —— 一个答得出、又不报错的错误结论。
type FleetSource func(ctx context.Context) (*cluster.Registry, error)

// agentPodPayload 是 agent 上报的一个 Pod。
//
// **没有 ipScope / ipScopeReason 字段。** 归属由平台判定（design doc §3.4），
// 而字段不存在，agent 就连声称一个归属的语法都没有 —— 比收下再覆盖更强：
// 后者依赖覆盖那一步不被谁删掉。
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

// agentObservationPayload 是一次采集观测到的资产。
//
// 本轮只带 Pod：G1 要证明的是这条链路本身（身份、认证、归属判定、落库），
// 其余资源类型跟着 G2 的分批上报一起加。少带一类的后果是那一类没有数据，
// 而**不是**一个看起来完整的错误结果。
type agentObservationPayload struct {
	Pods []agentPodPayload `json:"pods"`
}

// agentRunPayload 是一次资产上报的报文。
//
// **没有 clusterId 字段。** 集群归属只来自 token（design doc §2），而字段
// 不存在意味着带上它的报文会被 DisallowUnknownFields 整体拒绝 —— 这正是
// 期望行为：忽略那个字段会让一个装错 token 的 agent 静默地把数据写进别的
// 集群，而它自己以为写对了。
type agentRunPayload struct {
	SchemaVersion int                     `json:"schemaVersion"`
	RunID         string                  `json:"runId"`
	Status        string                  `json:"status"`
	StartedAt     time.Time               `json:"startedAt"`
	FinishedAt    time.Time               `json:"finishedAt"`
	ObservedAt    time.Time               `json:"observedAt"`
	Observation   agentObservationPayload `json:"observation"`
}

// handleAgentCollectionRun 收下一次由集群自己推上来的资产采集结果。
//
// 顺序是刻意的：先归属、再解码、再版本、再判定、最后落库。任何一步不成立
// 都不进入下一步 —— 半份被接受的上报比整份被拒的上报难查得多。
func handleAgentCollectionRun(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID, ok := AgentClusterFrom(r.Context())
		if !ok {
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeAgentUnauthenticated)
			return
		}
		if d.AgentSink == nil || d.Fleet == nil {
			// 没有落库端的部署形态收不下推送。答"依赖不可用"而不是成功：
			// 答成功会让 agent 把这一轮当成已经交付，那批观测就此丢了。
			d.Logger.Error("an agent pushed a run but this deployment has no sink",
				"cluster", clusterID, "request_id", RequestIDFrom(r.Context()))
			response.WriteSystem(w, http.StatusServiceUnavailable, response.CodeDependencyUnavailable)
			return
		}

		payload, ok := decodeAgentRun(w, r, d, clusterID)
		if !ok {
			return
		}

		fleet, err := d.Fleet(r.Context())
		if err != nil {
			d.Logger.Error("cannot read the fleet registration for classification",
				"err", err, "cluster", clusterID, "request_id", RequestIDFrom(r.Context()))
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}
		// 归属判定在平台侧发生，用的是与 PULL 完全相同的那个函数
		// （design doc §3.4）。两条路径调同一个实现，不是两份。
		run := collect.Classify(payload.toRun(clusterID), fleet)

		if err := d.AgentSink.Save(r.Context(), run); err != nil {
			if errors.Is(err, snapshotstore.ErrRunExists) {
				// 重复的一次是同一次采集又说了一遍，不是失败。答成功让
				// agent 停止重试；已存的那份不动。
				d.Logger.Info("an agent re-sent a run that was already stored",
					"cluster", clusterID, "runId", payload.RunID,
					"request_id", RequestIDFrom(r.Context()))
				response.WriteOK(w, map[string]string{"runId": payload.RunID})
				return
			}
			d.Logger.Error("cannot store a pushed collection run",
				"err", err, "cluster", clusterID, "runId", payload.RunID,
				"request_id", RequestIDFrom(r.Context()))
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}
		response.WriteOK(w, map[string]string{"runId": payload.RunID})
	}
}

// decodeAgentRun 解码并校验一次上报；不通过时已经写好响应。
func decodeAgentRun(
	w http.ResponseWriter, r *http.Request, d Deps, clusterID string,
) (agentRunPayload, bool) {
	dec := json.NewDecoder(r.Body)
	// **未知字段一律拒绝。** 契约版本对得上、却带着平台不认识的字段，说明
	// 两边对同一个版本的理解不一致 —— 那比版本号对不上更危险，因为版本
	// 检查拦不住它。这也是「报文里带 clusterId 要拒绝」的落点。
	dec.DisallowUnknownFields()

	var payload agentRunPayload
	if err := dec.Decode(&payload); err != nil {
		// 解码错误不外传：它会带上字段名与结构体类型名（规范 §22）。
		d.Logger.Warn("an agent sent an undecodable run",
			"err", err, "cluster", clusterID, "request_id", RequestIDFrom(r.Context()))
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentRunPayload{}, false
	}
	if payload.SchemaVersion != agentSchemaVersion {
		d.Logger.Warn("an agent reported an unsupported schema version",
			"cluster", clusterID, "version", payload.SchemaVersion,
			"request_id", RequestIDFrom(r.Context()))
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentRunPayload{}, false
	}
	if payload.RunID == "" {
		// run_id 是幂等键：没有它，一次重试会变成第二份历史记录。
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentRunPayload{}, false
	}
	if !validRunStatus(payload.Status) {
		response.WriteBusiness(w, response.CodeInvalidParam)
		return agentRunPayload{}, false
	}
	return payload, true
}

// toRun 把上报的报文变成一次采集运行。
//
// clusterID 由调用方从 token 取，**不从报文取**（design doc §2）：这是整条
// 摄入路径上唯一一处写下集群归属的地方。
func (p agentRunPayload) toRun(clusterID string) snapshot.Run {
	pods := make([]snapshot.Pod, 0, len(p.Observation.Pods))
	for _, in := range p.Observation.Pods {
		pods = append(pods, snapshot.Pod{
			ClusterID:      clusterID,
			Namespace:      in.Namespace,
			Name:           in.Name,
			UID:            in.UID,
			Phase:          in.Phase,
			IP:             in.IP,
			Labels:         in.Labels,
			HostNetwork:    in.HostNetwork,
			NodeName:       in.NodeName,
			ServiceAccount: in.ServiceAccount,
			OwnerKind:      in.OwnerKind,
			OwnerName:      in.OwnerName,
			WorkloadKind:   in.WorkloadKind,
			WorkloadName:   in.WorkloadName,
		})
	}
	return snapshot.Run{
		Status:     snapshot.RunStatus(p.Status),
		StartedAt:  p.StartedAt,
		FinishedAt: p.FinishedAt,
		Observation: snapshot.Observation{
			ClusterID:  clusterID,
			RunID:      p.RunID,
			ObservedAt: p.ObservedAt,
			Pods:       pods,
		},
	}
}

// validRunStatus 判断上报的运行结果是否在封闭枚举内。
//
// 在边界上校验而不是原样落库：status 在库里只是一列 VARCHAR，封闭性由
// Go 侧保证（CLAUDE.md §3）。而这个取值来自别人集群里的进程 —— 放进去一个
// 不认识的字符串，可见面会渲染出一个没人定义过的状态。
//
// 用显式 switch 而非查表：新增取值却忘了加进这里，switch 让这处遗漏在
// review 时是看得见的一行。
func validRunStatus(s string) bool {
	switch snapshot.RunStatus(s) {
	case snapshot.RunOK, snapshot.RunPartial, snapshot.RunFailed:
		return true
	default:
		return false
	}
}
