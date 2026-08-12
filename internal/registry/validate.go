package registry

import (
	"errors"
	"fmt"
	"net/netip"
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
	return validateGit(c.Git)
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

// validateGit 要求绑定要么完整要么不填。
//
// 填一半的绑定会在轮 3 变成一次指向不存在路径的写入尝试，
// 而报出来的错误只会说「路径不存在」，与真正的配置错误无法区分。
func validateGit(g *GitBinding) error {
	if g == nil {
		return nil
	}
	if g.RepoURL == "" || g.Branch == "" || g.PolicyPath == "" {
		return invalid("Git 绑定的 repoUrl、branch、policyPath 三项必须同时填写")
	}
	return nil
}

// checkCIDR 校验一个网段，错误信息带上字段名。
//
// 带字段名而非只说「非法网段」：一个集群有四类网段，
// 只报「非法」会让操作者逐个试。
func checkCIDR(field, value string) error {
	if value == "" {
		return invalidf("%s 不能为空", field)
	}
	if _, err := netip.ParsePrefix(value); err != nil {
		return invalidf("%s %q 不是合法网段", field, value)
	}
	return nil
}
