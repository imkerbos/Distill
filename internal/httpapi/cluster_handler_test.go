package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// memRegistry 是内存版 registry.Store，用于 handler 测试。
//
// 不连数据库：handler 要验证的是参数解析、错误码映射与响应形状，
// 而事务与外键行为在 internal/mysqlregistry 的集成测试里验证。
type memRegistry struct {
	clusters  map[string]registry.Cluster
	repos     map[string]registry.GitRepo
	imports   map[string][]registry.PolicyImport
	overrides map[string][]registry.RuleOverride
	failWith  error
	// failWritesWith 只让写方法失败，读仍然正常。
	//
	// 与 failWith 分开是必要的，不是方便：先读后写的 handler（重校验就是
	// 一个）在 failWith 下会停在读那一步，写路径上的错误处理根本走不到，
	// 而一条"没有泄漏"的断言在一个没被执行到的分支上永远成立。
	failWritesWith error
	// trace 记录写方法的调用序列，与 stubGitVerifier 共用一份切片。
	// 只有验证「校验发生在落库之前」的用例会用到它，其余用例留 nil。
	trace *[]string
	// lastCluster 是最近一次集群写入**拿到的原始参数**。
	//
	// 与 clusters 里存下的那份分开是必要的，不是方便：这个替身跟真实实现
	// 一样会丢弃 c.Git（绑定不走集群写路径），于是「handler 有没有把一个
	// 绑定交出来」在落库结果上根本看不见 —— 一条只看 clusters 的断言，
	// 无论 clusterPayload 还带不带 git 都会通过。
	lastCluster registry.Cluster
	// lastRepo 是最近一次仓库写入**拿到的原始参数**。
	//
	// 与 repos 里存下的那份分开，理由同 lastCluster：真实实现会把请求形状
	// 里没有的结论字段落成 NOT_VERIFIED，于是「handler 有没有把一个调用方
	// 伪造的 OK 交出来」在落库结果上根本看不见。
	lastRepo registry.GitRepo
	// setting 是这个替身持有的平台设置，见 Setting/UpdateSetting。
	setting registry.PlatformSetting
	// accounts 是这个替身持有的账号表，见本文件末尾的账号方法。
	accounts map[string]*memAccount
	// agents 是这个替身持有的 agent 表，键是公开段（全局唯一）。
	//
	// 按 agentID 而不是 (clusterID, agentID) 建索引，与真实实现的唯一索引
	// 对齐：认证路径上只有 token 在手，能拿到的只有公开段。
	agents map[string]registry.ClusterAgent
	// failAccountsWith 让账号读写失败。
	//
	// **账号路径有自己的开关，不吃 failWith / failWritesWith**，这不是
	// 图省事：那两个开关表达的是"这个端点要读写的东西不可用"，而每一个
	// 用例的装配都要先登录一次，登录与随后的每次授权判定都要读账号表。
	// 让账号表跟着一起坏，那些用例就会在装配阶段就拿不到会话，断言的
	// 对象从"这个端点怎么处理故障"悄悄变成"授权中间件怎么处理故障"——
	// 而后者在 authz_internal_test.go 里有自己的覆盖。
	failAccountsWith error
	// bootstrapLogins 记下每一次 BOOTSTRAP_LOGIN 审计写入的操作者。
	//
	// 这是替身里唯一被观察的审计痕迹：引导账号登录必须留痕（design doc
	// 2026-08-14 §2），而 registry.Store 刻意不暴露审计的读路径，测试
	// 只能从这里断言那条写入确实发生过。
	bootstrapLogins []string
	// policyExports 记下每一次 EXPORT_POLICY 审计写入的内容。
	//
	// 与 bootstrapLogins 同理：registry.Store 刻意不暴露审计的读路径，
	// 而「导出必须留痕」（design doc 2026-08-14 §5，规范 §43）只能从这里
	// 断言。记的是整条记录而不是一个计数 —— 一条只数次数的断言，对一次
	// 把命名空间或时间窗记错的导出照样是绿的。
	policyExports []registry.PolicyExport
	// writebackPlans 与 writebackPushes 分别记下 PLAN_POLICY_WRITEBACK 与
	// PUSH_POLICY_WRITEBACK 两个动作各自写下的审计内容。
	//
	// 分成两个切片而不是一个带动作名的列表：「出计划与推送是两个动作」
	// 这条（design doc 2026-08-14 §9）只有在两者分得开时才断言得了，
	// 而一个共用的列表会让把两者写成同一个动作的实现照样通过。
	// 方法本身在 writeback_handler_test.go 里。
	writebackPlans  []memWriteback
	writebackPushes []memWriteback
}

// memAccount 是替身里的一行账号，比 registry.Account 多一个哈希。
//
// 哈希留在替身内部、不出现在任何读方法的返回里（AccountPasswordHash 除外，
// 而它回的是 registry.PasswordHash）：真实实现的读路径根本不 SELECT 这一列
// （见 mysqlregistry.accountColumns），替身若把它挂在读模型上，"某条响应
// 把哈希带出去了"就会在 handler 测试里表现成正常。
type memAccount struct {
	account   registry.Account
	hash      string
	deletedAt *time.Time
}

// writeErr 返回本次写调用该失败的错误，两个字段都没设时为 nil。
func (m *memRegistry) writeErr() error {
	if m.failWith != nil {
		return m.failWith
	}
	return m.failWritesWith
}

// record 记一次写调用。
func (m *memRegistry) record(op string) {
	if m.trace != nil {
		*m.trace = append(*m.trace, op)
	}
}

func newMemRegistry() *memRegistry {
	return &memRegistry{
		clusters:  map[string]registry.Cluster{},
		repos:     map[string]registry.GitRepo{},
		imports:   map[string][]registry.PolicyImport{},
		overrides: map[string][]registry.RuleOverride{},
		accounts:  map[string]*memAccount{},
		agents:    map[string]registry.ClusterAgent{},
		// 一份能过 ValidatePlatformSetting 的设置：零值那份读出来是
		// 「会话立即过期、超时保护关掉」，不是一个可用的初始状态。
		setting: registry.PlatformSetting{
			SessionTTL:          8 * time.Hour,
			HTTPReadTimeout:     10 * time.Second,
			HTTPWriteTimeout:    20 * time.Second,
			HTTPShutdownTimeout: 15 * time.Second,
			SecretsBackend:      registry.SecretsBackendNone,
			GitVerifyTimeout:    10 * time.Second,
			GitWriteTimeout:     15 * time.Second,
		},
	}
}

// SetOtherPlanes 记下策略平面探测的结论。
//
// 替身里真的记下来而不是丢掉：有用例要断言"写下的是 UNKNOWN 而不是 NONE"，
// 而一个把它吞掉的替身会让那条断言永远通过。
func (m *memRegistry) SetOtherPlanes(
	_ context.Context, clusterID string, planes registry.PolicyPlanes,
) error {
	c, ok := m.clusters[clusterID]
	if !ok {
		return registry.ErrNotFound
	}
	c.OtherPlanes = planes
	m.clusters[clusterID] = c
	return nil
}

// SetCNI 真的记下来，不吞掉：有用例断言"采集之后界面上能看到 CNI"，
// 而一个把它丢掉的替身会让那条断言永远失败在一个与被测行为无关的地方。
func (m *memRegistry) SetCNI(
	_ context.Context, clusterID string, cni cluster.CNI,
) error {
	c, ok := m.clusters[clusterID]
	if !ok {
		return registry.ErrNotFound
	}
	c.CNI = cni
	m.clusters[clusterID] = c
	return nil
}

func (m *memRegistry) Clusters(context.Context) ([]registry.Cluster, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	out := make([]registry.Cluster, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, c)
	}
	return out, nil
}

func (m *memRegistry) Cluster(_ context.Context, id string) (registry.Cluster, bool, error) {
	if m.failWith != nil {
		return registry.Cluster{}, false, m.failWith
	}
	c, ok := m.clusters[id]
	return c, ok, nil
}

// CreateCluster 与 mysqlregistry 一样**忽略 c.Git**：绑定有自己的写入路径
// （BindGitRepo）与自己的审计行。一个会顺手把集群对象上的绑定落下来的替身，
// 会让「集群写路径悄悄改了绑定」这类 bug 在 handler 测试里全绿通过。
func (m *memRegistry) CreateCluster(_ context.Context, _ registry.Actor, c registry.Cluster) error {
	m.record("create")
	m.lastCluster = c
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	c.Git = nil
	m.clusters[c.ID] = c
	return nil
}

// UpdateCluster 同样不碰绑定：真实实现的 UPDATE 语句里根本没有
// cluster_git_binding 这张表，改一次网段不会顺手解绑。
func (m *memRegistry) UpdateCluster(_ context.Context, _ registry.Actor, c registry.Cluster) error {
	m.record("update")
	m.lastCluster = c
	if err := m.writeErr(); err != nil {
		return err
	}
	existing, ok := m.clusters[c.ID]
	if !ok {
		return registry.ErrNotFound
	}
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	c.Git = existing.Git
	m.clusters[c.ID] = c
	return nil
}

// BindGitRepo 写入或替换绑定。
//
// 与 mysqlregistry.BindGitRepo 一样先过 ValidateGitBinding、且**不**校验
// 集群其余字段：这次拆分买到的正是这一条，替身若在这里顺手跑一遍
// ValidateCluster，「绑定的合法性与集群其余字段无关」就没有东西守得住了。
func (m *memRegistry) BindGitRepo(
	_ context.Context, _ registry.Actor, clusterID string, b registry.GitBinding,
) error {
	m.record("bind")
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidateGitBinding(b); err != nil {
		return err
	}
	c, ok := m.clusters[clusterID]
	if !ok {
		return registry.ErrNotFound
	}
	// 与 mysqlregistry 一样断言仓库存在：绑定表上有一把指向 git_repo 的
	// 外键，一个绑到不存在仓库的替身会让 handler 层「先确认仓库在不在」
	// 这条前置判断失去意义。
	if _, ok := m.repos[b.RepoID]; !ok {
		return registry.ErrNotFound
	}
	// 空值按 NOT_VERIFIED 落库，与真实实现一致：空串不是登记过的枚举值。
	if b.VerifyResult == "" {
		b.VerifyResult = registry.BindingVerifyNotVerified
	}
	c.Git = &b
	m.clusters[clusterID] = c
	return nil
}

// GitRepos 返回全部已登记的仓库。
func (m *memRegistry) GitRepos(context.Context) ([]registry.GitRepo, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	out := make([]registry.GitRepo, 0, len(m.repos))
	for _, r := range m.repos {
		out = append(out, r)
	}
	return out, nil
}

func (m *memRegistry) GitRepo(_ context.Context, repoID string) (registry.GitRepo, bool, error) {
	if m.failWith != nil {
		return registry.GitRepo{}, false, m.failWith
	}
	r, ok := m.repos[repoID]
	return r, ok, nil
}

// CreateGitRepo 与 mysqlregistry 一样先过 ValidateGitRepo，并把空结论落成
// NOT_VERIFIED：这个替身若比真实实现宽松，handler 测试就会在一条真实实现
// 会拒绝的输入上通过。
func (m *memRegistry) CreateGitRepo(_ context.Context, _ registry.Actor, r registry.GitRepo) error {
	m.record("create-repo")
	m.lastRepo = r
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidateGitRepo(r); err != nil {
		return err
	}
	if r.VerifyResult == "" {
		r.VerifyResult = registry.RepoVerifyNotVerified
	}
	m.repos[r.ID] = r
	return nil
}

// UpdateGitRepo 整体替换一个仓库，与真实实现一样把结论清成 NOT_VERIFIED ——
// 换了地址之后，旧的 OK 描述的是另一个仓库。
func (m *memRegistry) UpdateGitRepo(_ context.Context, _ registry.Actor, r registry.GitRepo) error {
	m.record("update-repo")
	m.lastRepo = r
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidateGitRepo(r); err != nil {
		return err
	}
	if _, ok := m.repos[r.ID]; !ok {
		return registry.ErrNotFound
	}
	if r.VerifyResult == "" {
		r.VerifyResult = registry.RepoVerifyNotVerified
	}
	m.repos[r.ID] = r
	return nil
}

// SoftDeleteGitRepo 下线一个仓库，仍被绑定时返回 registry.ErrRepoInUse。
//
// 「仍被绑定就拒绝」写在这里而不是留给 handler，与真实实现一致
// （design doc §4）：替身若不带这条，边界层把 ErrRepoInUse 映射成业务失败
// 的那段代码就永远不会被执行到，而一条断言在没被执行到的分支上永远成立。
func (m *memRegistry) SoftDeleteGitRepo(_ context.Context, _ registry.Actor, repoID string) error {
	m.record("delete-repo")
	if err := m.writeErr(); err != nil {
		return err
	}
	if _, ok := m.repos[repoID]; !ok {
		return registry.ErrNotFound
	}
	for _, c := range m.clusters {
		if c.Git != nil && c.Git.RepoID == repoID {
			return fmt.Errorf("%w: repo %s is bound to cluster %s", registry.ErrRepoInUse, repoID, c.ID)
		}
	}
	delete(m.repos, repoID)
	return nil
}

// SetGitRepoVerifyResult 只写仓库级结论与时间。
//
// 只动这两个字段而不是整体替换仓库：真实实现的 UPDATE 只有 verify_result
// 与 verified_at 两列，一个顺手重写整行的替身会让「跑一次校验改写了仓库
// 地址」这种事在测试里看不出来。
func (m *memRegistry) SetGitRepoVerifyResult(
	_ context.Context, _ registry.Actor, repoID string,
	result registry.RepoVerifyResult, at time.Time,
) error {
	m.record("set-repo-verdict")
	if err := m.writeErr(); err != nil {
		return err
	}
	if !result.Valid() {
		return registry.NewInvalidError("verifyResult 不在已登记的取值范围内")
	}
	r, ok := m.repos[repoID]
	if !ok {
		return registry.ErrNotFound
	}
	r.VerifyResult = result
	r.VerifiedAt = &at
	m.repos[repoID] = r
	return nil
}

// UnbindGitRepo 解除绑定。集群不存在与未绑定同样返回 ErrNotFound ——
// 两者从调用方视角都是「要解绑的那个东西不在」。
func (m *memRegistry) UnbindGitRepo(_ context.Context, _ registry.Actor, clusterID string) error {
	m.record("unbind")
	if err := m.writeErr(); err != nil {
		return err
	}
	c, ok := m.clusters[clusterID]
	if !ok || c.Git == nil {
		return registry.ErrNotFound
	}
	c.Git = nil
	m.clusters[clusterID] = c
	return nil
}

// SetGitVerifyResult 只写结论与时间。
//
// 只动这两个字段而不是整体替换绑定：真实实现的 UPDATE 只有 verify_result
// 与 verified_at 两列，一个顺手重写整行的替身会让「跑一次校验改写了仓库
// 地址」这种事在测试里看不出来。
func (m *memRegistry) SetGitVerifyResult(
	_ context.Context, _ registry.Actor, clusterID string,
	result registry.BindingVerifyResult, at time.Time,
) error {
	m.record("set-verdict")
	if err := m.writeErr(); err != nil {
		return err
	}
	if !result.Valid() {
		return registry.NewInvalidError("verifyResult 不在已登记的取值范围内")
	}
	c, ok := m.clusters[clusterID]
	if !ok || c.Git == nil {
		return registry.ErrNotFound
	}
	g := *c.Git
	g.VerifyResult = result
	g.VerifiedAt = &at
	c.Git = &g
	m.clusters[clusterID] = c
	return nil
}

func (m *memRegistry) SoftDeleteCluster(_ context.Context, _ registry.Actor, id string) error {
	if m.failWith != nil {
		return m.failWith
	}
	if _, ok := m.clusters[id]; !ok {
		return registry.ErrNotFound
	}
	delete(m.clusters, id)
	// 下线顺带吊销这个集群的 agent 凭据，与真实实现同一个事务里做的事。
	// 假实现漏掉它，就会让「下线之后凭据仍是 ACTIVE」这件事在单测里
	// 永远不成立，而它恰恰是要被测到的那一半。
	for id2, a := range m.agents {
		if a.ClusterID == id && a.State == registry.AgentActive {
			a.State = registry.AgentRevoked
			a.RevokedAt = time.Now().UTC()
			m.agents[id2] = a
		}
	}
	return nil
}

func (m *memRegistry) PolicyImports(_ context.Context, clusterID string) ([]registry.PolicyImport, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	return m.imports[clusterID], nil
}

func (m *memRegistry) CreatePolicyImport(_ context.Context, _ registry.Actor, p registry.PolicyImport) error {
	if m.failWith != nil {
		return m.failWith
	}
	// 与 mysqlregistry.CreatePolicyImport 一样先过校验：这个替身若比真实
	// 实现宽松，handler 测试就会在一条真实实现会拒绝的输入上通过。
	if err := registry.ValidatePolicyImport(p); err != nil {
		return err
	}
	m.imports[p.ClusterID] = append(m.imports[p.ClusterID], p)
	return nil
}

func (m *memRegistry) SoftDeletePolicyImport(_ context.Context, _ registry.Actor, clusterID, importID string) error {
	if m.failWith != nil {
		return m.failWith
	}
	kept := m.imports[clusterID][:0]
	for _, p := range m.imports[clusterID] {
		if p.ImportID != importID {
			kept = append(kept, p)
		}
	}
	m.imports[clusterID] = kept
	return nil
}

func (m *memRegistry) RuleOverrides(_ context.Context, clusterID string) ([]registry.RuleOverride, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	return m.overrides[clusterID], nil
}

// CreateRuleOverride 与 mysqlregistry 一样覆盖旧值而非报冲突：这个替身若
// 让重复决定并存，handler 测试就验证不出「重复决定该覆盖」这条约束。
func (m *memRegistry) CreateRuleOverride(_ context.Context, _ registry.Actor, o registry.RuleOverride) error {
	if m.failWith != nil {
		return m.failWith
	}
	if err := registry.ValidateOverride(o); err != nil {
		return err
	}
	list := m.overrides[o.ClusterID]
	for i, existing := range list {
		if existing.Namespace == o.Namespace && existing.Workload == o.Workload &&
			existing.Fingerprint == o.Fingerprint {
			list[i] = o
			return nil
		}
	}
	m.overrides[o.ClusterID] = append(list, o)
	return nil
}

func (m *memRegistry) SoftDeleteRuleOverride(
	_ context.Context, _ registry.Actor, clusterID, namespace, workload, fingerprint string,
) error {
	if m.failWith != nil {
		return m.failWith
	}
	list := m.overrides[clusterID]
	for i, existing := range list {
		if existing.Namespace == namespace && existing.Workload == workload &&
			existing.Fingerprint == fingerprint {
			m.overrides[clusterID] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return registry.ErrNotFound
}

// setting 是这个替身持有的平台设置。
//
// 与真实实现一样在 UpdateSetting 里过一次 ValidatePlatformSetting：替身若
// 比真实实现宽松，设置端点的测试就会在一份真实实现会拒绝的输入上通过。
// 校验器本身仍由装配方（cmd/distill-api）按当前设置现装，httpapi 只拿到
// 一个 GitVerifier 接口 —— 这两个方法服务的是 /settings 那两个端点。
func (m *memRegistry) Setting(context.Context) (registry.PlatformSetting, error) {
	if m.failWith != nil {
		return registry.PlatformSetting{}, m.failWith
	}
	return m.setting, nil
}

func (m *memRegistry) UpdateSetting(_ context.Context, _ registry.Actor, s registry.PlatformSetting) error {
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidatePlatformSetting(s); err != nil {
		return err
	}
	// 变更约束与真实实现同处一层（mysqlregistry.UpdateSetting）：替身漏了
	// 这一条，「一次 PUT 抹掉信任锚」就会在设置端点的测试里表现成成功。
	if err := registry.ValidateSettingUpdate(m.setting, s); err != nil {
		return err
	}
	m.record("UpdateSetting")
	m.setting = s
	return nil
}

// —— 账号 ——
//
// 这一组方法与真实实现（internal/mysqlregistry/account.go）在**拒绝的
// 条件上**保持一致：软删除与停用的可见性、最后一个管理员的保护、用户名
// 不复用。替身若比真实实现宽松，账号端点的测试就会在一条真实实现会拒绝
// 的输入上通过 —— 与本文件其余替身方法同一条纪律。

// accountErr 返回本次账号调用该失败的错误，见 failAccountsWith。
func (m *memRegistry) accountErr() error {
	return m.failAccountsWith
}

// live 返回一个未软删除的账号。
func (m *memRegistry) live(username string) (*memAccount, bool) {
	a, ok := m.accounts[username]
	if !ok || a.deletedAt != nil {
		return nil, false
	}
	return a, true
}

// lastEnabledAdmin 判断 username 是不是此刻唯一一个启用中的管理员。
//
// 真实实现在事务里加锁做同一件判定（requireAdminRemains）：替身漏了这条，
// 边界层把 ErrLastAdmin 映射成业务失败的那段代码就永远不会被执行到，而一条
// 断言在没被执行到的分支上永远成立。
func (m *memRegistry) lastEnabledAdmin(username string) bool {
	var admins []string
	for name, a := range m.accounts {
		if a.deletedAt == nil && a.account.DisabledAt == nil && a.account.Role == registry.RoleAdmin {
			admins = append(admins, name)
		}
	}
	return len(admins) == 1 && admins[0] == username
}

func (m *memRegistry) Accounts(context.Context) ([]registry.Account, error) {
	if err := m.accountErr(); err != nil {
		return nil, err
	}
	out := make([]registry.Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a.deletedAt == nil {
			out = append(out, a.account)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (m *memRegistry) Account(_ context.Context, username string) (registry.Account, bool, error) {
	if err := m.accountErr(); err != nil {
		return registry.Account{}, false, err
	}
	a, ok := m.live(username)
	if !ok {
		return registry.Account{}, false, nil
	}
	return a.account, true, nil
}

// AccountPasswordHash 的谓词比 Account 多一条「未停用」，与真实实现一致：
// 一个被停用的账号连会话都换不到，而不是换到一张什么都做不了的会话。
func (m *memRegistry) AccountPasswordHash(
	_ context.Context, username string,
) (registry.PasswordHash, bool, error) {
	if err := m.accountErr(); err != nil {
		return registry.PasswordHash{}, false, err
	}
	a, ok := m.live(username)
	if !ok || a.account.DisabledAt != nil {
		return registry.PasswordHash{}, false, nil
	}
	return registry.NewPasswordHash(a.hash), true, nil
}

func (m *memRegistry) CreateAccount(
	_ context.Context, _ registry.Actor, a registry.Account, passwordHash string,
) error {
	m.record("CreateAccount")
	if err := m.accountErr(); err != nil {
		return err
	}
	if err := registry.ValidateAccount(a); err != nil {
		return err
	}
	if passwordHash == "" {
		return registry.NewInvalidError("密码哈希不能为空")
	}
	// 软删除的行仍占着主键：用户名不复用是设计的一部分，复用会让新账号
	// 继承旧账号在审计里的身份（design doc 2026-08-14 §3）。
	if _, taken := m.accounts[a.Username]; taken {
		return registry.NewInvalidError("用户名已被占用（含已删除的），请换一个")
	}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	created := a
	created.DisabledAt = nil
	created.CreatedAt = now
	created.UpdatedAt = now
	m.accounts[a.Username] = &memAccount{account: created, hash: passwordHash}
	return nil
}

func (m *memRegistry) UpdateAccountRole(
	_ context.Context, _ registry.Actor, username string, role registry.Role,
) error {
	m.record("UpdateAccountRole")
	if err := m.accountErr(); err != nil {
		return err
	}
	if !role.Valid() {
		return registry.NewInvalidError("角色不在已登记的取值范围内")
	}
	a, ok := m.live(username)
	if !ok {
		return registry.ErrNotFound
	}
	// 降级最后一个启用中的管理员会让平台再也没有人能管理它。
	if role != registry.RoleAdmin && m.lastEnabledAdmin(username) {
		return fmt.Errorf("%w: %s", registry.ErrLastAdmin, username)
	}
	a.account.Role = role
	return nil
}

func (m *memRegistry) DisableAccount(_ context.Context, _ registry.Actor, username string) error {
	m.record("DisableAccount")
	if err := m.accountErr(); err != nil {
		return err
	}
	a, ok := m.live(username)
	if !ok {
		return registry.ErrNotFound
	}
	if m.lastEnabledAdmin(username) {
		return fmt.Errorf("%w: %s", registry.ErrLastAdmin, username)
	}
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	a.account.DisabledAt = &at
	return nil
}

func (m *memRegistry) EnableAccount(_ context.Context, _ registry.Actor, username string) error {
	m.record("EnableAccount")
	if err := m.accountErr(); err != nil {
		return err
	}
	a, ok := m.live(username)
	if !ok {
		return registry.ErrNotFound
	}
	a.account.DisabledAt = nil
	return nil
}

func (m *memRegistry) SoftDeleteAccount(_ context.Context, _ registry.Actor, username string) error {
	m.record("SoftDeleteAccount")
	if err := m.accountErr(); err != nil {
		return err
	}
	a, ok := m.live(username)
	if !ok {
		return registry.ErrNotFound
	}
	if m.lastEnabledAdmin(username) {
		return fmt.Errorf("%w: %s", registry.ErrLastAdmin, username)
	}
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	a.deletedAt = &at
	return nil
}

func (m *memRegistry) SetAccountPassword(
	_ context.Context, _ registry.Actor, username string, passwordHash string,
) error {
	m.record("SetAccountPassword")
	if err := m.accountErr(); err != nil {
		return err
	}
	if passwordHash == "" {
		return registry.NewInvalidError("密码哈希不能为空")
	}
	a, ok := m.live(username)
	if !ok {
		return registry.ErrNotFound
	}
	a.hash = passwordHash
	return nil
}

// RecordBootstrapLogin 不进 trace：那份序列是用来给**一次被测操作**里的
// 校验与写入排序的，而引导登录发生在装配阶段，混进去只会让每条顺序断言
// 都多出一个与被测行为无关的头部。它自己的观察通道是 bootstrapLogins。
// RecordPolicyExport 走 writeErr：导出的审计写失败必须能让整次导出失败，
// 而那条路径只有在替身也会失败时才走得到。
func (m *memRegistry) RecordPolicyExport(
	_ context.Context, actor registry.Actor, e registry.PolicyExport,
) error {
	m.record("RecordPolicyExport")
	if err := m.writeErr(); err != nil {
		return err
	}
	if actor.Username == "" {
		// 审计要答得出"谁"。空操作者在真实实现里会写进 actor 列，
		// 事后读出来是一条无主的导出记录。
		return registry.NewInvalidError("审计缺少操作者")
	}
	m.policyExports = append(m.policyExports, e)
	return nil
}

func (m *memRegistry) RecordBootstrapLogin(_ context.Context, actor registry.Actor) error {
	if err := m.accountErr(); err != nil {
		return err
	}
	m.bootstrapLogins = append(m.bootstrapLogins, actor.Username)
	return nil
}

// withAccount 往替身里塞一个账号，返回它的明文密码。
//
// 走 CreateAccount 而不是直接写 map：建号路径上的那几条拒绝（角色合法、
// 哈希非空、用户名不复用）因此对测试数据同样生效，测试就不可能在一个
// 真实实现建不出来的账号上做断言。
func (m *memRegistry) withAccount(t *testing.T, username string, role registry.Role, password string) {
	t.Helper()
	// MinCost 而不是 DefaultCost：这些账号只是测试夹具，而一次 DefaultCost
	// 哈希要几十毫秒，几十个用例累加起来就是分钟级。被测代码自己用的仍是
	// DefaultCost。
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash %s: %v", username, err)
	}
	a := registry.Account{Username: username, Role: role}
	if err := m.CreateAccount(context.Background(), registry.Actor{Username: "test"}, a, string(hash)); err != nil {
		t.Fatalf("seed account %s: %v", username, err)
	}
}

// authedPostJSON 与 session_handler_test.go 的 postJSON 同形，多带一个
// 会话 Cookie —— 同名会与那个无 Cookie 的版本签名冲突，所以另起一个
// 名字，呼应已有的 authedGet。
func authedPostJSON(t *testing.T, h http.Handler, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postJSONNoAuth 就是 session_handler_test.go 里那个不带 Cookie 的
// postJSON —— 起别名是为了让调用点读起来能看出"故意不登录"这层意图。
func postJSONNoAuth(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, h, path, body)
}

func authedPutJSON(t *testing.T, h http.Handler, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateClusterRequiresSession(t *testing.T) {
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// 请求体不是合法 JSON 是协议层问题，不是业务失败 —— 与 handleCreateSession
// 对畸形登录请求的处理保持一致（见 session_handler.go），必须是真实的 400，
// 不是 200 + code。这条路径此前没有测试锁住，是审阅时点出的相邻缺口。
func TestCreateClusterRejectsMalformedJSON(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

func TestUpdateClusterRejectsMalformedJSON(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	req := httptest.NewRequest(http.MethodPut, "/api/v1/clusters/c1", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

func TestCreateClusterRoundTrips(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "new-1", "displayName": "New", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "state": "REGISTERED",
		"apiServers":         []map[string]any{{"host": "10.9.0.2", "cidr": "10.9.0.0/28", "port": 443}},
		"healthCheckSources": []string{"35.191.0.0/16"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0", got)
	}
	if _, ok := reg.clusters["new-1"]; !ok {
		t.Error("cluster was not stored")
	}
}

// 集群必须能通过接口说出它的 kubeconfig 引用。
//
// 这条是 2026-08-17 第一次对真实 kind 集群跑采集时撞出来的：迁移 000010
// 建了 kubeconfig_ref 列，registry.Cluster 有这个字段，ValidateCluster 校验
// 它，mysqlregistry 读写它 —— 但 clusterPayload 里没有这个键，toCluster()
// 也不赋值，于是**没有任何路径能把它设成非空**，采集器永远拿不到凭据。
// 每一层都有，只差写入它的那一层，而所有既有测试都直接构造
// registry.Cluster，绕过了这一层。
//
// 断言读回来的值而不是"请求成功"：一个收下再丢掉的字段同样会返回 200。
func TestCreateClusterStoresTheKubeconfigReference(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "cred-1", "displayName": "Cred", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "kubeconfigRef": "kind-distill",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got, ok := reg.clusters["cred-1"]
	if !ok {
		t.Fatal("cluster was not stored")
	}
	if got.KubeconfigRef != "kind-distill" {
		t.Errorf("KubeconfigRef = %q, want %q: without it the collector can never "+
			"reach this cluster, and the failure surfaces only at collection time",
			got.KubeconfigRef, "kind-distill")
	}
}

// 非法的引用必须被拒，且要点名是哪个字段。
//
// 引用会被拼进凭据后端的资源路径，能表达的字符越多，能表达的越权路径越多
// （secrets.ValidateRef）。校验早就在 registry 里了，这条钉的是它确实被
// 这条 HTTP 路径走到 —— 一个根本不传字段的实现同样能让上一条通过。
func TestCreateClusterRejectsAMalformedKubeconfigReference(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "cred-bad", "displayName": "Bad", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "kubeconfigRef": "../../etc/passwd",
	})
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001", got)
	}
	if msg, _ := bodyOf(t, rec)["msg"].(string); !strings.Contains(msg, "kubeconfigRef") {
		t.Errorf("msg = %q, want it to name kubeconfigRef", msg)
	}
}

// 修改集群时，引用同样要写进去。
//
// PUT 写整行（见 handleUpdateCluster 的注释），所以漏掉这个字段的后果不是
// "改不了"，而是**每一次改集群都会把已经登记好的凭据引用清空** ——
// 改一次显示名，采集器就再也连不上了。
func TestUpdateClusterWritesTheKubeconfigReference(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	if rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "cred-2", "displayName": "Cred", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "kubeconfigRef": "first-ref",
	}); rec.Code != http.StatusOK {
		t.Fatalf("create status = %d (body %s)", rec.Code, rec.Body.String())
	}

	if rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/cred-2", map[string]any{
		"id": "cred-2", "displayName": "Cred renamed", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "kubeconfigRef": "second-ref",
	}); rec.Code != http.StatusOK {
		t.Fatalf("update status = %d (body %s)", rec.Code, rec.Body.String())
	}

	if got := reg.clusters["cred-2"].KubeconfigRef; got != "second-ref" {
		t.Errorf("KubeconfigRef after update = %q, want %q", got, "second-ref")
	}
}

// 网段写错是业务失败，不该计入服务错误率，也不该只回一句「参数不合法」——
// 一个集群有四类网段，不说是哪一类会让操作者逐个试。
func TestCreateClusterRejectsMalformedCIDR(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "bad-1", "displayName": "Bad", "podCidr": "10.20.0/14",
		"nodeCidr": "10.140.0.0/20", "state": "REGISTERED",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad field value is a business failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
	// msg 必须点名是哪一类网段错了：一个集群有四类网段，只回一句
	// 「参数不合法」会让操作者逐个试。这段文案是 registry.ValidateCluster
	// 自己写的，不是驱动或第三方库的错误文本，回传它不泄露任何内部拓扑。
	if msg, _ := bodyOf(t, rec)["msg"].(string); !strings.Contains(msg, "podCIDR") {
		t.Errorf("msg = %q, want it to name podCIDR", msg)
	}
}

// 接入状态由服务端决定：spec 要求创建一律从 REGISTERED 起步，只在字段
// 为空时兜底不足以兑现这句话 —— 一个显式的 {"state":"READY"} 必须
// 同样被忽略，否则调用方可以把「还没有数据」标成「可以出推荐了」。
func TestCreateClusterIgnoresCallerSuppliedState(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "sneaky-1", "displayName": "Sneaky", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "state": "READY",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	stored, ok := reg.clusters["sneaky-1"]
	if !ok {
		t.Fatal("cluster was not stored")
	}
	if stored.State != registry.StateRegistered {
		t.Errorf("state = %q, want REGISTERED — an explicit READY in the request must be ignored", stored.State)
	}
}

// 更新必须保留库里已有的接入状态：既不能被请求体里任意的 state 值
// 改写，也不该在修改网段这类操作时被悄悄打回 REGISTERED。
func TestUpdateClusterPreservesExistingState(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", map[string]any{
		"displayName": "C1 renamed", "podCidr": "10.4.0.0/14",
		"nodeCidr": "10.128.0.0/20", "state": "REGISTERED",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if reg.clusters["c1"].State != registry.StateReady {
		t.Errorf("state = %q, want READY to survive the update", reg.clusters["c1"].State)
	}
	if reg.clusters["c1"].DisplayName != "C1 renamed" {
		t.Errorf("displayName = %q, want the update applied", reg.clusters["c1"].DisplayName)
	}
}

// 端点是整体替换，动词必须说的是同一件事。PATCH 在 HTTP 里承诺的是
// 「只改我给的字段」，而这个 handler 写整行 —— 留着这条路由，第一个
// 按 PATCH 语义只发 {"git":{...}} 的调用方就会把 podCIDR 清成空串。
// 它不该被友好地接受，也不该被静默改写，而该在路由层就不存在。
func TestUpdateClusterRejectsPatchVerb(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/clusters/c1",
		bytes.NewReader([]byte(`{"displayName":"C1"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 — PATCH must not name a full-replacement endpoint", rec.Code)
	}
}

// 整体替换必须表现得像整体替换：只带 displayName 的请求体不会被合并进
// 现有行，它是一个缺了 podCIDR/nodeCIDR 的完整集群，因此被校验拒绝。
//
// 这条测试守的是「不要好心补全」：一旦有人在 handler 里用库里的值填上
// 请求体没给的字段，这个请求就会成功 —— 而成功的代价是调用方从此无法
// 表达「把这一项清空」，且 PUT 的语义与实现再次分家。
func TestUpdateClusterIsReplacementNotMerge(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", map[string]any{
		"displayName": "C1 renamed",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a bad body is a business failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 — a name-only body is not a complete cluster", got)
	}
	// 被拒绝的请求不得留下任何痕迹：一次半成功的替换比一次失败更难排查。
	if got := reg.clusters["c1"].PodCIDR; got != "10.4.0.0/14" {
		t.Errorf("podCIDR = %q, want it untouched by a rejected update", got)
	}
}

// fullClusterBody 是一个完整的集群 PUT 请求体。
//
// extra 里的键会并进顶层对象，用于往请求体里塞那些**不该被采纳**的字段 ——
// 请求体必须能表达它们，测试才证明得了它们被忽略。
func fullClusterBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"displayName": "C1", "podCidr": "10.4.0.0/14", "nodeCidr": "10.128.0.0/20",
		"apiServers":         []map[string]any{{"host": "10.9.0.2", "cidr": "10.9.0.0/28", "port": 443}},
		"healthCheckSources": []string{"35.191.0.0/16"},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// 集群写入永不发出站：绑定已经不在这条路径上了，一次 SSH 握手在这里
// 既没有请求体上的目标，也会在远端日志里留下一条没人能解释的连接。
//
// 两个动词都打：create 与 update 是两处独立的调用点，只测一个的话，
// 另一处留着 verifyOnSave 也不会有测试变红。
func TestClusterWritesNeverReachOut(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			reg := newMemRegistry()
			reg.clusters["c1"] = boundCluster()
			stub := &stubGitVerifier{result: registry.BindingVerifyOK}
			h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

			body := fullClusterBody(nil)
			if method == http.MethodPost {
				body["id"] = "new-4"
				if got := bodyOf(t, authedPostJSON(t, h, cookie, "/api/v1/clusters", body))["code"]; got != float64(0) {
					t.Fatalf("code = %v, want 0", got)
				}
			} else if got := bodyOf(t, authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", body))["code"]; got != float64(0) {
				t.Fatalf("code = %v, want 0", got)
			}
			if stub.calls != 0 {
				t.Errorf("verifier calls = %d, want 0 — a cluster write has no binding to verify", stub.calls)
			}
		})
	}
}

// 改集群不得动绑定。
//
// 绑定有自己的生命周期与自己的审计行；一次改网段顺手把绑定清掉，是
// 绑定还嵌在集群写模型里时才会发生的事，而它不会报错 —— 表现只是
// 某个集群某天起不再下发策略。
func TestUpdateClusterLeavesTheBindingAlone(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", fullClusterBody(nil))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	stored := reg.clusters["c1"].Git
	if stored == nil || *stored != *boundCluster().Git {
		t.Errorf("git = %+v, want the binding untouched by a cluster update", stored)
	}
}

func TestUpdateClusterUnknownIsBusinessNotFound(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/no-such", map[string]any{
		"displayName": "X", "podCidr": "10.4.0.0/14", "nodeCidr": "10.128.0.0/20",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a missing resource is not a server fault", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}

func TestClustersEndpointReadsRegistry(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateRegistered,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedGet(t, h, cookie, "/api/v1/clusters")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	data, ok := bodyOf(t, rec)["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want one cluster from the registry", bodyOf(t, rec)["data"])
	}
}

// registry 内部故障（比如数据库连不上）必须计入服务错误率，走真实的
// 500 —— 但错误细节只进日志，不能顺着响应把内部拓扑（主机、端口）
// 交给调用方。这一条与 writeReaderError 对 Reader 故障的处理对称，
// 因为两者共享同一个"该不该计入服务错误率"的判据。
func TestCreateClusterFailurePropagatesRegistryInternalError(t *testing.T) {
	reg := newMemRegistry()
	reg.failWith = errors.New("mysql: dial tcp 10.0.0.5:3306: connection refused")
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "x", "displayName": "X", "podCidr": "10.4.0.0/14", "nodeCidr": "10.128.0.0/20",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(50001) {
		t.Errorf("code = %v, want 50001", got)
	}
	// 与 ErrInvalid 分支的对照组：这条错误不是 registry 自己写的校验文案，
	// 是驱动的原始报错，所以 msg 必须是固定文案，一个字都不能带出去 ——
	// WriteInvalid 只喂给「我们自己写的」错误，这里走的是 default 分支，
	// 还是 response.WriteSystem，行为不该受这次改动影响。
	if got := bodyOf(t, rec)["msg"]; got != response.CodeInternal.Message() {
		t.Errorf("msg = %q, want the fixed internal-error message %q", got, response.CodeInternal.Message())
	}
	for _, secret := range []string{"mysql", "10.0.0.5", "3306"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("response leaked %q: %s", secret, rec.Body.String())
		}
	}
}

func TestDeleteClusterRoundTrips(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateRegistered,
	}
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/clusters/c1", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if _, ok := reg.clusters["c1"]; ok {
		t.Error("cluster still present after delete")
	}
}

// gitBindingRef 是 Secret Manager 中那条引用的取值，凭据本身永不入库。
//
// 提成常量而不是写在两处字面量里：除了让期望值与请求体共用同一个值，
// 也避免 gosec G101 把「字段名带 credential」的字面量当成硬编码凭据 ——
// 这里存的是引用，不是凭据，而消掉误报比挂一条 //nolint 更诚实。
const gitBindingRef = "distill-git"

// 请求体 → 领域对象的整体比对：逐字段挑着断言挡不住漏映射。
//
// 审阅时的实证是 —— 把 toCluster 里 APIServers / HealthCheckSources
// 两行赋值删掉，./internal/httpapi 全绿。而这两项正是 control-plane 与
// 健康检查两类 Baseline 的推导依据，漏掉的后果是少一条放行规则，
// 表现为生产阻断而不是注册时的报错。
//
// 比对整个结构体而不是选几个字段：新增一个字段却忘记映射时，
// 表现必须是这个测试失败，而不是没有人注意到。
func TestCreateClusterCarriesEveryFieldIntoTheDomainObject(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "rt-1", "displayName": "Round Trip", "podCidr": "10.20.0.0/14",
		"nodeCidr": "10.140.0.0/20", "ccnpPresent": true,
		// state 是服务端决定的，请求体里给一个相反的值，
		// 期望值里写 REGISTERED —— 这条断言顺带锁住「忽略调用方的 state」。
		"state": "READY",
		"apiServers": []map[string]any{
			{"host": "10.9.0.2", "cidr": "10.9.0.0/28", "port": 443},
			{"host": "10.9.0.3", "cidr": "10.9.0.0/28", "port": 443},
		},
		"healthCheckSources": []string{"35.191.0.0/16", "130.211.0.0/22"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	want := registry.Cluster{
		ID: "rt-1", DisplayName: "Round Trip",
		PodCIDR: "10.20.0.0/14", NodeCIDR: "10.140.0.0/20",
		CCNPPresent: true, State: registry.StateRegistered,
		APIServers: []registry.APIServer{
			{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
			{Host: "10.9.0.3", CIDR: "10.9.0.0/28", Port: 443},
		},
		HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
	}
	got, ok := reg.clusters["rt-1"]
	if !ok {
		t.Fatal("cluster was not stored")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored cluster =\n%+v\nwant\n%+v", got, want)
	}
}

// --- cluster agent（design doc 2026-08-18 §3）---
//
// 这个替身刻意实现出真实的定位语义，而不是"存下就算数"：跨集群吊销必须
// 落空、已吊销的记录必须仍然查得到。handler 与中间件的测试正是靠这两条
// 区分「被吊销」与「不存在」，替身把它们抹平，那些断言就全成了摆设。

func (m *memRegistry) IssueClusterAgent(
	_ context.Context, _ registry.Actor, a registry.ClusterAgent,
) error {
	if err := m.writeErr(); err != nil {
		return err
	}
	if err := registry.ValidateClusterAgent(a); err != nil {
		return err
	}
	if _, exists := m.agents[a.AgentID]; exists {
		return registry.NewInvalidError("agent 已存在")
	}
	a.State = registry.AgentActive
	a.CreatedAt = time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	m.agents[a.AgentID] = a
	return nil
}

func (m *memRegistry) RevokeClusterAgent(
	_ context.Context, _ registry.Actor, clusterID, agentID string,
) error {
	if err := m.writeErr(); err != nil {
		return err
	}
	a, ok := m.agents[agentID]
	// 三个条件缺一不可，与真实实现的 WHERE 一一对应：集群不符、已经吊销
	// 过、根本不存在，都答 ErrNotFound。
	if !ok || a.ClusterID != clusterID || a.State != registry.AgentActive {
		return registry.ErrNotFound
	}
	a.State = registry.AgentRevoked
	a.RevokedAt = time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC)
	m.agents[agentID] = a
	return nil
}

func (m *memRegistry) ClusterAgents(_ context.Context, clusterID string) ([]registry.ClusterAgent, error) {
	if m.failWith != nil {
		return nil, m.failWith
	}
	var out []registry.ClusterAgent
	for _, a := range m.agents {
		if a.ClusterID == clusterID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

func (m *memRegistry) ClusterAgentByID(_ context.Context, agentID string) (registry.ClusterAgent, bool, error) {
	if m.failWith != nil {
		return registry.ClusterAgent{}, false, m.failWith
	}
	a, ok := m.agents[agentID]
	if ok {
		// 与 mysqlregistry 一样，这一位是**算出来的**，不是存下来的：
		// 存一份就有可能漂成"集群没了但这一位还说在"，而那正是这条检查
		// 要挡住的情形。
		_, alive := m.clusters[a.ClusterID]
		a.ClusterRetired = !alive
	}
	return a, ok, nil
}

func (m *memRegistry) TouchClusterAgent(_ context.Context, agentID string, at time.Time) error {
	a, ok := m.agents[agentID]
	if !ok {
		return nil
	}
	a.LastSeenAt = at
	m.agents[agentID] = a
	return nil
}

// metrics 抓取端必须能从请求体走到领域对象。
//
// 这条与 kubeconfigRef 那条同形（clusterPayload 的注释）：集群没有别的路由
// 能说出自己的抓取端，漏掉它的后果是请求返回成功、登记从未写下，而
// METRICS_SCRAPE 继续报缺失 —— 没有任何东西指向登记。
func TestClusterPayloadCarriesMetricsScrapers(t *testing.T) {
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "new-cluster", "displayName": "New", "podCidr": "10.4.0.0/14",
		"nodeCidr": "10.128.0.0/20",
		"metricsScrapers": []map[string]any{{
			"namespace": "monitoring",
			"labels":    map[string]string{"app.kubernetes.io/name": "prometheus"},
		}},
	})
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}
	got := reg.lastCluster
	if len(got.MetricsScrapers) != 1 {
		t.Fatalf("MetricsScrapers = %+v, want one", got.MetricsScrapers)
	}
	if got.MetricsScrapers[0].Namespace != "monitoring" {
		t.Errorf("namespace = %q, want monitoring", got.MetricsScrapers[0].Namespace)
	}
	if got.MetricsScrapers[0].Labels["app.kubernetes.io/name"] != "prometheus" {
		t.Errorf("labels = %v", got.MetricsScrapers[0].Labels)
	}
}

func TestClusterPayloadRejectsAScraperWithoutLabels(t *testing.T) {
	// 空 podSelector 会放行那个命名空间里的每一个 Pod，而请求照样"成功"。
	reg := newMemRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters", map[string]any{
		"id": "new-cluster", "displayName": "New", "podCidr": "10.4.0.0/14",
		"nodeCidr":        "10.128.0.0/20",
		"metricsScrapers": []map[string]any{{"namespace": "monitoring"}},
	})
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d: %s", got, response.CodeInvalidParam, rec.Body.String())
	}
}
