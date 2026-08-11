// Package risk 定义高风险端口的封闭清单与风险分类。
//
// 单独成包而非留在 store：候选策略生成需要同一份判定，而 store 要消费
// 生成结果，留在 store 会成环。这份清单是判定依据，不是某个消费方的私产。
package risk

import "sort"

// RiskCategory 是高风险端口的风险来源。封闭枚举。
//
//nolint:revive // 命名与 store 中既有别名及消费方保持一致，重命名会波及整条引用链。
type RiskCategory string

const (
	// RiskAdminPlaintext 是明文管理端口：拿到即等于拿到主机。
	RiskAdminPlaintext RiskCategory = "ADMIN_PLAINTEXT"
	// RiskDatabase 是数据库直连端口：绕过应用层的一切授权与审计。
	RiskDatabase RiskCategory = "DATABASE"
	// RiskFileShare 是文件共享端口：历史漏洞密集且常被横向移动利用。
	RiskFileShare RiskCategory = "FILE_SHARE"
)

// RiskPosition 是风险连接所处的位置。
//
// 必须与 RiskCategory 分开表达，不得合成一个分数：同一个 22 端口，
// 出公网、跨 namespace、同 namespace 内的处置方式完全不同，
// 合成之后使用者无法判断该找谁。
//
//nolint:revive // 命名与 store 中既有别名及消费方保持一致，重命名会波及整条引用链。
type RiskPosition string

const (
	// PositionEgressInternet 是出公网，风险最高的一类。
	PositionEgressInternet RiskPosition = "EGRESS_INTERNET"
	// PositionCrossNamespace 是跨 namespace。
	PositionCrossNamespace RiskPosition = "CROSS_NAMESPACE"
	// PositionSameNamespace 是同 namespace 内，通常属于正常架构。
	PositionSameNamespace RiskPosition = "SAME_NAMESPACE"
)

// RiskPort 是风险端口清单中的一项。
//
//nolint:revive // 命名与 store 中既有别名及消费方保持一致，重命名会波及整条引用链。
type RiskPort struct {
	Port     int32        `json:"port"`
	Name     string       `json:"name"`
	Category RiskCategory `json:"category"`
}

// ports 是判定所用的封闭端口清单。
//
// 风险来自端口背后的协议语义，不来自"端口是否常见"。任何启发式规则
// （高位端口、非白名单端口）都会把正常架构标成风险，而一份充满误报的
// 安全报告最终会被整体忽略。
//
// 刻意不包含 9090 与 9100：它们是 Prometheus 与 node-exporter 的标准端口。
// 为了让报告有内容而给正常端口贴上风险标签，与伪造判定没有区别。
var ports = map[int32]RiskPort{
	22:    {Port: 22, Name: "SSH", Category: RiskAdminPlaintext},
	23:    {Port: 23, Name: "Telnet", Category: RiskAdminPlaintext},
	3389:  {Port: 3389, Name: "RDP", Category: RiskAdminPlaintext},
	3306:  {Port: 3306, Name: "MySQL", Category: RiskDatabase},
	5432:  {Port: 5432, Name: "PostgreSQL", Category: RiskDatabase},
	6379:  {Port: 6379, Name: "Redis", Category: RiskDatabase},
	27017: {Port: 27017, Name: "MongoDB", Category: RiskDatabase},
	9200:  {Port: 9200, Name: "Elasticsearch", Category: RiskDatabase},
	445:   {Port: 445, Name: "SMB", Category: RiskFileShare},
}

// Catalog 返回判定所用的完整端口清单，按端口号升序。
//
// 随报告一起返回：报告为空时，使用者必须能看到"我们查了哪些端口"。
// 缺了这份清单，一份空报告与一次根本没做的检查在界面上无法区分。
func Catalog() []RiskPort {
	out := make([]RiskPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// Lookup 按端口号查风险分类。第二个返回值为 false 表示不在清单内。
func Lookup(port int32) (RiskPort, bool) {
	p, ok := ports[port]
	return p, ok
}
