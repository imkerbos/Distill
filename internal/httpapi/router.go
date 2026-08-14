package httpapi

import (
	"log/slog"
	"net/http"
	"time"

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

// 登录限流的参数。
//
// 只有登录挂限流：它是唯一无需会话的端点，而且每次调用要算一次 bcrypt ——
// 既是撞库的目标，也是「一次请求换几十毫秒 CPU」的放大器（规范 §23、§24）。
// 其余端点都在 RequireSession 之后，先要有一个有效会话才够得着。
//
// 每分钟 10 次：一个记错密码的人重试三五次不会被挡，而在这个速率下撞库
// 没有意义。
//
// 4096 个键封住内存：单条计数是常数大小，键数封顶，整张表的占用就封顶。
// 表满且回收不出空位时新键一律被拒（见 RateLimiter.Allow）—— 失效的方向
// 必须是关闭，不是打开。
const (
	loginRateLimit   = 10
	loginRateWindow  = time.Minute
	loginRateMaxKeys = 4096
)

// apiPrefix 是全部 API 路由的挂载前缀。
//
// 常量而非两处各写一遍的字面量：授权表的键是 chi 匹配出的**完整**模式，
// 前缀与挂载点错开一个字符，声明就一条都对不上，而症状是所有端点 403。
const apiPrefix = "/api/v1"

// NewRouter 装配 HTTP 路由。
func NewRouter(d Deps) http.Handler {
	// 限流器随路由一起构造，生命周期与它相同：计数是本进程内的，
	// 不跨实例，见 RateLimiter 的文档注释。
	loginLimiter := NewRateLimiter(loginRateLimit, loginRateWindow, loginRateMaxKeys, nil)

	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(RequestLogger(d.Logger))
	// Recoverer 在日志之后：panic 的请求同样要留下一条完成日志，
	// 且日志里要有可供用户报障的 request_id。
	r.Use(Recoverer(d.Logger))
	// 安全头在 Recoverer 之内：头在进入 handler 前就写进 w，因此
	// 500、404、405 这些不经过 handler 的响应同样带着它们。
	r.Use(SecurityHeaders)
	// 请求体上限装在路由根部，而不是五个 Decode 调用点各一份 ——
	// 漏掉的那一个就是会被挑中的那一个。
	r.Use(LimitRequestBody(MaxRequestBytes))

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusNotFound, response.CodeNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusMethodNotAllowed, response.CodeInvalidParam)
	})

	// 每条受保护路由都在注册的同一行里声明所需权限；没有声明的路由会被
	// 拒绝，而不是放行（见 authorizer）。
	az := newAuthorizer(apiPrefix)

	r.Route(apiPrefix, func(api chi.Router) {
		// 登录是唯一无需会话的端点，也因此是唯一挂了限流的那个。
		// 它也是唯一不经过授权层的端点：还没有会话，也就还没有角色。
		api.With(loginLimiter.Middleware).Post("/sessions", handleCreateSession(d))

		api.Group(func(protected chi.Router) {
			protected.Use(RequireSession(d.Sessions))
			// 授权紧跟在认证之后：先知道是谁，才谈得上他能做什么。
			protected.Use(az.enforce)

			// 会话自身：读自己的会话、结束自己的会话。对任何已登录的人
			// 都成立，与角色无关。
			az.route(protected, http.MethodDelete, "/sessions/current", accessSession, handleDeleteSession(d))
			az.route(protected, http.MethodGet, "/sessions/current", accessSession, handleCurrentSession())

			// 平台设置：读与写都按管理员算。写是显然的（改 host key 就是
			// 改平台连接策略仓库时的信任锚，design doc §1.3）；读之所以也是
			// 管理员，是因为它回传的是凭据后端、Secret Manager 项目与前缀 ——
			// 平台自己的部署形态，而只读账号的用途是看拓扑与流量。分类存疑时
			// 取更严的那一侧，与 policy-imports 那条同一处置（见本次报告）。
			az.route(protected, http.MethodGet, "/settings", accessAdmin, handleGetSetting(d))
			az.route(protected, http.MethodPut, "/settings", accessAdmin, handleUpdateSetting(d))

			// 策略仓库是独立实体（design doc §3），因此有自己的一组地址。
			// 列表同样按管理员算：它回传仓库地址、分支与凭据引用，属于策略
			// 下发链路的内部状态（同上，见本次报告）。
			az.route(protected, http.MethodGet, "/git-repos", accessAdmin, handleListGitRepos(d))
			az.route(protected, http.MethodPost, "/git-repos", accessAdmin, handleCreateGitRepo(d))
			// PUT 而非 PATCH，理由与集群那条一样：这里写整行。仓库 ID 不在
			// 可改之列 —— 它取自路径，请求体里的 repoId 不参与。
			az.route(protected, http.MethodPut, "/git-repos/{repoID}", accessAdmin, handleUpdateGitRepo(d))
			az.route(protected, http.MethodDelete, "/git-repos/{repoID}", accessAdmin, handleDeleteGitRepo(d))
			// POST 而非 GET：与绑定那条同理，这次调用会真的发起一次出站
			// 认证连接，并写下新的结论与一条审计行。
			az.route(protected, http.MethodPost, "/git-repos/{repoID}/verify",
				accessAdmin, handleVerifyGitRepo(d))

			az.route(protected, http.MethodGet, "/clusters", accessViewer, handleListClustersFromRegistry(d))
			az.route(protected, http.MethodPost, "/clusters", accessAdmin, handleCreateCluster(d))
			// PUT 而非 PATCH：handleUpdateCluster 写整行，请求体没给的字段
			// 会被写成空值。挂成 PATCH 是在邀请调用方只发一个字段，然后
			// 把 podCIDR 清空 —— 那不是一次失败的请求，而是此后每一次
			// 判定都用错了网段分类，且没有任何报错。
			az.route(protected, http.MethodPut, "/clusters/{clusterID}", accessAdmin, handleUpdateCluster(d))
			az.route(protected, http.MethodDelete, "/clusters/{clusterID}", accessAdmin, handleDeleteCluster(d))
			// 绑定是有自己生命周期的资源，因此有自己的地址与动词
			// （design doc 2026-08-13 §5）。PUT 而非 PATCH，理由与集群
			// 那条一样：repoId 与 policyPath 是绑定的全部可写内容，
			// 这里写整行。
			az.route(protected, http.MethodPut, "/clusters/{clusterID}/git-binding",
				accessAdmin, handleBindGitRepo(d))
			// DELETE 而非「两个字段全空的一次 PUT」：解绑是一个明确的动作，
			// 靠一个约定俗成的空值形状表达它，任何一次误发的空请求体都会
			// 变成一次无声的解绑。
			az.route(protected, http.MethodDelete, "/clusters/{clusterID}/git-binding",
				accessAdmin, handleUnbindGitRepo(d))
			// POST 而非 GET：这次调用会真的发起一次出站认证连接，并写下
			// 新的结论与一条审计行 —— 不是一次读取，不该被浏览器、代理
			// 或前端的重试逻辑当成可以随手重放的东西。
			az.route(protected, http.MethodPost, "/clusters/{clusterID}/git-binding/verify",
				accessAdmin, handleVerifyGitBinding(d))
			// 导入清单的读取按管理员算，不是按只读算：它回传的是导入的
			// NetworkPolicy 原文与校验状态，属于策略下发链路的内部状态，
			// 而只读账号的用途是看拓扑与流量。分类存疑时取更严的那一侧
			// （见本次报告）。
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/policy-imports",
				accessAdmin, handleListImports(d))
			az.route(protected, http.MethodPost, "/clusters/{clusterID}/policy-imports",
				accessAdmin, handleCreateImport(d))
			az.route(protected, http.MethodDelete, "/clusters/{clusterID}/policy-imports/{importID}",
				accessAdmin, handleDeleteImport(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/topology",
				accessViewer, handleTopology(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/quality",
				accessViewer, handleQuality(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/security",
				accessViewer, handleSecurity(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/policy-preview",
				accessViewer, handlePolicyPreview(d))
			az.route(protected, http.MethodPost, "/clusters/{clusterID}/rule-overrides",
				accessAdmin, handleCreateOverride(d))
			az.route(protected, http.MethodDelete, "/clusters/{clusterID}/rule-overrides",
				accessAdmin, handleDeleteOverride(d))
			az.route(protected, http.MethodGet, "/flows", accessViewer, handleListFlows(d))
			az.route(protected, http.MethodGet, "/flows/{flowID}/decision",
				accessViewer, handleFlowDecision(d))
		})
	})

	return r
}
