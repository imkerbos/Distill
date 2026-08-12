package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// clusterPayload 是集群注册与修改的请求体。
//
// 与 registry.Cluster 分开：接口形状属于边界层，直接复用领域类型会让
// 一次内部字段重命名变成一次不兼容的 API 变更。
type clusterPayload struct {
	ID                 string               `json:"id"`
	DisplayName        string               `json:"displayName"`
	PodCIDR            string               `json:"podCidr"`
	NodeCIDR           string               `json:"nodeCidr"`
	CCNPPresent        bool                 `json:"ccnpPresent"`
	State              string               `json:"state"`
	APIServers         []registry.APIServer `json:"apiServers"`
	HealthCheckSources []string             `json:"healthCheckSources"`
	Git                *gitPayload          `json:"git"`
}

// gitPayload 是 Git 绑定的请求体形状。
//
// 刻意**不含** lastWrittenCommit，而不是收下再忽略：策略存在 Git 里的
// 理由是双重审计 —— Git 保存记录，这个字段是平台对那份记录的声明，
// 漂移检测拿两者比对。允许调用方设置它，等于允许调用方让平台的说法与
// 仓库现状对上：基准从此无法被证伪，而平台会带着信心报告「无漂移」，
// 比根本没有基准更糟。
//
// 一个永远不会被采纳的字段留在请求形状里，只会让下一个调用方以为它有用；
// 它的值由 handleUpdateCluster 从库里的现值推导（见 carryDriftBaseline）。
//
// credentialRef 不在此列：它是 Secret Manager 里的引用，是操作者填写的
// 配置，不是平台对外部世界的断言 —— 由调用方提供正是它的设计意图。
//
// verifyResult 与 verifiedAt 同样**不在**请求形状里，理由与
// lastWrittenCommit 完全一样：结论是平台自己校验出来的事实，一个能被
// 请求体设置的 OK 就是一句无法被证伪的「可以下发」。它们由保存路径从
// verifyBinding 的返回值填入（见 applyVerdict）。
type gitPayload struct {
	RepoURL       string `json:"repoUrl"`
	Branch        string `json:"branch"`
	PolicyPath    string `json:"policyPath"`
	CredentialRef string `json:"credentialRef"`
}

// toBinding 把 Git 请求体转成领域对象，LastWrittenCommit 一律留空。
func (g *gitPayload) toBinding() *registry.GitBinding {
	if g == nil {
		return nil
	}
	return &registry.GitBinding{
		RepoURL:       g.RepoURL,
		Branch:        g.Branch,
		PolicyPath:    g.PolicyPath,
		CredentialRef: g.CredentialRef,
	}
}

// toCluster 把请求体转成领域对象。
//
// state 参数**完全忽略**：接入状态反映平台实际收到了什么，不是调用方的意愿。
// 只做「为空时兜底」是不够的 —— 那样一次显式的 {"state":"READY"} 仍会被接受，
// 等于允许把「还没有数据」标成「可以出推荐了」。创建一律从 REGISTERED 起步，
// 修改时保留库里已有的状态（见 handleUpdateCluster）。
//
// 漂移基准 lastWrittenCommit 同理：创建一律为空（平台还没往那个仓库写过
// 任何东西），修改时从库里的现值推导。
func (p clusterPayload) toCluster() registry.Cluster {
	return registry.Cluster{
		State:              registry.StateRegistered,
		ID:                 p.ID,
		DisplayName:        p.DisplayName,
		PodCIDR:            p.PodCIDR,
		NodeCIDR:           p.NodeCIDR,
		CCNPPresent:        p.CCNPPresent,
		APIServers:         p.APIServers,
		HealthCheckSources: p.HealthCheckSources,
		Git:                p.Git.toBinding(),
	}
}

// carryDriftBaseline 用库里的现值填回漂移基准。
//
// 绑定仍指向同一仓库同一分支时沿用：那个 commit 仍然描述平台最近一次
// 往这个目标写了什么。改指向或解除绑定时归零 —— 旧 SHA 指向的历史与新
// 目标无关，留着它会让漂移检测拿一个不相干的 commit 当基准，得出的
// 「有漂移／无漂移」两种结论都不成立。
func carryDriftBaseline(next, existing *registry.GitBinding) {
	if next == nil || existing == nil {
		return
	}
	if next.RepoURL == existing.RepoURL && next.Branch == existing.Branch {
		next.LastWrittenCommit = existing.LastWrittenCommit
	}
}

// decodeClusterPayload 解析请求体。
//
// 解析失败是协议层的问题（请求体本身不是合法 JSON），不是业务失败 ——
// 与 handleCreateSession 对畸形登录请求的处理保持一致，返回真实的 400
// 而不是 200 + code，否则网关与前端拦截器都需要先解析响应体才能
// 判断这是不是同一类故障。
func decodeClusterPayload(w http.ResponseWriter, r *http.Request) (clusterPayload, bool) {
	var p clusterPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
		return clusterPayload{}, false
	}
	return p, true
}

// handleListClustersFromRegistry 返回已注册的集群。
func handleListClustersFromRegistry(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := d.Registry.Clusters(r.Context())
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if list == nil {
			list = []registry.Cluster{}
		}
		response.WriteOK(w, list)
	}
}

// handleCreateCluster 注册一个集群。
//
// 带绑定注册时同样先校验一次（spec §3.3「保存绑定时自动触发一次」）：
// 一个只在修改时校验的实现，会让「注册时就填好绑定」这条最常见的路径
// 永远停在 NOT_VERIFIED 上。
func handleCreateCluster(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeClusterPayload(w, r)
		if !ok {
			return
		}
		c := p.toCluster()
		verifyOnSave(r, d, c.Git)
		if err := d.Registry.CreateCluster(r.Context(), actorOf(r), c); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"id": p.ID})
	}
}

// handleUpdateCluster 整体替换一个集群的登记信息。
//
// 语义是替换而非合并：请求体没给的字段会被写成空值，因此它挂在 PUT 上。
// 这不是遗憾的实现细节 —— podCIDR/nodeCIDR 是求值层做网段分类的依据，
// 一个「只发改动字段」的调用把它们清空，不会报错，只会让此后每一次
// 判定都用错的网段回答。要合并语义就得在这里显式实现并测试，在此之前
// 动词必须诚实。
func handleUpdateCluster(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeClusterPayload(w, r)
		if !ok {
			return
		}
		p.ID = chi.URLParam(r, "clusterID")
		c := p.toCluster()
		// 保留库里已有的接入状态：修改网段不该把一个 READY 的集群打回 REGISTERED，
		// 也不该让调用方借修改之机把状态改成任何它想要的值。
		existing, found, err := d.Registry.Cluster(r.Context(), c.ID)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if !found {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		c.State = existing.State
		// 同一条纪律的第二处应用：漂移基准是平台自己的断言，只能从库里
		// 已有的值推导，不能由请求体携带。
		carryDriftBaseline(c.Git, existing.Git)
		verifyOnSave(r, d, c.Git)
		if err := d.Registry.UpdateCluster(r.Context(), actorOf(r), c); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"id": p.ID})
	}
}

// handleDeleteCluster 下线集群。
func handleDeleteCluster(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "clusterID")
		if err := d.Registry.SoftDeleteCluster(r.Context(), actorOf(r), id); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"id": id})
	}
}

// actorOf 从请求的会话中取出操作者身份。
//
// 审计的 actor 必须来自会话而非请求体：任何允许调用方自称身份的
// 审计记录，都无法在事后证明是谁做的。
//
// 这些 handler 全部挂在 RequireSession 之后，取不到会话意味着路由
// 装配错了 —— 返回空串而不是 panic，让审计行留下一个可见的空洞，
// 比让进程崩掉更容易定位。
func actorOf(r *http.Request) registry.Actor {
	sess, ok := SessionFrom(r.Context())
	if !ok {
		return registry.Actor{}
	}
	return registry.Actor{Username: sess.Username}
}

// writeRegistryError 把 registry 层的错误映射为响应。
//
// 输入不合法与目标不存在都是业务失败，用 code + HTTP 200；
// 其余按内部错误处理，真实原因只进日志。判据是「该不该计入服务错误率」。
//
// InvalidError 分支只回传 ie.Detail，绝不回传 err.Error()：Detail 是
// registry 自己写的文案（比如「podCIDR "10.20.0/14" 不是合法网段」），
// 但 err.Error() 可能因为 %v 拼进了 Cause —— 而 Cause 在 ParseImport
// 的 YAML 解析失败路径上就是 sigs.k8s.io/yaml / encoding/json 的原始
// 报错，文本里直接带 Go 结构体类型名（NetworkPolicySpec、LabelSelector
// 之类），回传等于把内部实现细节交给调用方。「这是不是校验错误」与
// 「哪段文字可以回传」由 errors.As 到具体类型来回答，不能靠
// errors.Is(err, registry.ErrInvalid) 这一个谓词兼顾两件事。
//
// default 分支兜住的是数据库、网络这类别人给的错误，只能进日志。
// 「msg 永不回传内部错误细节」约束的是这一支，不是 InvalidError 那一支。
func writeRegistryError(w http.ResponseWriter, r *http.Request, d Deps, err error) {
	var ie *registry.InvalidError
	switch {
	case errors.As(err, &ie):
		// err 本身（含 Cause）进日志，Warn 而非 Error：这是调用方的输入
		// 问题，不是服务故障，但运维事后排查「用户昨天为什么提交失败」
		// 时需要在日志里找到完整原因，不能指望对方还留着截图。
		d.Logger.Warn("registry validation rejected",
			"err", err, "request_id", RequestIDFrom(r.Context()))
		response.WriteInvalid(w, ie.Detail)
	case errors.Is(err, registry.ErrNotFound):
		response.WriteBusiness(w, response.CodeNotFound)
	default:
		d.Logger.Error("registry operation failed",
			"err", err, "request_id", RequestIDFrom(r.Context()))
		response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
	}
}
