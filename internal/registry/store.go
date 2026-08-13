package registry

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示目标不存在。
//
// 定义在本包而非各实现里：handler 需要把它映射成业务错误码，
// 而边界层依赖某个具体存储实现的哨兵，会让将来换存储变成跨层改动。
var ErrNotFound = errors.New("registry target not found")

// Actor 是操作者身份，写入审计。
type Actor struct {
	// Username 是操作者登录名。
	Username string
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

	// PolicyImports 返回一个集群下未删除的导入策略。
	PolicyImports(ctx context.Context, clusterID string) ([]PolicyImport, error)
	// CreatePolicyImport 记录一条导入，同事务写审计。
	CreatePolicyImport(ctx context.Context, actor Actor, p PolicyImport) error
	// SoftDeletePolicyImport 删除一条导入，同事务写审计。
	SoftDeletePolicyImport(ctx context.Context, actor Actor, clusterID, importID string) error

	// RuleOverrides 返回一个集群下未删除的人工决定。
	RuleOverrides(ctx context.Context, clusterID string) ([]RuleOverride, error)
	// CreateRuleOverride 记录一条人工决定，同事务写审计。
	//
	// 同一条规则重复决定时覆盖旧值：人改主意是正常的，而两条互相矛盾
	// 的决定并存会让「这条规则到底开不开」没有答案。旧值进审计。
	CreateRuleOverride(ctx context.Context, actor Actor, o RuleOverride) error
	// SoftDeleteRuleOverride 撤销一条人工决定，同事务写审计。
	SoftDeleteRuleOverride(ctx context.Context, actor Actor, clusterID, namespace, workload, fingerprint string) error

	// BindGitRepo 写入或替换一个集群的 Git 绑定，同事务写审计。
	// 集群不存在时返回 ErrNotFound。
	//
	// 整体替换，不做部分更新：可写字段是绑定的全部内容
	// （design doc 2026-08-13 §5）。这一步是操作者在改配置——仓库地址、
	// 分支、路径都是平台自己不会产生的值，谁改的、改成了什么，必须能
	// 从审计里单独认出来，不与 SetGitVerifyResult 的写入混在一起
	// （理由见该方法的注释）。
	BindGitRepo(ctx context.Context, actor Actor, clusterID string, b GitBinding) error
	// UnbindGitRepo 解除一个集群的 Git 绑定，同事务写审计。
	// 绑定不存在时返回 ErrNotFound。
	UnbindGitRepo(ctx context.Context, actor Actor, clusterID string) error
	// SetGitVerifyResult 只写一次只读校验的结论与时间，同事务写审计。
	//
	// 与 BindGitRepo 分开而不是合成一个方法：二者的授权含义不同。
	// BindGitRepo 是操作者在下达配置变更；SetGitVerifyResult 是平台在
	// 记录自己跑校验得到的判断，操作者并未提供这个结论、也不该能顺着
	// 这条路径伪造它。合成一个方法之后，审计行就分不清"谁改了仓库
	// 地址"与"平台跑了一次校验"这两件不同的事——这正是绑定嵌在集群
	// 写模型里时已经付出的代价之一（design doc 2026-08-13 §1）。
	SetGitVerifyResult(ctx context.Context, actor Actor, clusterID string,
		result VerifyResult, at time.Time) error

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
}
