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
//
// **不含 git**：绑定是有自己生命周期的资源，走自己的两条路由与自己的
// 审计动作（design doc 2026-08-13 §5，见 gitbinding_handler.go）。留一个
// 收下再忽略的 git 字段在这里，后果不是多一个没用的字段 —— 而是调用方
// 填了仓库地址、请求返回成功、绑定却从未写下，直到某天有人发现这个集群
// 的策略一直没有下发。
type clusterPayload struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	PodCIDR     string `json:"podCidr"`
	NodeCIDR    string `json:"nodeCidr"`
	CCNPPresent bool   `json:"ccnpPresent"`
	// KubeconfigRef 是这个集群的 kubeconfig 在凭据后端里的短名。
	//
	// 采集器是唯一的消费方，也是唯一解析它的地方（cmd/distill-collector）。
	// 这里只收下一个名字：主服务永不解析它，更不持有它指向的凭据。
	//
	// 与 git 那条相反，这个字段**必须**在这里：集群没有别的路由能说出
	// 自己的凭据引用，漏掉它的后果是采集器永远拿不到凭据，
	// 而这件事要到第一次真采集才暴露。
	KubeconfigRef      string               `json:"kubeconfigRef"`
	State              string               `json:"state"`
	APIServers         []registry.APIServer `json:"apiServers"`
	HealthCheckSources []string             `json:"healthCheckSources"`
	// MetricsScrapers 是 metrics 抓取端登记。
	//
	// 与 healthCheckSources 同类：它是 METRICS_SCRAPE Baseline 依据的一半，
	// 而这一半观测不出来（design doc 2026-08-18-metrics-scrape-evidence §3.2）。
	// 漏掉它的后果与漏掉 kubeconfigRef 同形 —— 请求成功、登记没落下，
	// 而那一类 Baseline 继续报缺失，没有任何东西指向登记。
	MetricsScrapers []registry.MetricsScraper `json:"metricsScrapers"`
}

// toCluster 把请求体转成领域对象。
//
// state 参数**完全忽略**：接入状态反映平台实际收到了什么，不是调用方的意愿。
// 只做「为空时兜底」是不够的 —— 那样一次显式的 {"state":"READY"} 仍会被接受，
// 等于允许把「还没有数据」标成「可以出推荐了」。创建一律从 REGISTERED 起步，
// 修改时保留库里已有的状态（见 handleUpdateCluster）。
//
// Git 一律留空：集群写路径不碰绑定，registry.Store 的两个集群写方法也不会
// 落它。这条纪律在两侧各有一道，缺一侧都会让绑定多出一条绕开
// BIND_GIT_REPO 审计的写路径。
func (p clusterPayload) toCluster() registry.Cluster {
	return registry.Cluster{
		State:              registry.StateRegistered,
		ID:                 p.ID,
		DisplayName:        p.DisplayName,
		PodCIDR:            p.PodCIDR,
		NodeCIDR:           p.NodeCIDR,
		CCNPPresent:        p.CCNPPresent,
		KubeconfigRef:      p.KubeconfigRef,
		APIServers:         p.APIServers,
		HealthCheckSources: p.HealthCheckSources,
		MetricsScrapers:    p.MetricsScrapers,
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
// 不发任何出站请求：绑定不在这条路径上，注册完成后由
// PUT /clusters/{id}/git-binding 单独绑定并校验。
func handleCreateCluster(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeClusterPayload(w, r)
		if !ok {
			return
		}
		c := p.toCluster()
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
	case errors.Is(err, registry.ErrRepoInUse):
		// 仍被绑定的仓库不能删，这是一个调用方能据以行动的业务失败，
		// 不是服务故障：走 500 会让它计入服务错误率，而界面上只剩一句
		// 「服务内部错误」，操作者不会知道该先去解除那个绑定。
		//
		// 文案是本平台自己写的固定一句，不回传 err.Error()：那串文本由
		// mysqlregistry 拼出，里面带着仓库 ID 与**集群 ID**，而回传通道
		// 只读我们自己写的文字（规范 §19、§22，同 InvalidError 那一支的
		// 处置）。少掉的那一半信息（是哪个集群绑着）在集群页上看得到。
		response.WriteInvalid(w, "该仓库仍被某个集群绑定，请先解除绑定再删除")
	case errors.Is(err, registry.ErrLastAdmin):
		// 与 ErrRepoInUse 同一处置：这是一个调用方能据以行动的业务失败
		// （先提一个管理员上来，再动这一个），不是服务故障。走 500 会让
		// 「不得把自己锁在门外」这条保护在界面上显示成一句"服务内部错误"，
		// 操作者只会以为平台坏了，然后重试 —— 而重试永远得到同一个结果
		// （design doc 2026-08-14 §5）。
		//
		// 文案固定，不回传 err.Error()：那串文本由 mysqlregistry 拼出，
		// 带着用户名。
		response.WriteInvalid(w, "这是最后一个启用中的管理员，停用、删除或降级它之后就没有人能管理平台了")
	default:
		d.Logger.Error("registry operation failed",
			"err", err, "request_id", RequestIDFrom(r.Context()))
		response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
	}
}
