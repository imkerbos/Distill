package registry

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/secrets"
)

// ErrInvalid 表示输入不合法。
var ErrInvalid = errors.New("invalid registry input")

// InvalidError 是本平台自己构造的校验错误。
//
// Detail 一定是我们写的文案 —— 回传通道只读它，永不读 Error()。
// 分成两个东西而不是复用 Error()：Error() 会因为 %w 包装把第三方库的
// 文本一并带上（YAML/JSON 解析失败时尤其如此，那种错误文本里常常
// 直接带着 Go 结构体类型名），而「这是不是校验错误」与「哪一段文字
// 可以回传给调用方」是两个不同的问题，用同一个 errors.Is(err, ErrInvalid)
// 兼顾会让下一条包了第三方错误的校验路径悄悄泄露，且 review 看不出来。
// 要泄露第三方文本，必须有人主动把它写进 Detail —— 那在 review 里是
// 看得见的一行，不是一次隐式的字符串拼接。
type InvalidError struct {
	// Detail 是可以回传给调用方的文案，只能是本平台自己写的文字。
	Detail string
	// Cause 是底层原因（可能来自第三方库），只进日志，永不回传。
	Cause error
}

// Error 实现 error。Cause 存在时一并纳入，供日志与 %v 使用 ——
// 这条路径不面向调用方，只面向排障的人。
func (e *InvalidError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", ErrInvalid, e.Detail, e.Cause)
	}
	return fmt.Sprintf("%s: %s", ErrInvalid, e.Detail)
}

// Unwrap 暴露 Cause，让 errors.As 仍能取到底层的第三方错误类型 ——
// 这是给日志与调试用的，不是给回传通道用的。
func (e *InvalidError) Unwrap() error {
	return e.Cause
}

// Is 让 errors.Is(err, ErrInvalid) 对 *InvalidError 恒真。
//
// 不依赖 Unwrap 链去够到 ErrInvalid：Cause 是第三方错误，链上根本
// 没有 ErrInvalid 这个哨兵，必须显式声明「我就是一条校验错误」。
func (e *InvalidError) Is(target error) bool {
	return target == ErrInvalid
}

// invalid 构造一条不带底层原因的校验错误：文案完全是我们自己写的。
func invalid(detail string) *InvalidError {
	return &InvalidError{Detail: detail}
}

// invalidf 是 invalid 的 Sprintf 版本。
func invalidf(format string, args ...any) *InvalidError {
	return &InvalidError{Detail: fmt.Sprintf(format, args...)}
}

// wrapInvalid 包一条底层原因：detail 是我们自己写的、可以回传给调用方的
// 文案，cause 是触发校验失败的原始错误（常常来自第三方库），只进日志。
func wrapInvalid(detail string, cause error) *InvalidError {
	return &InvalidError{Detail: detail, Cause: cause}
}

// NewInvalidError 构造一条校验错误，供 internal/registry 之外、但同样在
// 校验用户输入的调用方使用（目前是 internal/mysqlregistry 对导入
// role/source 的校验）。
//
// 校验错误只有这一条构造路径：不让每个包各自拼一个
// fmt.Errorf("%w: ...", registry.ErrInvalid, ...)，否则「哪段文字可以
// 回传」这条规则只在 internal/registry 内部成立，出了这个包就失效。
func NewInvalidError(detail string) error {
	return invalid(detail)
}

// ValidateCluster 校验一个集群注册请求。
//
// 网段在入库前校验而非推导时才发现：一个写错的网段会让 Baseline 生成
// 一条永远匹配不上的规则，而那条规则外观完全正常 —— 等到上线后监控
// 静默中断才暴露，那时恰好看不到数据。
func ValidateCluster(c Cluster) error {
	if c.ID == "" {
		return invalid("集群 ID 不能为空")
	}
	if c.DisplayName == "" {
		return invalid("显示名不能为空")
	}
	if !c.State.Valid() {
		return invalidf("接入状态 %q 不在已登记的取值范围内", c.State)
	}
	// 纳入系统命名空间必须带理由：这是一次会改变爆炸半径的决定
	// （一份下发到 kube-dns 的 default-deny 会让全集群失去 DNS），
	// 而没有理由的决定在事后复盘时与"手滑填上去的"分不开。
	if len(c.ManagedSystemNamespaces) > 0 &&
		strings.TrimSpace(c.ManagedSystemNamespacesReason) == "" {
		return invalid("把系统命名空间交给平台管理必须写明理由：" +
			"平台会为其中的每个 workload 生成 default-deny 候选策略，" +
			"而一份下发到 kube-dns 的 default-deny 会让整个集群失去 DNS")
	}
	for _, ns := range c.ManagedSystemNamespaces {
		if !policygen.IsSystemNamespace(ns) {
			return invalidf("%q 不是 Kubernetes 内置系统命名空间，"+
				"不需要在这里声明 —— 平台本来就会为它生成候选策略", ns)
		}
	}
	if err := checkCIDR("podCIDR", c.PodCIDR); err != nil {
		return err
	}
	if err := checkCIDR("nodeCIDR", c.NodeCIDR); err != nil {
		return err
	}
	for i, a := range c.APIServers {
		if a.Host == "" {
			return invalidf("apiserver[%d] host 不能为空", i)
		}
		if a.Port <= 0 || a.Port > 65535 {
			return invalidf("apiserver[%d] port %d 超出 1-65535 范围", i, a.Port)
		}
		if err := checkCIDR(fmt.Sprintf("apiserver[%d] cidr", i), a.CIDR); err != nil {
			return err
		}
	}
	for i, s := range c.HealthCheckSources {
		if err := checkCIDR(fmt.Sprintf("healthCheck[%d] cidr", i), s); err != nil {
			return err
		}
	}
	// kubeconfigRef 可以为空：集群可以先登记下来，凭据稍后再配 —— 与
	// GitRepo.CredentialRef 同一条规则，理由也同一条。非空时复用
	// secrets.ValidateRef，不另写一份字符集校验：两份安全相关的字符类
	// 定义迟早会走样，而走样的那一份放行的是一个能指向凭据目录之外的
	// 引用，丢了不会有任何症状，直到有人用它读到别的东西。
	for i, a := range c.NodeAgents {
		if err := ValidateNodeAgent(a); err != nil {
			return wrapInvalid(fmt.Sprintf("nodeAgents[%d] 不合法", i), err)
		}
	}
	// 登记了 agent 又声明"没有需要放行的 agent"：一次自相矛盾的登记。
	// 收下它之后，缺失清单该不该有这一类没有答案（design doc §8）。
	if len(c.NodeAgents) > 0 && c.NoNodeAgentsReason != "" {
		return invalid("已经登记了节点 agent，就不能同时声明这个集群没有需要放行的节点 agent")
	}
	// 业务周期与它的理由必须一起出现（design doc 2026-08-25 §5）。
	//
	// 只填时长不填理由：事后没有人答得出「当初凭什么说一周够」。
	// 只填理由不填时长：门禁拿不到可比的数，那条理由等于没写。
	if (c.BusinessCycle > 0) != (c.BusinessCycleReason != "") {
		return invalid("业务周期与它的理由必须同时给出：" +
			"只给时长，事后答不出当初凭什么这么定；只给理由，门禁拿不到可比的数")
	}
	if c.BusinessCycle < 0 {
		return invalid("业务周期不能是负数")
	}
	for i, sc := range c.MetricsScrapers {
		if err := ValidateMetricsScraper(sc); err != nil {
			return wrapInvalid(fmt.Sprintf("metricsScrapers[%d] 不合法", i), err)
		}
	}
	if c.KubeconfigRef != "" {
		if err := secrets.ValidateRef(c.KubeconfigRef); err != nil {
			return wrapInvalid(fmt.Sprintf("kubeconfigRef %q 不是合法的凭据引用", c.KubeconfigRef), err)
		}
	}
	return nil
}

// gitCommitSHALength 是一个完整 Git commit SHA-1 的十六进制长度。
//
// 不接受缩写：漂移检测要拿它去仓库里定位一次提交，而缩写在仓库变大后
// 可能歧义 —— 一个「核对过」的标记指向两个提交，比没有标记更糟。
const gitCommitSHALength = 40

// ValidatePolicyImport 校验一条导入记录。
//
// 除枚举取值外，这里管的是「来源」与「commit」必须互相印证（spec §4）：
// GIT 却没有 commit，等于把一次未经核对的粘贴标成「与仓库一致的现状」，
// 而轮 3 的漂移检测正是拿这个 commit 做基准；反过来，非 GIT 来源却带着
// commit，是一句没有东西支撑的溯源声明 —— 两者都会让界面上的「已用 Git
// 核对」失去含义，而它们都不会报错。
func ValidatePolicyImport(p PolicyImport) error {
	if !p.Role.Valid() {
		return invalidf("导入角色 %q 不在已登记的取值范围内", p.Role)
	}
	if !p.Source.Valid() {
		return invalidf("导入来源 %q 不在已登记的取值范围内", p.Source)
	}
	if p.Source == SourceGit && p.GitCommitSHA == "" {
		return invalid("来源为 GIT 时必须提供 gitCommitSha —— " +
			"没有 commit 就无法证明这份内容与仓库一致")
	}
	if p.Source != SourceGit && p.GitCommitSHA != "" {
		return invalidf("来源为 %q 却带了 gitCommitSha：commit 只有在来源为 GIT 时才有依据", p.Source)
	}
	if p.GitCommitSHA != "" && !isHexSHA(p.GitCommitSHA) {
		return invalidf("gitCommitSha %q 不是 %d 位十六进制的完整 commit SHA",
			p.GitCommitSHA, gitCommitSHALength)
	}
	return nil
}

// isHexSHA 报告 s 是否为完整长度的十六进制 commit SHA。
func isHexSHA(s string) bool {
	if len(s) != gitCommitSHALength {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// ValidateGitRepo 校验一个策略仓库。
//
// repoUrl / branch / credentialRef 的规则随字段一起从绑定搬到这里
// （design doc 2026-08-13 §3.1、§3.2）。搬家不改判定：一个搬家途中丢掉的
// 校验规则，症状与从来没写过这条规则完全一样，而它曾经存在这件事只留在
// git 历史里。
func ValidateGitRepo(r GitRepo) error {
	if r.ID == "" {
		return invalid("仓库 ID 不能为空")
	}
	if r.URL == "" || r.Branch == "" {
		return invalid("仓库的 repoUrl 与 branch 必须同时填写")
	}
	if !isSSHRepoURL(r.URL) {
		return invalidf("repoUrl %q 不是 SSH 形态：平台按 per-repo deploy key 访问策略仓库，"+
			"只支持 ssh://[user@]host[:port]/path 与 git@host:path 两种写法。"+
			"填 https:// 的后果不是「校验不通过」而是一句假的结论 —— 认证方法与传输对不上，"+
			"一次拨号都不会发生，失败却会被报成「仓库不可达」", r.URL)
	}
	// credentialRef 可以为空：仓库可以先登记下来，凭据稍后再配。非空时
	// 复用 secrets.ValidateRef，不另写一份字符集校验 —— 两份安全相关的
	// 字符类定义迟早会走样。
	if r.CredentialRef != "" {
		if err := secrets.ValidateRef(r.CredentialRef); err != nil {
			return wrapInvalid(fmt.Sprintf("credentialRef %q 不是合法的凭据引用", r.CredentialRef), err)
		}
	}
	// 空值按 NOT_VERIFIED 处理：仓库刚登记、还没跑过校验时，
	// VerifyResult 字段就是零值，这不该被当成非法输入。
	vr := r.VerifyResult
	if vr == "" {
		vr = RepoVerifyNotVerified
	}
	if !vr.Valid() {
		return invalidf("verifyResult %q 不在已登记的取值范围内", r.VerifyResult)
	}
	return nil
}

// ValidateGitBinding 校验一个 Git 绑定，要么完整要么不填。
//
// 独立成导出函数、ValidateCluster 不再调用它（design doc 2026-08-13 §6）：
// 绑定已经从集群写模型里摘出来，成了有自己生命周期的资源，它的合法性
// 不该与集群其余字段的合法性互相牵连。一个 podCidr 写错的集群，其绑定
// 仍然应当能被独立校验、独立写下结论；反过来，一个非法的绑定也不该
// 挡住集群其余字段的校验或让重新校验的结论无法落库——这正是拆分之前
// 已经在代码里付出的代价。
//
// 参数是值而非指针：走到这里的调用方（BindGitRepo 的请求体、
// handleVerifyGitBinding 重新校验时读到的库内记录）手上总已经有一个
// 具体的绑定要校验；「绑定压根不存在」现在由 *GitBinding == nil 或
// UnbindGitRepo 表达，不是这个函数要处理的一种合法输入。
//
// 只管绑定自己的两个字段：repoId 指向的仓库是否存在、是否合法，是
// 存储层的外键与 ValidateGitRepo 的事，不在这里重复判断 —— 拿一份可能
// 已经过期的仓库副本在这里再判一次，得到的结论会与仓库页上的不一致。
//
// 填一半的绑定会在轮 3 变成一次指向不存在路径的写入尝试，
// 而报出来的错误只会说「路径不存在」，与真正的配置错误无法区分。
func ValidateGitBinding(b GitBinding) error {
	if b.RepoID == "" || b.PolicyPath == "" {
		return invalid("Git 绑定的 repoId 与 policyPath 必须同时填写")
	}
	// 空值按 NOT_VERIFIED 处理：绑定刚创建、还没跑过校验时，
	// VerifyResult 字段就是零值，这不该被当成非法输入。
	vr := b.VerifyResult
	if vr == "" {
		vr = BindingVerifyNotVerified
	}
	if !vr.Valid() {
		return invalidf("verifyResult %q 不在已登记的取值范围内", b.VerifyResult)
	}
	return nil
}

// schemeRepoURL 匹配 ssh://[user@]host[:port]/path。
//
// (?i) 是因为 scheme 大小写不敏感：一个 SSH:// 的地址是对的地址，不该
// 因为大写被指成配置错误。结尾要求有一段非空路径 —— 只有 "ssh://" 的
// 串通过校验也没有意义，它到不了任何仓库。
var schemeRepoURL = regexp.MustCompile(`(?i)^ssh://([A-Za-z0-9._-]+@)?[A-Za-z0-9._-]+(:[0-9]+)?/\S+$`)

// scpLikeRepoURL 匹配 scp 式的 SSH 地址：user@host:path。
//
// 要求 user@ 与 host 都在，且冒号之后不再出现冒号 —— 后半句是为了让
// scheme 形态（https://、file://、git://）落不进这条分支，而不是靠
// 另写一份 scheme 黑名单：黑名单漏掉一个就是放行一个。
var scpLikeRepoURL = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:[^:\s]+$`)

// isSSHRepoURL 报告 repoUrl 是否是平台真正会去连的那种地址。
//
// 白名单而非黑名单：校验路径给 clone 挂的是 SSH 认证方法，而 go-git 按
// scheme 选传输 —— 除 SSH 之外的任何传输都会在拨号之前就拒掉这个认证
// 方法，几微秒内失败，然后这个失败被归进 REPO_UNREACHABLE。那是一句
// 关于网络的结论，而网络从未被碰过：操作者会去查一道不存在的防火墙。
// 拒绝的时机必须是保存，不是校验（spec §2.2）。
//
// 顺带关掉的一件事：repoUrl 不加限制时，校验端点就是一个由调用方指定
// 目标的出站探针（file:///…、http://169.254.169.254/… 都能填），回答虽然
// 只有七个枚举值，但目标是任意的。白名单让这个形状不复存在。
func isSSHRepoURL(u string) bool {
	return schemeRepoURL.MatchString(u) || scpLikeRepoURL.MatchString(u)
}

// checkCIDR 校验一个网段，错误信息带上字段名。
//
// 带字段名而非只说「非法网段」：一个集群有四类网段，
// 只报「非法」会让操作者逐个试。
func checkCIDR(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalidf("%s 不能为空", field)
	}
	// 支持逗号分隔的多段：双栈集群的每个 Pod 有两个地址，一个 IPv4、
	// 一个 IPv6。只登记得下一个的话，走另一个协议族的连接会落进 EXTERNAL，
	// 平台据此生成 ipBlock 规则而不是 selector 规则，放行面宽得多
	// （fleet.parsePrefixes 是同一套解析）。
	//
	// **这里的判据必须与 fleet.parsePrefixes 一致**：一条这里放行、那边
	// 却解析不出来的登记，会安静地落进「网段登记坏掉」而不是在提交时被拒。
	for _, part := range strings.Split(value, ",") {
		seg := strings.TrimSpace(part)
		if seg == "" {
			return invalidf("%s %q 里有空段（多半是多余的逗号）", field, value)
		}
		if _, err := netip.ParsePrefix(seg); err != nil {
			return invalidf("%s 里的 %q 不是合法网段", field, seg)
		}
	}
	return nil
}

// ValidateClusterAgent 校验一条 agent 记录在入库前是否自洽。
//
// **错误文案里不带哈希内容**：这条错误会走到 API 边界，而哈希是离线
// 爆破的输入（规范 §19、§20、§22）。说得出「长度不对」就够定位了。
func ValidateClusterAgent(a ClusterAgent) error {
	if a.ClusterID == "" {
		return invalid("agent 必须绑定一个集群")
	}
	if !isAgentID(a.AgentID) {
		return invalidf("agent ID %q 不是一个合法的 agent 标识", a.AgentID)
	}
	if len(a.TokenHash) != TokenHashLen {
		// 长度不符说明存进来的不是 SHA-256。比对按整段比，长度不符恒不
		// 相等 —— 症状是「这把 token 怎么都认不过」，而成因在写入侧，
		// 从症状反推不到。必须在入库前拒绝。
		return invalidf("agent token 摘要长度为 %d，应为 %d", len(a.TokenHash), TokenHashLen)
	}
	if !a.State.Valid() {
		return invalidf("agent 状态 %q 不在已登记的取值范围内", a.State)
	}
	return nil
}

// isAgentID 判断一个字符串是否是平台生成的 agent 公开段：定长小写 hex。
//
// 不复用 secrets.ValidateRef：那一条管的是「能不能拼进凭据后端的资源
// 路径」，字符集比这里宽。两个约束的成因不同，合用一份会让其中一边
// 将来被放宽时另一边跟着松掉，而松掉不会有任何症状。
func isAgentID(s string) bool {
	if len(s) != AgentIDLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
