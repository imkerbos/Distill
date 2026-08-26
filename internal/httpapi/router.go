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
	// Writeback 提供写回所需的两条持久化路径：记一次计划，与记一次推送并
	// 落 commit。
	//
	// 与 Registry 分开而不是合成一个（同 registry.WritebackStore 的注释）：
	// 写回只需要那两个方法，而 Store 带着集群、账号、设置的全部写方法。
	// 允许为 nil —— 此时写回两个端点都拒绝，因为一次留不下痕迹的写回，
	// 事后没有任何东西能回答"谁把它推了出去"。
	Writeback registry.WritebackStore
	// PolicyWriter 把已确认的写回计划推进策略仓库。
	//
	// 允许为 nil：未配置 secrets 的部署没有写回这回事。nil 表示"没有这条
	// 路径"，**不是**"随便谁都能推"——见 handlePolicyWritebackPush。
	PolicyWriter PolicyWriter
	// AgentSink 落库一次由集群 agent 推上来的采集运行。
	//
	// 允许为 nil：没有部署 agent 的形态下没有推送这回事。nil 表示「本部署
	// 收不下推送」，摄入端点据此答依赖不可用 —— **不是**静默成功，那会让
	// agent 把这一轮当成已经交付，而那批观测就此丢了。
	AgentSink AgentSink
	// AgentDeriver 在一次推送落库之后推导 Pod 身份区间。
	//
	// **没有它，推送式接入的集群是不可用的**：pod_identity_interval 会是空的，
	// 而六个读方法每一个都从那张表出发 —— 页面上显示「这个集群还没有可用的
	// 采集数据」，而资产其实已经在库里了。
	//
	// 拉取式接入下这件事由采集器自己做（它有库）；推送式下 agent 读不到整张
	// 区间表，只能发生在这一侧（design doc 2026-08-18）。
	//
	// 允许为 nil：与 AgentSink 一样，没有部署 agent 的形态下没有推送这回事。
	AgentDeriver AgentDeriver
	// AgentFlowSink 落库一次由集群 agent 推上来的流量摄入。
	//
	// 与 AgentSink 分开而不是共用一个：资产与流量是两条独立的链路，各自
	// 会失败、各自重试。一个只推资产的部署形态是真实存在的（还没接上流量
	// 来源的集群），而共用一个字段会让它必须提供一个假的摄入端。
	//
	// 允许为 nil：nil 表示「本部署收不下流量」，摄入端点据此答依赖不可用 ——
	// **不是**静默成功，那会让 agent 把这一轮当成已经交付。
	AgentFlowSink AgentFlowSink
	// Fleet 现读全 fleet 的网段登记，供摄入路径判定地址归属。
	//
	// 与 AgentSink 一起为 nil 或一起非 nil：判不出归属的落库比不落库更糟，
	// 那批 Pod 会以「归属未知」的形态进库，而症状要到求值阶段才冒出来。
	Fleet FleetSource
	// Collection 读最近一次资产采集运行的摘要。
	//
	// 允许为 nil：采集是一个独立进程，未部署它的形态下没有采集这回事。
	// nil 表示"本部署没有采集读取端"，**不是**"这个集群没有采集记录"——
	// 见 handleCollection。
	Collection CollectionReader
	// FlowIngest 读最近一次流量摄入的摘要。
	//
	// 与 Collection 分开：资产采集与流量摄入是两条独立链路，各自会失败、
	// 各自重试，而一个部署可以只有其中一条。允许为 nil —— nil 表示"本部署
	// 没有流量读取端"，**不是**"这个集群没有摄入记录"。
	FlowIngest FlowIngestReader
	// Reconciliations 读一致率的历史走向（design doc 2026-08-25 §9 第 3 条）。
	//
	// 与 Reader.Reconciliation 分开：那个算的是**一个窗口**的对账，现算现出；
	// 这个读的是历次对账落下来的记录，回答"在变好还是变坏"。一次 97% 无所谓，
	// 从 100% 掉到 97% 才是信号，而后者只能从历史里看出来。
	//
	// 允许为 nil：nil 表示"本部署不记录对账历史"，**不是**"这个集群没有历史"。
	Reconciliations ReconciliationTrendReader
	// GitVerifier 对 Git 绑定做只读校验。
	//
	// 允许为 nil：未配置 secrets 的部署（比如 demo）不做校验，结论一律是
	// NOT_VERIFIED。nil 表示"没有校验这回事"，**不是**"校验都通过"——
	// 见 verifyBinding。
	GitVerifier GitVerifier
	// 这里曾经有一个 DefaultWindow store.TimeWindow：装配时算一次、六条路径
	// 共用的常量时间窗。它取的是合成数据集自己的范围（2026-07-31 上的约 36
	// 秒），于是接线之后每个 COLLECTED 集群的每一屏都被钉在去年那几十秒上，
	// 而前端从不发 from/to。删掉它是本轮的核心改动，不是清理
	// （design doc 2026-08-18 §3.1）——默认窗口现在由 Reader 按集群现答
	// （store.Reader.DefaultWindow），与六个读方法走同一个按来源的分派。
	//
	// **不要把它加回来**，包括加成一个「最近 N 小时」的配置项：那样的取值
	// 要有人填对才安全，而填错了没有任何症状 —— 填长了扫过量数据，填短了
	// 漏掉真实流量并把它读成「这条规则没有流量、可以收紧」。「我们实际能
	// 回答的是哪一段时间」是库里现成的事实，不是一个待填的参数。
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
	// 请求体上限**按子树装**，不装在根部：agent 子树的报文与人的子树差三个
	// 数量级（一次流量摄入 1–2 MB，一次登录几百字节），装在根部只能取小的
	// 那个，而那会让摄入过不去。见 MaxAgentRequestBytes 的说明。
	//
	// 仍然是子树级中间件、不是每个 Decode 调用点各一份 —— 那条理由没变：
	// 漏掉的那一个就是会被挑中的那一个。新增一条**子树**时要记得声明，
	// 而 TestEveryBodyTakingSubtreeIsAccountedFor 守着这件事。

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusNotFound, response.CodeNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusMethodNotAllowed, response.CodeInvalidParam)
	})

	// 每条受保护路由都在注册的同一行里声明所需权限；没有声明的路由会被
	// 拒绝，而不是放行（见 authorizer）。角色由校验器在每次请求上现读。
	az := newAuthorizer(apiPrefix, d.Verifier, d.Logger)

	r.Route(apiPrefix, func(api chi.Router) {
		// 登录是唯一无需会话的端点，也因此是唯一挂了限流的那个。
		// 它也是唯一不经过授权层的端点：还没有会话，也就还没有角色。
		api.With(loginLimiter.Middleware, LimitRequestBody(MaxRequestBytes)).
			Post("/sessions", handleCreateSession(d))

		// agent 子树与人的子树**并列，不嵌套**（design doc 2026-08-18 §3.3）。
		//
		// 这里没有 RequireSession，也没有 az.enforce：agent 没有 Cookie、
		// 没有角色，它能做什么完全由 token 绑定的那个集群决定。两条链互不
		// 相通 —— 人的会话不得成为一次摄入的身份，一把泄漏的 agent token
		// 也不得成为一把能读全平台的钥匙。
		//
		// 默认拒绝同样成立，只是形状不同：这条链上没有有效 token 一律 401。
		api.Route("/agent", func(agent chi.Router) {
			agent.Use(LimitRequestBody(MaxAgentRequestBytes))
			agent.Use(RequireAgent(d))
			agent.Get("/config", handleAgentConfig())
			agent.Post("/collection-runs", handleAgentCollectionRun(d))
			// 流量与资产是两条独立的链路：一个集群可以只推资产（还没接上
			// 流量来源），也可以两条都推。因此是两条路由、两个 sink。
			agent.Post("/flow-ingests", handleAgentFlowIngest(d))
		})

		api.Group(func(protected chi.Router) {
			protected.Use(LimitRequestBody(MaxRequestBytes))
			protected.Use(RequireSession(d.Sessions))
			// 授权紧跟在认证之后：先知道是谁，才谈得上他能做什么。
			protected.Use(az.enforce)

			// 会话自身：读自己的会话、结束自己的会话。对任何已登录的人
			// 都成立，与角色无关。
			az.route(protected, http.MethodDelete, "/sessions/current", accessSession, handleDeleteSession(d))
			az.route(protected, http.MethodGet, "/sessions/current", accessSession, handleCurrentSession())

			// 改自己的密码对任何角色都成立，与账号管理无关，因此挂在
			// /me 下而不是 /accounts 下：目标取自会话，路径里没有用户名
			// 可写，也就没有"改别人的密码"这条形状（design doc §6）。
			az.route(protected, http.MethodPost, "/me/password", accessSession, handleChangeOwnPassword(d))

			// 账号管理整组按管理员算：建号、改角色、停用、删除都是权限
			// 变更（规范 §7、§28）。列表也是管理员 —— 它回传的是平台上
			// 都有谁、谁是管理员，那是一份攻击者最想先拿到的名单，而只读
			// 账号的用途是看拓扑与流量。
			az.route(protected, http.MethodGet, "/accounts", accessAdmin, handleListAccounts(d))
			az.route(protected, http.MethodPost, "/accounts", accessAdmin, handleCreateAccount(d))
			// PUT 而非 PATCH：这里写的是角色这一个字段的全部内容。
			az.route(protected, http.MethodPut, "/accounts/{username}/role",
				accessAdmin, handleUpdateAccountRole(d))
			// 停用与启用是两个动作，不是同一个端点的两个取值：一个约定
			// 俗成的布尔会让任何一次误发的请求体变成一次无声的启用，而
			// 启用一个被停掉的管理员是把权限还回去。
			az.route(protected, http.MethodPost, "/accounts/{username}/disable",
				accessAdmin, handleDisableAccount(d))
			az.route(protected, http.MethodPost, "/accounts/{username}/enable",
				accessAdmin, handleEnableAccount(d))
			az.route(protected, http.MethodDelete, "/accounts/{username}", accessAdmin, handleDeleteAccount(d))
			// POST 而非 PUT：重置密码不是把某个资源写成给定的样子，它是
			// 一次动作，且不该被浏览器或前端的重试逻辑当成可以随手重放的
			// 东西 —— 与两个 verify 端点同一处置。
			az.route(protected, http.MethodPost, "/accounts/{username}/password",
				accessAdmin, handleResetAccountPassword(d))

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

			// 集群 agent 的签发、台账与吊销（design doc 2026-08-18 §3）。
			//
			// 三条都要管理员，列表也不例外：它回答的是「这个集群有几把能往
			// 平台写数据的钥匙、上次什么时候用的」，那是凭据台账，不是只读
			// 业务数据（规范 §28）。
			//
			// 这三条是**人**用的，挂在会话子树里。agent 自己用的那几条走
			// 独立子树、独立认证，两者互不相通（design doc §3.3）。
			az.route(protected, http.MethodPost, "/clusters/{clusterID}/agents",
				accessAdmin, handleIssueClusterAgent(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/agents",
				accessAdmin, handleListClusterAgents(d))
			az.route(protected, http.MethodDelete, "/clusters/{clusterID}/agents/{agentID}",
				accessAdmin, handleRevokeClusterAgent(d))
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
			// 漂移检测是 GET：它只读 —— 不写仓库、不改绑定、不动锚点，
			// 也不落结论（design doc 2026-08-18-drift-detection §4）。
			//
			// 仍然要管理员：它会真的发起一次出站克隆，且回传的是策略下发
			// 链路的内部状态，与 verify 同一档。
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/git-binding/drift",
				accessAdmin, handleGitBindingDrift(d))
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
			// 采集摘要按管理员算，不是按只读算。它回传的是一个**真实**集群
			// 的资产盘点（各类资源各有多少），以及平台在这个集群上哪几类
			// 资源没被授权读 —— 后者是一份「平台在哪里是瞎的」的清单，属于
			// 平台自身的权限态势，与设置页回传凭据后端同类（规范 §19：
			// 内部网络信息、安全策略）。
			//
			// 存疑的方向记在这里：/quality 与 /security 是形状相同的完整度
			// 报告，而它们是只读级。区别在于那两条描述的是合成数据集上的
			// 判定能力，这一条描述的是真集群的资产与平台自己的 RBAC 缺口。
			// 分类存疑时取更严的那一侧，与 policy-imports、settings 同一处置。
			// 只读账号的用途是看拓扑与流量，而这份数据不参与任何结论
			// （spec §5.2）—— 没有哪条只读判定依赖它。
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/collection",
				accessAdmin, handleCollection(d))
			// 流量摄入与资产采集并列：两者答的是同一个问题的两半 ——
			// 平台看见了这个集群的什么。
			// 与 /collection 同为 accessAdmin：摘要里带着来源类型（装了
			// Hubble 还是 conntrack agent）与失败原因（UNAUTHORIZED 说明
			// 凭据出了问题），那是平台自身的部署形态。分类存疑时取更严的
			// 那一侧，与本文件其余几处同一处置。
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/flow-ingest",
				accessAdmin, handleFlowIngest(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/topology",
				accessViewer, handleTopology(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/quality",
				accessViewer, handleQuality(d))
			// 一致率与判定同屏可达：它回答的是"这些判定有多可信"，
			// 而一个要另外找地方看的可信度指标等于没有。
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/reconciliation",
				accessViewer, handleReconciliation(d))
			// 趋势与单窗口对账并列：一次 97% 无所谓，从 100% 掉到 97% 才是
			// 信号，而后者只能从历史里看出来（design doc §9 第 3 条：
			// "一致率每天可查"）。
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/reconciliation/trend",
				accessViewer, handleReconciliationTrend(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/security",
				accessViewer, handleSecurity(d))
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/policy-preview",
				accessViewer, handlePolicyPreview(d))
			// 导出按管理员算，比它渲染的那份预览更严：预览是看，导出交出去的
			// 是一份完整的网络策略文档，等同于这个集群网络结构的说明书
			// （design doc 2026-08-14 §5）。它也是本组里唯一写审计的读端点。
			az.route(protected, http.MethodGet, "/clusters/{clusterID}/policy-export",
				accessAdmin, handlePolicyExport(d))
			// 写回两个端点都按管理员算（design doc 2026-08-14 §9）。出计划
			// 也是管理员：它回传的是将要写进策略仓库的完整文件内容与目标
			// 分支，比导出多带一份"平台打算往哪里写"，而只读账号的用途是
			// 看拓扑与流量。
			//
			// 两条地址而不是一个端点带 dryRun 参数：一个约定俗成的布尔会让
			// 任何一次误发的请求体变成一次真的推送，而推送是往生产下发链路
			// 上写。分成两条之后，"出计划"与"真的推"在路由表、审计动作与
			// 权限声明三处都是两件事（§5、§9）。
			az.route(protected, http.MethodPost, "/clusters/{clusterID}/policy-writeback/plan",
				accessAdmin, handlePolicyWritebackPlan(d))
			az.route(protected, http.MethodPost, "/clusters/{clusterID}/policy-writeback/push",
				accessAdmin, handlePolicyWritebackPush(d))
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
