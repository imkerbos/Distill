package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/collect"
	"github.com/imkerbos/Distill/internal/identityderive"
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
	// SaveAbortedRun 记下一次**在读到集群之前就失败**的运行。
	//
	// 与 Save 分开而不是给它传一个空 Observation，理由同
	// snapshotstore.SaveAbortedRun：Save 会为每一类资源写一行计数、值为 0，
	// 而那些 0 在这里全是假话 —— 一个资源都没被尝试过。落成一排 0 会让
	// 界面显示「采到了零个 Pod、零个 Service」，读起来像一个空集群。
	SaveAbortedRun(
		ctx context.Context, clusterID, runID string,
		startedAt, finishedAt time.Time, reason snapshot.RunErrorReason,
	) error
}

// AgentDeriver 为一次已落库的采集运行推导 Pod 身份区间。
//
// 与 AgentSink 分成两个接口而不是并成一个（同 collector 侧 runStore /
// deriveStore 的拆法）：推导的成败落在自己的记录上，分开的接口让「顺手把
// 推导结果塞进采集那张表」在类型层面就没有落脚点。
//
// *snapshotstore.Store 满足它，identityderive.Once 消费它。
type AgentDeriver = identityderive.Store

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
// agentAdminPolicyPayload 是 agent 上报的一条管理面策略。
//
// priority 与 priorityKnown 分两个字段：0 是合法且**最高**的 ANP 优先级，
// 拿 0 兼表"没读到"，会把一条读不懂的策略排到所有策略之前。
type agentAdminPolicyPayload struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	UID           string `json:"uid,omitempty"`
	Priority      int32  `json:"priority,omitempty"`
	PriorityKnown bool   `json:"priorityKnown,omitempty"`
	Manifest      string `json:"manifest"`
}

type agentObservationPayload struct {
	Namespaces []agentNamespacePayload `json:"namespaces,omitempty"`
	Pods       []agentPodPayload       `json:"pods,omitempty"`
	Nodes      []agentNodePayload      `json:"nodes,omitempty"`
	Services   []agentServicePayload   `json:"services,omitempty"`
	Endpoints  []agentEndpointsPayload `json:"endpoints,omitempty"`
	Policies   []agentPolicyPayload    `json:"policies,omitempty"`
	Gateways   []agentGatewayPayload   `json:"gateways,omitempty"`
	// AdminPolicies 是 ANP 与 BANP 的原文。
	//
	// 收下它，哪怕这个集群还没声明由平台解释这个平面：不收的后果不是
	// "少一类数据"，是那一轮的资源计数写下 ADMINNETWORKPOLICY = 0 ——
	// 而 0 的含义是「采过了，就是没有」，与「agent 根本没送」分不开。
	AdminPolicies []agentAdminPolicyPayload `json:"adminPolicies,omitempty"`
	// Warnings 是采集当时发现的、事实与登记不符的地方。
	//
	// 一并上报而不是丢掉：它们的条数与构成要进可见面与统计口径，而 agent
	// 是唯一看见过那一刻的人。归属判定产生的告警由平台在 Classify 里另外
	// 追加，两批合在一起才是这次运行的全貌。
	Warnings []agentWarningPayload `json:"warnings,omitempty"`
}

// 以下各类的字段与 internal/snapshot 一一对应，唯独**一律没有 ClusterID**：
// 集群归属只来自 token，由平台在 toRun 里逐条填上（design doc §2）。

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
	Name string `json:"name,omitempty"`
	Port int32  `json:"port"`
	// TargetPort 与 TargetPortName 分成两个字段，理由同 snapshot.ServicePort：
	// 命名端口合成一个 int32 会静默记成 0，而 0 是合法端口值 —— 一条指向
	// 端口 0 的规则永远匹配不上，外观却完全正常。
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
	// Manifest 是策略的 YAML 原文 —— 「集群当时是什么样」的证据。解析可以
	// 重做，丢掉的字段找不回来。
	Manifest string `json:"manifest"`
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

// agentRunPayload 是一次资产上报的报文。
//
// **没有 clusterId 字段。** 集群归属只来自 token（design doc §2），而字段
// 不存在意味着带上它的报文会被 DisallowUnknownFields 整体拒绝 —— 这正是
// 期望行为：忽略那个字段会让一个装错 token 的 agent 静默地把数据写进别的
// 集群，而它自己以为写对了。
type agentRunPayload struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	Status        string `json:"status"`
	// ErrorReason 非空表示这一轮**根本没能开始采集**（凭据取不到、客户端
	// 建不起来、只读自证没过）。此时 Observation 里不会有任何资产。
	//
	// 上报它而不是让 agent 干脆不上报：不留痕迹的话，界面显示「这个集群
	// 还没有过任何一次资产采集」—— 与一个 agent 压根没被拉起来的集群
	// 一模一样，操作者会去等一次永远不会成功的采集。
	ErrorReason string                  `json:"errorReason,omitempty"`
	StartedAt   time.Time               `json:"startedAt"`
	FinishedAt  time.Time               `json:"finishedAt"`
	ObservedAt  time.Time               `json:"observedAt"`
	Observation agentObservationPayload `json:"observation"`
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
		if d.AgentSink == nil || d.Fleet == nil || d.AgentDeriver == nil {
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

		// 中止的运行走另一条落库路径：它没有观测，也就没有可判定的归属。
		// 走 Save 会为每一类资源写一行 0，而那些 0 全是假话。
		if payload.ErrorReason != "" {
			if err := d.AgentSink.SaveAbortedRun(r.Context(), clusterID, payload.RunID,
				payload.StartedAt, payload.FinishedAt,
				snapshot.RunErrorReason(payload.ErrorReason)); err != nil {
				if errors.Is(err, snapshotstore.ErrRunExists) {
					response.WriteOK(w, map[string]string{"runId": payload.RunID})
					return
				}
				d.Logger.Error("cannot store a pushed aborted run",
					"err", err, "cluster", clusterID, "runId", payload.RunID,
					"request_id", RequestIDFrom(r.Context()))
				response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
				return
			}
			response.WriteOK(w, map[string]string{"runId": payload.RunID})
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
			// **重复的一次不是失败，但也不是「什么都不用做」。** 上一次可能
			// 是落库成功、推导失败之后 agent 重推的 —— 直接答成功会让那次
			// 失败的推导永远补不上，而那个集群会一直停在「资产有了、身份
			// 没有」的状态。已存的那份不动，推导照跑。
			if errors.Is(err, snapshotstore.ErrObservationExists) {
				// 另一次运行占了这个时刻。这不是服务故障，也不是「这一次
				// 已经交付过了」—— 答一个说得出成因的业务码，操作者才会去
				// 查为什么这个集群同时跑着两个采集器。
				d.Logger.Warn("two collectors are pushing the same cluster at the same instant",
					"cluster", clusterID, "runId", payload.RunID,
					"request_id", RequestIDFrom(r.Context()))
				response.WriteBusiness(w, response.CodeConcurrentCollection)
				return
			}
			if !errors.Is(err, snapshotstore.ErrRunExists) {
				d.Logger.Error("cannot store a pushed collection run",
					"err", err, "cluster", clusterID, "runId", payload.RunID,
					"request_id", RequestIDFrom(r.Context()))
				response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
				return
			}
			d.Logger.Info("an agent re-sent a run that was already stored",
				"cluster", clusterID, "runId", payload.RunID,
				"request_id", RequestIDFrom(r.Context()))
		}

		// 推导紧跟落库，在同一次请求里（design doc 2026-08-18）。
		//
		// **同步而不是丢进后台**：丢进后台就要回答「进程重启时那些没跑完的
		// 推导去哪了」，而答案会是「没有人知道」—— 一个资产有了、身份没有的
		// 集群，在界面上与一个完整采集的集群一模一样。同步的代价是这次请求
		// 慢一点，而调用方是 CronJob，它不在乎。
		//
		// 推导失败要**答失败**：agent 会重推，而重推会再推导一次，顺序重推
		// 是幂等的（snapshotstore 那条集成用例钉着这一点），于是这条路径
		// 自愈。答成功则那次失败永远补不上。
		if err := identityderive.Once(r.Context(), clusterID, payload.RunID,
			d.AgentDeriver, d.Logger); err != nil {
			// 拿不到互斥说明这个集群同时跑着另一次采集 —— 与观测撞车是同一
			// 件事的另一个阶段，处置也相同。不是服务故障，答业务码；答
			// 「服务内部错误」会把人支去查平台，而平台什么问题都没有。
			if errors.Is(err, snapshotstore.ErrDeriveInProgress) {
				d.Logger.Warn("two collections are in flight for this cluster",
					"cluster", clusterID, "runId", payload.RunID,
					"request_id", RequestIDFrom(r.Context()))
				response.WriteBusiness(w, response.CodeConcurrentCollection)
				return
			}
			// 其余原因已经由 identityderive 落进 identity_derive_run 并记了
			// 日志。这里不再重复它的文本 —— 那段文本里带着底层错误。
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
	if payload.ErrorReason != "" {
		// 两条约束缺一不可。**原因必须在封闭枚举内**：它落进
		// collection_run.error_reason，那一列的封闭性只由 Go 侧保证
		// （CLAUDE.md §3），而这个取值来自别人集群里的进程。
		if !validRunErrorReason(payload.ErrorReason) {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return agentRunPayload{}, false
		}
		// **一轮没能开始的运行不可能带着资产。** 两者同时出现说明报文自相
		// 矛盾，而收下它会让一份「失败但有数据」的运行进库 —— 之后没有
		// 任何一屏知道该信哪一半。
		if len(payload.Observation.Pods) != 0 || payload.Status != string(snapshot.RunFailed) {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return agentRunPayload{}, false
		}
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
	namespaces := make([]snapshot.Namespace, 0, len(p.Observation.Namespaces))
	for _, in := range p.Observation.Namespaces {
		namespaces = append(namespaces, snapshot.Namespace{
			ClusterID:  clusterID,
			Name:       in.Name,
			Labels:     in.Labels,
			InMesh:     in.InMesh,
			MeshSource: cluster.MeshSource(in.MeshSource),
			MeshDetail: in.MeshDetail,
		})
	}
	nodes := make([]snapshot.Node, 0, len(p.Observation.Nodes))
	for _, in := range p.Observation.Nodes {
		nodes = append(nodes, snapshot.Node{
			ClusterID:   clusterID,
			Name:        in.Name,
			PodCIDRs:    in.PodCIDRs,
			InternalIPs: in.InternalIPs,
		})
	}
	services := make([]snapshot.Service, 0, len(p.Observation.Services))
	for _, in := range p.Observation.Services {
		ports := make([]snapshot.ServicePort, 0, len(in.Ports))
		for _, sp := range in.Ports {
			ports = append(ports, snapshot.ServicePort{
				Name:           sp.Name,
				Port:           sp.Port,
				TargetPort:     sp.TargetPort,
				TargetPortName: sp.TargetPortName,
				Protocol:       sp.Protocol,
			})
		}
		services = append(services, snapshot.Service{
			ClusterID: clusterID,
			Namespace: in.Namespace,
			Name:      in.Name,
			Type:      in.Type,
			Selector:  in.Selector,
			ClusterIP: in.ClusterIP,
			Ports:     ports,
		})
	}
	endpoints := make([]snapshot.Endpoints, 0, len(p.Observation.Endpoints))
	for _, in := range p.Observation.Endpoints {
		endpoints = append(endpoints, snapshot.Endpoints{
			ClusterID: clusterID,
			Namespace: in.Namespace,
			Name:      in.Name,
			Addresses: in.Addresses,
			Ports:     in.Ports,
		})
	}
	policies := make([]snapshot.NetworkPolicy, 0, len(p.Observation.Policies))
	for _, in := range p.Observation.Policies {
		policies = append(policies, snapshot.NetworkPolicy{
			ClusterID: clusterID,
			Namespace: in.Namespace,
			Name:      in.Name,
			UID:       in.UID,
			Manifest:  in.Manifest,
		})
	}
	gateways := make([]snapshot.Gateway, 0, len(p.Observation.Gateways))
	for _, in := range p.Observation.Gateways {
		gateways = append(gateways, snapshot.Gateway{
			ClusterID:      clusterID,
			Namespace:      in.Namespace,
			Name:           in.Name,
			Kind:           in.Kind,
			BackendService: in.BackendService,
		})
	}
	warnings := make([]snapshot.Warning, 0, len(p.Observation.Warnings))
	for _, in := range p.Observation.Warnings {
		warnings = append(warnings, snapshot.Warning{
			Kind:    snapshot.WarningKind(in.Kind),
			Subject: in.Subject,
			Detail:  in.Detail,
		})
	}

	adminPolicies := make([]snapshot.AdminPolicy, 0, len(p.Observation.AdminPolicies))
	for _, a := range p.Observation.AdminPolicies {
		adminPolicies = append(adminPolicies, snapshot.AdminPolicy{
			ClusterID: clusterID,
			Kind:      snapshot.AdminPolicyKind(a.Kind),
			Name:      a.Name, UID: a.UID,
			Priority: a.Priority, PriorityKnown: a.PriorityKnown,
			Manifest: a.Manifest,
		})
	}

	return snapshot.Run{
		Status:     snapshot.RunStatus(p.Status),
		StartedAt:  p.StartedAt,
		FinishedAt: p.FinishedAt,
		Observation: snapshot.Observation{
			ClusterID:     clusterID,
			RunID:         p.RunID,
			ObservedAt:    p.ObservedAt,
			Namespaces:    namespaces,
			Pods:          pods,
			Nodes:         nodes,
			Services:      services,
			Endpoints:     endpoints,
			Policies:      policies,
			Gateways:      gateways,
			AdminPolicies: adminPolicies,
			Warnings:      warnings,
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

// validRunErrorReason 判断上报的中止原因是否在封闭枚举内。
//
// 理由同 validRunStatus：error_reason 在库里只是一列 VARCHAR，而这个取值
// 来自别人集群里的进程。放进去一个不认识的字符串，统计口径上就再也看不出
// 有一类原因被漏登记了。
//
// 空串不算合法取值：调用方用「非空」来判断这是不是一次中止运行，让空串
// 从这里通过等于让那个判断多一个含义。
func validRunErrorReason(s string) bool {
	switch snapshot.RunErrorReason(s) {
	case snapshot.RunErrorCredentialUnavailable,
		snapshot.RunErrorClientUnavailable,
		snapshot.RunErrorReadOnlyUnproven:
		return true
	default:
		return false
	}
}
