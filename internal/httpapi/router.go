package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// Deps 是路由所需的全部依赖。
//
// 显式结构体而非全局变量：测试可以只装配自己需要的部分，
// 而不必让整个进程处于某种状态。
type Deps struct {
	// Sessions 是会话存储。
	Sessions *auth.SessionStore
	// Verifier 校验登录凭证。
	Verifier *auth.Verifier
	// Logger 是结构化日志器。
	Logger *slog.Logger
	// Reader 提供 Fleet 数据查询。
	Reader store.Reader
	// Registry 提供集群注册与策略导入的持久化。
	Registry registry.Store
	// GitVerifier 对 Git 绑定做只读校验。
	//
	// 允许为 nil：未配置 secrets 的部署（比如 demo）不做校验，结论一律是
	// NOT_VERIFIED。nil 表示"没有校验这回事"，**不是**"校验都通过"——
	// 见 verifyBinding。
	GitVerifier GitVerifier
	// DefaultWindow 是流量查询未指定 from/to 时使用的时间窗。
	//
	// 由装配方注入而非在此取默认值：合适的默认窗口取决于部署形态。
	// demo 用覆盖 fixture 全量的窗口（数据固定在过去某一天，任何
	// "最近 N 天"都会在某天悄悄返回 0 条）；真实部署应注入一个有界
	// 窗口，与事实层的 require_partition_filter 相称。
	DefaultWindow store.TimeWindow
}

// NewRouter 装配 HTTP 路由。
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(RequestLogger(d.Logger))
	// Recoverer 在日志之后：panic 的请求同样要留下一条完成日志，
	// 且日志里要有可供用户报障的 request_id。
	r.Use(Recoverer(d.Logger))

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusNotFound, response.CodeNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusMethodNotAllowed, response.CodeInvalidParam)
	})

	r.Route("/api/v1", func(api chi.Router) {
		// 登录是唯一无需会话的端点。
		api.Post("/sessions", handleCreateSession(d))

		api.Group(func(protected chi.Router) {
			protected.Use(RequireSession(d.Sessions))
			protected.Delete("/sessions/current", handleDeleteSession(d))
			protected.Get("/sessions/current", handleCurrentSession())
			protected.Get("/clusters", handleListClustersFromRegistry(d))
			protected.Post("/clusters", handleCreateCluster(d))
			// PUT 而非 PATCH：handleUpdateCluster 写整行，请求体没给的字段
			// 会被写成空值。挂成 PATCH 是在邀请调用方只发一个字段，然后
			// 把 podCIDR 清空 —— 那不是一次失败的请求，而是此后每一次
			// 判定都用错了网段分类，且没有任何报错。
			protected.Put("/clusters/{clusterID}", handleUpdateCluster(d))
			protected.Delete("/clusters/{clusterID}", handleDeleteCluster(d))
			// POST 而非 GET：这次调用会真的发起一次出站认证连接，并写下
			// 新的结论与一条审计行 —— 不是一次读取，不该被浏览器、代理
			// 或前端的重试逻辑当成可以随手重放的东西。
			protected.Post("/clusters/{clusterID}/git-binding/verify", handleVerifyGitBinding(d))
			protected.Get("/clusters/{clusterID}/policy-imports", handleListImports(d))
			protected.Post("/clusters/{clusterID}/policy-imports", handleCreateImport(d))
			protected.Delete("/clusters/{clusterID}/policy-imports/{importID}", handleDeleteImport(d))
			protected.Get("/clusters/{clusterID}/topology", handleTopology(d))
			protected.Get("/clusters/{clusterID}/quality", handleQuality(d))
			protected.Get("/clusters/{clusterID}/security", handleSecurity(d))
			protected.Get("/clusters/{clusterID}/policy-preview", handlePolicyPreview(d))
			protected.Post("/clusters/{clusterID}/rule-overrides", handleCreateOverride(d))
			protected.Delete("/clusters/{clusterID}/rule-overrides", handleDeleteOverride(d))
			protected.Get("/flows", handleListFlows(d))
			protected.Get("/flows/{flowID}/decision", handleFlowDecision(d))
		})
	})

	return r
}
