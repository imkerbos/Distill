package registry

import (
	"net/netip"
	"strings"
	"time"
)

// SecretsBackend 是凭据解析后端的封闭枚举。
//
// 与 internal/config 里同名类型的旧形态不同：这里的取值是操作者在设置页
// 做出的一个明确选择，不是从「填了哪几个字段」反推出来的。反推形态下，
// project 与 dir 同时留在库里但只有一个在生效，操作者读不出哪个在生效；
// 显式选择把这件事挪到 ValidatePlatformSetting，让「选了什么」与
// 「填了什么」必须互相印证，见该函数的注释。
type SecretsBackend string

const (
	// SecretsBackendNone 表示不解析凭据，因而也不做 Git 绑定校验。
	SecretsBackendNone SecretsBackend = "NONE"
	// SecretsBackendDir 是本地开发用的目录解析器，一个 ref 一个文件。
	SecretsBackendDir SecretsBackend = "DIR"
	// SecretsBackendSecretManager 是生产用的 Secret Manager 解析器。
	SecretsBackendSecretManager SecretsBackend = "SECRET_MANAGER"
	// SecretsBackendDB 把凭据以密文存进平台自己的数据库。
	//
	// 加密密钥来自启动配置、不经过数据库：拿到一份完整转储得到的是一堆
	// 解不开的字节（design doc 2026-09-01 §2）。选它的理由是可用性——
	// 另外两个后端都要求在平台之外先把私钥放好，而那不是"在界面上配一个仓库"。
	SecretsBackendDB SecretsBackend = "DB"
)

// Valid 判断该后端是否已登记。
//
// 显式 switch 而非 map/slice 查表：新增一个常量却忘了在这里同步，
// switch 让这处遗漏在 review 时是看得见的一行；换成表驱动，
// 遗漏就是一次无声的编译通过（同 VerifyResult.Valid 的取舍）。
func (b SecretsBackend) Valid() bool {
	switch b {
	case SecretsBackendNone, SecretsBackendDir, SecretsBackendSecretManager, SecretsBackendDB:
		return true
	default:
		return false
	}
}

// PlatformSetting 是落库的运行期配置：会话 TTL、HTTP 超时、凭据后端、
// Git 校验超时与 SSH host keys（design doc 2026-08-13 §1、§2）。
//
// 配置文件只留启动必需项，其余全部是这个类型的字段，按需读取，
// 不在启动时缓存快照 —— 快照曾经造成过一次事故（轮 2，下线的集群
// 靠着启动快照继续被服务，直到进程重启才消失）。
type PlatformSetting struct {
	// SessionTTL 是会话有效期。
	SessionTTL time.Duration `json:"sessionTtl"`
	// HTTPReadTimeout 限制读取请求的时长。
	HTTPReadTimeout time.Duration `json:"httpReadTimeout"`
	// HTTPWriteTimeout 限制写出响应的时长。
	HTTPWriteTimeout time.Duration `json:"httpWriteTimeout"`
	// HTTPShutdownTimeout 是优雅退出的等待上限。
	HTTPShutdownTimeout time.Duration `json:"httpShutdownTimeout"`
	// SecretsBackend 是操作者选中的凭据解析后端。
	SecretsBackend SecretsBackend `json:"secretsBackend"`
	// SecretsProject 是 Secret Manager 所在的项目，仅 SECRET_MANAGER 后端使用。
	SecretsProject string `json:"secretsProject"`
	// SecretsPrefix 是短名拼进资源路径前的固定前缀，仅 SECRET_MANAGER 后端使用。
	//
	// 空前缀仍能拼出合法路径，但围栏就只剩项目一层，任何短名都能指向
	// 项目里的任意 secret —— 因此 SECRET_MANAGER 后端要求它必须非空。
	SecretsPrefix string `json:"secretsPrefix"`
	// SecretsDir 是本地开发用的凭据目录，仅 DIR 后端使用。该目录必须 gitignored。
	SecretsDir string `json:"secretsDir"`
	// GitVerifyTimeout 是 Git 绑定只读校验的单次出站超时。
	GitVerifyTimeout time.Duration `json:"gitVerifyTimeout"`
	// GitVerifyHostKeys 是 known_hosts 格式的已知主机公钥，Git 校验时的信任锚。
	//
	// 为空时 gitverify.New 仍然拒绝构造（既有行为，不在这里重复校验）——
	// 清空这个字段不会让平台退化成「不校验」，只会让它起不来。
	GitVerifyHostKeys string `json:"gitVerifyHostKeys"`
	// GitAllowedDestinations 是允许出站到达的**私有**网段，CIDR，一行一条。
	//
	// 空表示只放行公网——也就是引入这一项之前的行为。**没有"默认放行内网"
	// 这一档**：一次升级不该悄悄改变任何现有部署的出站面
	// （design doc 2026-09-01 §3.3）。
	//
	// 它只影响私有地址那一档。回环、链路本地、组播与云元数据网段在这份清单
	// **之上**，改不了：前者一旦可放行，平台就成了本机服务的探测器；
	// 后者一旦可放行，平台就成了一把偷取实例凭据的钥匙（§3.1）。
	//
	// 加上它意味着"平台不能被诱导去连任意内网地址"这条保证，从结构上不可能
	// 降级成取决于操作者填了什么。这是真实的削弱，换来的是平台能在内网
	// GitOps 环境里工作——而那本来就是它的目标场景（§2）。
	GitAllowedDestinations string `json:"gitAllowedDestinations"`
}

// ValidatePlatformSetting 校验一份设置。
//
// 两类风险各对应一段检查：
//
//   - 后端与其字段必须互相印证（见 validateSecretsBackendFields）。
//   - 所有超时字段必须为正：SessionTTL 为零会让会话立即过期，
//     HTTP 超时为零等于关闭超时保护，GitVerifyTimeout 非正会被
//     gitverify.New 拒绝 —— 与其等下一次校验才暴露，不如保存时就拦下,
//     让操作者在犯错的那一刻就知道。
func ValidatePlatformSetting(s PlatformSetting) error {
	if s.SessionTTL <= 0 {
		return invalid("sessionTtl 必须为正")
	}
	if s.HTTPReadTimeout <= 0 {
		return invalid("httpReadTimeout 必须为正")
	}
	if s.HTTPWriteTimeout <= 0 {
		return invalid("httpWriteTimeout 必须为正")
	}
	if s.HTTPShutdownTimeout <= 0 {
		return invalid("httpShutdownTimeout 必须为正")
	}
	if s.GitVerifyTimeout <= 0 {
		return invalid("gitVerifyTimeout 必须为正")
	}
	if !s.SecretsBackend.Valid() {
		return invalidf("secretsBackend %q 不在已登记的取值范围内", s.SecretsBackend)
	}
	// 保存时就把非法网段挡下来。留到拨号那一刻才发现，症状是
	// REPO_UNREACHABLE —— 一句说不出真正原因的话。
	if _, err := ParseAllowedDestinations(s.GitAllowedDestinations); err != nil {
		return err
	}
	return validateSecretsBackendFields(s)
}

// ValidateSettingUpdate 校验一次设置**变更**。
//
// 与 ValidatePlatformSetting 分开：那个函数看的是一份设置自身是否成立，
// 而这里的约束是关于**从哪一份变到哪一份**的，单看提交的那一份判不出来。
//
// 目前只有一条：库里已经装着 SSH 信任锚时，不接受一次把它清空的提交。
//
// 这条判定必须长在服务端。设置页确实拦过它，但那是一份镜像、不是判定
// （规范 §34：前端不是安全边界）—— 任何一次直接调 API 的 PUT、任何将来
// 新增的页面，都能一句话抹掉信任锚。
//
// 清空的后果不是「退化成不校验」：host key 为空时 gitverify.New 直接拒绝
// 构造，失败方向朝关。坏在它是一次**无声的能力丧失** —— 此后每一次 Git
// 校验都出不了结论，而没有任何人做过这个决定；原文又永远读不回来
// （读取端点只回指纹），于是这次丢失不可逆。
//
// 不并进 ValidatePlatformSetting：空 host key 本身是一个合法状态（全新
// 部署尚未配置时就是它），而 settings.Provider 会对**读上来的**每一份设置
// 跑一次 ValidatePlatformSetting —— 把空值判成非法，平台会连启动都做不到。
func ValidateSettingUpdate(current, next PlatformSetting) error {
	if current.GitVerifyHostKeys != "" && next.GitVerifyHostKeys == "" {
		return invalid("gitVerifyHostKeys 不能被清空：它是平台连接策略仓库时的信任锚，" +
			"清空之后所有 Git 校验都无法完成，而原文无法再从平台读回。" +
			"不修改信任锚时不要带这个字段")
	}
	return nil
}

// validateSecretsBackendFields 检查凭据字段是否与选中的后端互相印证。
//
// 三条约束都指向同一件事：设置页上的后端选择必须是唯一真相，字段不能
// 各说各话。最坏的落法是选了 DIR、project 却还留在库里 —— 进程正常
// 起来、校验正常出结论，只是身份来源读成了本地目录，没有任何症状会
// 暴露它（brief 第一条风险）。
//
//   - NONE 要求三个字段全空：选了「不解析凭据」却留着字段值，
//     等于操作者以为自己配置的东西其实从未生效。
//   - DIR 要求 dir 非空、project 与 prefix 全空。
//   - SECRET_MANAGER 要求 project 与 prefix 都非空、dir 为空。
//     空 prefix 仍能拼出合法路径，但围栏就只剩项目一层。
func validateSecretsBackendFields(s PlatformSetting) error {
	switch s.SecretsBackend {
	case SecretsBackendNone:
		if s.SecretsProject != "" || s.SecretsPrefix != "" || s.SecretsDir != "" {
			return invalid("secretsBackend 为 NONE 时 secretsProject/secretsPrefix/secretsDir 必须全部为空")
		}
	case SecretsBackendDir:
		if s.SecretsDir == "" {
			return invalid("secretsBackend 为 DIR 时 secretsDir 不能为空")
		}
		if s.SecretsProject != "" || s.SecretsPrefix != "" {
			return invalid("secretsBackend 为 DIR 时 secretsProject/secretsPrefix 必须为空，" +
				"否则操作者会以为两套后端都在生效")
		}
	case SecretsBackendSecretManager:
		if s.SecretsProject == "" || s.SecretsPrefix == "" {
			return invalid("secretsBackend 为 SECRET_MANAGER 时 secretsProject 与 secretsPrefix 都必须非空")
		}
		if s.SecretsDir != "" {
			return invalid("secretsBackend 为 SECRET_MANAGER 时 secretsDir 必须为空")
		}
	case SecretsBackendDB:
		// DB 后端的凭据存在平台自己的库里，另外两套后端的定位字段都用不上。
		// 留着非空值会让操作者以为两套后端都在生效——与 DIR 那条同一个理由。
		//
		// **加密密钥不在这里校验**：它来自启动配置、不落库，因此它缺不缺
		// 是装配层的事（newSecretResolver 那里构造失败）。写进设置里校验
		// 等于把密钥也搬进数据库的势力范围。
		if s.SecretsProject != "" || s.SecretsPrefix != "" || s.SecretsDir != "" {
			return invalid("secretsBackend 为 DB 时 secretsProject/secretsPrefix/secretsDir 必须全部为空")
		}
	}
	return nil
}

// privateRanges 是允许清单条目必须落在其中的地址空间。
//
// RFC1918 与 IPv6 ULA。清单的用途是放行内网，不是放行互联网——把判据写成
// "整段落在私有空间内"，同时挡住了三件事，而且是同一条规则挡的：
// 0.0.0.0/0 这种等于关掉检查的填法、公网网段、以及云元数据网段
// （169.254.169.254 是链路本地，168.63.129.16 与 100.64.0.0/10 都不是私有
// 地址，一律落不进来）。
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// ParseAllowedDestinations 把设置里的多行文本解析成网段清单。
//
// 空行与 # 注释跳过：这一栏会被人手工维护，而"为什么放行这一段"正是
// 该写在旁边的东西。
func ParseAllowedDestinations(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for i, line := range strings.Split(raw, "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		p, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, invalidf("第 %d 行 %q 不是合法的 CIDR 网段", i+1, text)
		}
		// Masked 之后再比：10.170.1.11/16 这种写法里主机位是有值的，
		// 不规范化会让包含判定落在另一条分支上。
		p = p.Masked()
		if !withinPrivateSpace(p) {
			return nil, invalidf(
				"第 %d 行 %q 不在私有地址空间内。这一栏只能放行内网网段"+
					"（10/8、172.16/12、192.168/16、fc00::/7 之内）——"+
					"填公网段、0.0.0.0/0 或云元数据网段等于关掉这道检查，"+
					"而它挡的是「平台被诱导去连任意地址」", i+1, text)
		}
		out = append(out, p)
	}
	return out, nil
}

// withinPrivateSpace 报告一个网段是否**整段**落在私有地址空间内。
//
// 判包含关系而不是"看起来像内网"：只判起始地址的话，10.255.255.0/16
// 这类跨界写法会被放过一半。前缀长度不短于私有段本身，才谈得上整段在内。
func withinPrivateSpace(p netip.Prefix) bool {
	for _, r := range privateRanges {
		if r.Contains(p.Addr()) && p.Bits() >= r.Bits() {
			return true
		}
	}
	return false
}
