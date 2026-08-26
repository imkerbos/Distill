package registry

import (
	"context"
	"errors"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
)

// ErrNotFound 表示目标不存在。
//
// 定义在本包而非各实现里：handler 需要把它映射成业务错误码，
// 而边界层依赖某个具体存储实现的哨兵，会让将来换存储变成跨层改动。
var ErrNotFound = errors.New("registry target not found")

// ErrRepoInUse 表示仓库仍被某个集群绑定，因而不能删除。
//
// 删除仍被绑定的仓库直接拒绝，不做级联（design doc 2026-08-13 §4）：
// 级联会让一次仓库清理静默解除某个集群的策略下发路径 —— 没有报错、
// 没有人做过「这个集群不再下发策略」的决定，下一次推荐照常产出，
// 只是再也没有地方接收它。
//
// 与 ErrNotFound 一样定义在本包：边界层要把它映射成业务错误码，
// 而依赖某个具体存储实现的哨兵会让将来换存储变成跨层改动。
var ErrRepoInUse = errors.New("git repo is still bound to a cluster")

// Actor 是操作者身份，写入审计。
type Actor struct {
	// Username 是操作者登录名。
	Username string
}

// PolicyExport 描述一次已确认策略的导出，写入审计。
//
// 时间窗拆成两个 time.Time 而不是复用 store.TimeWindow：registry 被 store
// 依赖，反向 import 会成环。
type PolicyExport struct {
	// ClusterID 是被导出的集群。
	ClusterID string
	// Namespace 是导出时生效的命名空间筛选；空表示全集群。
	Namespace string
	// From、To 是导出所依据的时间窗。
	//
	// 必须记下来：同一个集群换一个窗口导出的是另一套规则，少了它，
	// 事后无法判断某份流出去的文件对应的是哪一次判定。
	From time.Time
	To   time.Time
	// Documents 是文件里 NetworkPolicy 文档的份数。
	Documents int
	// Rules 是文件里启用规则的条数（design doc 2026-08-14 §5）。
	Rules int
}

// Store 是集群注册与策略导入的持久化接口。
//
// 只暴露完整的业务操作，不暴露事务句柄：任何能绕开审计的写路径，
// 都会在事故复盘时变成一段无法解释的历史 —— 而那正是最需要解释的时候。
type Store interface {
	// Clusters 返回全部未删除的集群。
	Clusters(ctx context.Context) ([]Cluster, error)
	// Cluster 按 ID 查一个未删除的集群。不存在时第二个返回值为 false。
	Cluster(ctx context.Context, id string) (Cluster, bool, error)
	// CreateCluster 注册一个集群，同事务写审计。
	CreateCluster(ctx context.Context, actor Actor, c Cluster) error
	// UpdateCluster 修改集群，同事务写审计。集群不存在时返回 ErrNotFound。
	UpdateCluster(ctx context.Context, actor Actor, c Cluster) error
	// SoftDeleteCluster 下线集群，同事务写审计。
	//
	// 软删除而非物理删除：级联删除会连带删掉覆盖决定，
	// 而那些行记录着谁在什么时候以什么理由启用了一条风险规则。
	SoftDeleteCluster(ctx context.Context, actor Actor, id string) error

	// IssueClusterAgent 登记一把新签发的 agent token，同事务写审计。
	//
	// 明文 token 不经过这个接口：签发方（internal/agentauth）把它交给
	// 操作者，落库的只有摘要（design doc 2026-08-18 §3.2）。
	IssueClusterAgent(ctx context.Context, actor Actor, a ClusterAgent) error
	// RevokeClusterAgent 吊销一把 token，同事务写审计。
	//
	// 按 (clusterID, agentID) 定位而不是只按 agentID：少了前者，一个
	// 管理员就能吊销别的集群的 agent，而界面上看起来他只在操作自己那个。
	// 没有可吊销的行时返回 ErrNotFound —— 静默成功会让一次吊销错对象的
	// 操作看起来生效了。
	RevokeClusterAgent(ctx context.Context, actor Actor, clusterID, agentID string) error
	// ClusterAgents 返回一个集群下的全部 agent，含已吊销的。
	ClusterAgents(ctx context.Context, clusterID string) ([]ClusterAgent, error)
	// ClusterAgentByID 按公开段查一条记录，**不按状态过滤**。
	//
	// 认证层要能分辨「被吊销」与「从来不存在」：前者意味着有人正在用一把
	// 该扔的凭据，那是要被看见的信号。在这里过滤掉就把两者合并了。
	ClusterAgentByID(ctx context.Context, agentID string) (ClusterAgent, bool, error)
	// TouchClusterAgent 记下一次成功认证的时刻。
	//
	// 不写审计：每一次成功的摄入都会调它一次。它是可观测性，不是安全判定
	// —— 写失败不该拒掉一次合法推送。
	TouchClusterAgent(ctx context.Context, agentID string, at time.Time) error

	// SetOtherPlanes 记下一次策略平面探测的结论（design doc 2026-08-25 §2.3）。
	//
	// 由采集层调用，**不在集群 CRUD 的写路径里**：它是一个关于集群的事实，
	// 不是一项人填的配置。人能拨的那一份是 CCNPPresent，且只能往降级方向拨。
	SetOtherPlanes(ctx context.Context, clusterID string, planes PolicyPlanes) error
	// SetCNI 记下采集认出来的网络插件。
	//
	// **认不出时写 UNKNOWN，不是不写** —— 与 SetOtherPlanes 同一条理由：
	// 不写会让上一次的结论留在那里，而集群的 CNI 是会换的（迁移）。
	SetCNI(ctx context.Context, clusterID string, cni cluster.CNI) error
	// PolicyImports 返回一个集群下未删除的导入策略。
	PolicyImports(ctx context.Context, clusterID string) ([]PolicyImport, error)
	// CreatePolicyImport 记录一条导入，同事务写审计。
	CreatePolicyImport(ctx context.Context, actor Actor, p PolicyImport) error
	// SoftDeletePolicyImport 删除一条导入，同事务写审计。
	SoftDeletePolicyImport(ctx context.Context, actor Actor, clusterID, importID string) error

	// RecordPolicyExport 为一次策略导出写一条审计行，动作 EXPORT_POLICY
	// （design doc 2026-08-14 §5，规范 §43「导出数据」）。
	//
	// 与 RecordBootstrapLogin 一样没有业务写入 —— 导出不改变平台里的任何
	// 状态，但导出的是这个集群完整的网络策略，等同于集群网络结构的说明书。
	// "谁在什么时候把它拿走了、拿的是哪个窗口下的哪一份"必须答得出来。
	//
	// 审计行本身不含策略内容：那是一份可能很大的文档，而审计要回答的是
	// 「谁拿走了什么范围」，不是把被拿走的东西再存一份。
	RecordPolicyExport(ctx context.Context, actor Actor, e PolicyExport) error

	// RuleOverrides 返回一个集群下未删除的人工决定。
	RuleOverrides(ctx context.Context, clusterID string) ([]RuleOverride, error)
	// CreateRuleOverride 记录一条人工决定，同事务写审计。
	//
	// 同一条规则重复决定时覆盖旧值：人改主意是正常的，而两条互相矛盾
	// 的决定并存会让「这条规则到底开不开」没有答案。旧值进审计。
	CreateRuleOverride(ctx context.Context, actor Actor, o RuleOverride) error
	// SoftDeleteRuleOverride 撤销一条人工决定，同事务写审计。
	SoftDeleteRuleOverride(ctx context.Context, actor Actor, clusterID, namespace, workload, fingerprint string) error

	// GitRepos 返回全部未删除的策略仓库。
	GitRepos(ctx context.Context) ([]GitRepo, error)
	// GitRepo 按 ID 查一个未删除的仓库。不存在时第二个返回值为 false。
	GitRepo(ctx context.Context, repoID string) (GitRepo, bool, error)
	// CreateGitRepo 登记一个仓库，同事务写审计。
	CreateGitRepo(ctx context.Context, actor Actor, r GitRepo) error
	// UpdateGitRepo 修改一个仓库，同事务写审计。仓库不存在时返回 ErrNotFound。
	//
	// 整体替换，不做部分更新：与 UpdateSetting 同一个理由，部分更新会让
	// 审计的前后值只描述「这次提交带上了哪几个字段」，追溯时读不出全貌。
	UpdateGitRepo(ctx context.Context, actor Actor, r GitRepo) error
	// SoftDeleteGitRepo 下线一个仓库，同事务写审计。
	// 仓库不存在时返回 ErrNotFound；仍被集群绑定时返回 ErrRepoInUse。
	//
	// 「仍被绑定就拒绝」是这个方法的契约，不是调用方的责任：放在 handler
	// 里检查，等于让「先查绑定、再删仓库」之间的并发窗口决定结果，而那
	// 段窗口里刚好新建的绑定会指向一个已经不存在的仓库。
	SoftDeleteGitRepo(ctx context.Context, actor Actor, repoID string) error
	// SetGitRepoVerifyResult 只写一次仓库级只读校验的结论与时间，同事务写审计。
	//
	// 与 UpdateGitRepo 分开，理由同 SetGitVerifyResult 与 BindGitRepo 的
	// 分开：一个是操作者在下达配置变更，一个是平台在记录自己跑校验得到的
	// 判断，操作者并未提供这个结论、也不该能顺着这条路径伪造它。
	SetGitRepoVerifyResult(ctx context.Context, actor Actor, repoID string,
		result RepoVerifyResult, at time.Time) error

	// BindGitRepo 写入或替换一个集群的 Git 绑定，同事务写审计。
	// 集群或 repoId 指向的仓库不存在时返回 ErrNotFound。
	//
	// 整体替换，不做部分更新：可写字段是绑定的全部内容
	// （design doc 2026-08-13 §5）。这一步是操作者在改配置——绑到哪个仓库、
	// 哪个路径都是平台自己不会产生的值，谁改的、改成了什么，必须能
	// 从审计里单独认出来，不与 SetGitVerifyResult 的写入混在一起
	// （理由见该方法的注释）。
	BindGitRepo(ctx context.Context, actor Actor, clusterID string, b GitBinding) error
	// UnbindGitRepo 解除一个集群的 Git 绑定，同事务写审计。
	// 绑定不存在时返回 ErrNotFound。
	UnbindGitRepo(ctx context.Context, actor Actor, clusterID string) error
	// SetGitVerifyResult 只写一次路径级只读校验的结论与时间，同事务写审计。
	//
	// 与 BindGitRepo 分开而不是合成一个方法：二者的授权含义不同。
	// BindGitRepo 是操作者在下达配置变更；SetGitVerifyResult 是平台在
	// 记录自己跑校验得到的判断，操作者并未提供这个结论、也不该能顺着
	// 这条路径伪造它。合成一个方法之后，审计行就分不清"谁改了仓库
	// 地址"与"平台跑了一次校验"这两件不同的事——这正是绑定嵌在集群
	// 写模型里时已经付出的代价之一（design doc 2026-08-13 §1）。
	SetGitVerifyResult(ctx context.Context, actor Actor, clusterID string,
		result BindingVerifyResult, at time.Time) error

	// Setting 读当前的平台设置。
	//
	// **调用方不得把结果缓存成启动快照**（design doc 2026-08-13 §1.1）。
	// 轮 2 已经在这件事上出过一次事故：集群注册状态被读成一份内存清单，
	// 下线一个集群之后接口继续服务它，直到进程重启。设置更甚 —— 改了
	// host key 或 Git 超时，界面显示保存成功，运行期却纹丝不动，而把它们
	// 挪进数据库正是为了终结这件事。读取走 internal/settings.Provider。
	Setting(ctx context.Context) (PlatformSetting, error)
	// UpdateSetting 整体替换平台设置，同事务写审计。
	//
	// 整体替换而非部分更新：设置页保存的就是完整的一份，而部分更新会让
	// 审计的前后值只描述「这次提交带上了哪几个字段」，追溯时读不出全貌。
	// host key 是平台连接策略仓库时的信任锚，而它能从后台改（§1.3）——
	// 那条审计行是事后唯一能回答「信任锚是谁在什么时候换的」的东西。
	UpdateSetting(ctx context.Context, actor Actor, s PlatformSetting) error

	// Accounts 返回全部未删除的账号。
	Accounts(ctx context.Context) ([]Account, error)
	// Account 按用户名查一个未删除的账号。不存在时第二个返回值为 false。
	Account(ctx context.Context, username string) (Account, bool, error)
	// AccountPasswordHash 读一个**此刻能够登录的**账号的密码哈希，供登录
	// 时比对。账号不存在、已停用或已软删除时第二个返回值为 false。
	//
	// 没有这条路径，库里的账号永远登不进来：引导账号建出第一个管理员之后
	// 引导闸门就关上了，而那个管理员没有任何一条能拿到自己哈希的路径 ——
	// 平台在第一次装好之后立刻不可用（design doc 2026-08-14 §2）。
	//
	// 返回 PasswordHash 而不是 string：哈希从这里出来之后就再也变不回一段
	// 可以写进响应体或日志的文字（见该类型的注释）。这是本方法唯一的
	// 返回形状，也是全平台唯一一条读得到哈希的路径。
	//
	// **谓词比 Account 多一条 disabled_at IS NULL，这是有意的。** RoleOf 早已
	// 确立"停用期间与没有这个账号等价"这条处置；把它同样落在认证这一层，
	// 一个被停用的账号连会话都换不到，而不是换到一张什么都做不了的会话。
	//
	// 调用方必须在第二个返回值为 false 时自行走完一次哈希比对，否则登录
	// 就成了一个用户名探针 —— 见 auth.Verify 与 auth.dummyHash 的注释。
	AccountPasswordHash(ctx context.Context, username string) (PasswordHash, bool, error)
	// CreateAccount 新建一个账号，同事务写审计。
	//
	// passwordHash 单独传入，不挂在 Account 上（见 Account 的注释）——
	// 调用方在到达这里之前已经完成了明文到哈希的转换，本方法只管落库。
	CreateAccount(ctx context.Context, actor Actor, a Account, passwordHash string) error
	// UpdateAccountRole 修改一个账号的角色，同事务写审计。
	//
	// 账号不存在时返回 ErrNotFound；这次修改会导致库中不再有任何一个
	// 启用中的管理员时返回 ErrLastAdmin——判定必须在同一事务内完成，
	// 见 ErrLastAdmin 的注释。
	UpdateAccountRole(ctx context.Context, actor Actor, username string, role Role) error
	// DisableAccount 停用一个账号，同事务写审计。
	//
	// 账号不存在时返回 ErrNotFound；这次停用会导致库中不再有任何一个
	// 启用中的管理员时返回 ErrLastAdmin。
	//
	// 停用是本方法唯一要做的事：它不撤销该账号已签发的会话——会话本身
	// 不缓存角色，权限在每次请求时现读账号记录，停用在下一次请求就会
	// 生效，不需要一条额外的撤销路径（design doc 2026-08-14 §4）。
	DisableAccount(ctx context.Context, actor Actor, username string) error
	// EnableAccount 重新启用一个账号，同事务写审计。
	//
	// 账号不存在时返回 ErrNotFound。
	EnableAccount(ctx context.Context, actor Actor, username string) error
	// SoftDeleteAccount 软删除一个账号，同事务写审计。
	//
	// 账号不存在时返回 ErrNotFound；这次删除会导致库中不再有任何一个
	// 启用中的管理员时返回 ErrLastAdmin。用户名不复用——软删除的行仍
	// 占着主键，新账号不会继承旧账号在审计里的身份（design doc
	// 2026-08-14 §3）。
	SoftDeleteAccount(ctx context.Context, actor Actor, username string) error
	// SetAccountPassword 写入一个账号的新密码哈希，同事务写审计。
	//
	// 账号不存在时返回 ErrNotFound。管理员重置他人密码与本人改自己的
	// 密码走同一个方法——两者的区别（是否需要先校验当前密码）是调用方
	// 的职责，本方法只管把新哈希落库（design doc 2026-08-14 §6）。
	// 审计前后值只记"密码已变更"这一事实，不含哈希本身。
	SetAccountPassword(ctx context.Context, actor Actor, username string, passwordHash string) error
	// RecordBootstrapLogin 为一次引导账号登录写一条审计行，动作
	// BOOTSTRAP_LOGIN（design doc 2026-08-14 §2）。
	//
	// 这是 Store 上唯一一个没有业务写入的方法，因为这件事本身没有业务
	// 写入 —— 但它必须留痕：引导账号能登进来，意味着平台此刻一个启用中的
	// 管理员都不剩。那是一次事故，不是一次登录，而事故必须在事后可见
	// （规范 §43）。审计行仍然走与业务写入同一条事务路径，因此它与别的
	// 审计行在失败语义上没有第二套规则。
	//
	// actor 就是引导账号本身；前后值都为空：动作名、操作者与时间已经答完
	// 了"谁在什么时候从逃生口进来的"，再没有别的状态发生变化。
	RecordBootstrapLogin(ctx context.Context, actor Actor) error
}
