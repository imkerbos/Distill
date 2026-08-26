// Package store 定义 Fleet 数据的查询接口及其 fixture 实现。
package store

import (
	networkingv1 "k8s.io/api/networking/v1"

	"context"
	"time"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
)

// ClusterSource 是 store 需要的最小注册信息读取面。
//
// 只要读的两个方法：store 不该有能力改注册表，而按请求解析而非启动时
// 缓存，是因为下线一个集群必须立刻生效 —— 缓存快照会让「已下线」的
// 确认与「仍在服务」的事实之间出现一个没有上限、也没有迹象的时间窗。
type ClusterSource interface {
	Clusters(ctx context.Context) ([]registry.Cluster, error)
	Cluster(ctx context.Context, id string) (registry.Cluster, bool, error)
	// RuleOverrides 返回一个集群下未删除的人工决定。
	//
	// 与集群一起放在同一个来源接口里：两者都来自 registry.Store，
	// 拆成两个参数只会让装配方有机会传进两个不一致的实现。
	RuleOverrides(ctx context.Context, clusterID string) ([]registry.RuleOverride, error)
	// PolicyImports 返回一个集群下未删除的导入策略。
	//
	// 与 RuleOverrides 放在同一个来源接口里，理由相同：两者都来自
	// registry.Store，而它们都会改变候选集长什么样 —— 拆成两个参数只会让
	// 装配方有机会传进两个不一致的实现。
	PolicyImports(ctx context.Context, clusterID string) ([]registry.PolicyImport, error)
}

// TopologyNode 是拓扑图中的一个命名空间节点。
type TopologyNode struct {
	ID                string `json:"id"`
	Cluster           string `json:"cluster"`
	Namespace         string `json:"namespace"`
	InMesh            bool   `json:"inMesh"`
	HasPolicy         bool   `json:"hasPolicy"`
	PodCount          int    `json:"podCount"`
	UnmanagedPodCount int    `json:"unmanagedPodCount"`
	// Foreign 表示该命名空间不属于本次查询的集群。
	// 跨集群边的对端必须作为节点出现，否则前端图会拿到悬空引用；
	// 但它不受本集群策略管辖，展示上要能区分。
	Foreign bool `json:"foreign"`
}

// TopologyEdge 是命名空间之间聚合后的通信关系。
type TopologyEdge struct {
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	Verdict      string  `json:"verdict"`
	Confidence   string  `json:"confidence"`
	CrossCluster bool    `json:"crossCluster"`
	FlowCount    int     `json:"flowCount"`
	Ports        []int32 `json:"ports"`
	// Unmanaged 表示这条边上存在 NetworkPolicy 管不到的流量（如 hostNetwork
	// Pod）。缺了它，一条通往特权组件的边会渲染成普通的绿色放行，
	// 与"策略确实允许了它"无法区分 —— 而这两件事的处置方式完全不同。
	Unmanaged bool `json:"unmanaged"`
	// DecidedBy 是做出判定的方向：INGRESS、EGRESS 或 MIXED。
	//
	// NetworkPolicy 是有方向的，一条 DENY 边究竟该改源端的 egress 规则
	// 还是目的端的 ingress 规则，只看边本身答不出来。
	DecidedBy string `json:"decidedBy"`
}

// Topology 是一个集群的通信拓扑。
type Topology struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
	// UnplaceableFlowCount 是因为端点无法定位到具体命名空间（身份未还原、
	// 出公网）而没能画进任何一条边的流量条数。不报这个数字，拓扑页与数据
	// 质量页会对同一个集群给出两个互不相同的流量总数，而差额里恰恰装着
	// 这个平台最该展示的那部分"不知道"。
	UnplaceableFlowCount int `json:"unplaceableFlowCount"`
	// Level 是本次聚合粒度，回显给调用方。
	Level string `json:"level"`
	// TrafficObserved 表示这份拓扑的边是不是基于真实观测。
	//
	// **为 false 时，空的 Edges 不是「这些工作负载之间没有通信」，而是
	// 「我们一条流量都还没观测过」。** 这两句话的区别是这个平台的核心
	// （CLAUDE.md §3）：没看见 ≠ 不存在，而把前者读成后者的后果是那条规则
	// 被判「无流量、可收紧」，推荐一份切断它的策略。
	//
	// 它是答案的一部分，不是可选的展示提示：调用方拿不到「边为空」而不同时
	// 拿到「因为没有观测」（design doc 2026-08-18 §2）。
	TrafficObserved bool `json:"trafficObserved"`
}

// TimeWindow 是一个左闭右开的时间区间 [From, To)。
//
// 半开而非闭区间：相邻窗口按序切分同一段时间时，闭区间会让边界上的
// flow 被两个窗口各计一次，而对账的分母正是靠窗口累加得出的。
type TimeWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Valid 报告窗口是否可用于查询。
func (w TimeWindow) Valid() bool {
	return !w.From.IsZero() && !w.To.IsZero() && w.From.Before(w.To)
}

// Contains 报告 t 是否落在 [From, To) 内。
func (w TimeWindow) Contains(t time.Time) bool {
	return !t.Before(w.From) && t.Before(w.To)
}

// FlowFilter 是流量列表的筛选条件。除 Window 外，空字段表示不筛选。
type FlowFilter struct {
	Cluster    string
	Verdict    string
	Confidence string
	Limit      int
	// Window 是必填的查询时间窗。
	//
	// 没有默认值：事实层要求 require_partition_filter（spec §5.1），
	// 一个缺失时间条件却照样返回结果的查询，接上真实存储后会变成一次
	// 全表扫描，而账单要到月底才可见。默认值该由装配方按部署形态给出，
	// 数据层只负责拒绝没带窗口的请求。
	Window TimeWindow
}

// ValidVerdict 报告 v 是否属于 verdict 的封闭枚举。
//
// 校验取值而非静默返回空列表：一个拼错的 verdict 会让界面显示"没有流量"，
// 把一次输入错误伪装成一个关于集群的结论。
func ValidVerdict(v string) bool {
	switch replay.Verdict(v) {
	case replay.VerdictAllow, replay.VerdictDeny, replay.VerdictUnknown:
		return true
	default:
		return false
	}
}

// ValidConfidence 报告 c 是否属于 confidence 的封闭枚举。
func ValidConfidence(c string) bool {
	switch replay.Confidence(c) {
	case replay.ConfidenceTrusted, replay.ConfidenceDegraded:
		return true
	default:
		return false
	}
}

// FlowRecord 是流量列表中的一行。
type FlowRecord struct {
	ID string `json:"id"`
	// Timestamp 是该条流量的发生时刻。
	Timestamp     time.Time `json:"timestamp"`
	SourceLabel   string    `json:"sourceLabel"`
	DestLabel     string    `json:"destLabel"`
	Protocol      string    `json:"protocol"`
	Port          int32     `json:"port"`
	Verdict       string    `json:"verdict"`
	Confidence    string    `json:"confidence"`
	UnknownReason string    `json:"unknownReason"`
	CrossCluster  bool      `json:"crossCluster"`
	// Unmanaged 表示这条流量的主体不受 NetworkPolicy 管控（如 hostNetwork
	// Pod）。"策略放行了它"与"策略根本管不到它"都会得到 ALLOW，但前者是
	// 一条可以照着推荐策略的证据，后者是一个必须靠别的手段封堵的敞口。
	Unmanaged bool `json:"unmanaged"`
}

// FlowPage 是一次流量查询的结果，带上截断前的总数。
//
// 不返回裸切片：列表被 Limit 截断时，只看切片的调用方无从知道自己
// 拿到的是全部还是一角，界面会把"我只给你看了 100 条"显示成"一共
// 就这些"。这个平台的每块屏都必须能说清自己没告诉你什么。
type FlowPage struct {
	// Items 是本页的流量。
	Items []FlowRecord
	// Total 是筛选后、截断前的条数。
	Total int
	// Limit 是实际生效的条数上限。未指定时由数据层填入默认值，
	// 由数据层而非 handler 回答，避免默认值在两处各写一份。
	Limit int
	// Window 是实际生效的查询时间窗。
	//
	// 必须回显：一个按时间筛过的列表若不说明筛的是哪一段，在界面上
	// 与全量列表无法区分 —— 与 Total 存在的理由相同。
	Window TimeWindow
}

// DecisionReason 是判定的结构化理由。
type DecisionReason struct {
	Direction      string `json:"direction"`
	Isolated       bool   `json:"isolated"`
	Unmanaged      bool   `json:"unmanaged"`
	MatchedPolicy  string `json:"matchedPolicy"`
	MatchedRuleIdx int    `json:"matchedRuleIdx"`
	Detail         string `json:"detail"`
}

// Decision 是单条流量的完整判定。
type Decision struct {
	FlowRecord
	Reason DecisionReason `json:"reason"`
}

// Quality 是一个集群的数据质量。
//
// UnknownComposition 是构成明细而非单一比例：只报一个 UNKNOWN 百分比
// 无法告诉运维该去修哪个子系统。
//
// TotalFlows 统计的是"与本集群有关"的流量，跨集群流量在两端各计一次，
// 因此各集群的 TotalFlows 不可相加当作总量 —— 两端都得知道自己在跟外面
// 通信，这比一个能对上账的总数重要。
type Quality struct {
	Cluster      string  `json:"cluster"`
	TotalFlows   int     `json:"totalFlows"`
	TrustedRate  float64 `json:"trustedRate"`
	UnknownRate  float64 `json:"unknownRate"`
	DegradedRate float64 `json:"degradedRate"`
	// UnknownCount 是 UNKNOWN 的绝对条数。与 UnknownRate 并列给出，
	// 界面才能把它直接摆在 UnknownComposition 旁边比对，
	// 而不是拿一个浮点比例去反乘总数、再和明细对不上账。
	UnknownCount       int            `json:"unknownCount"`
	UnknownComposition map[string]int `json:"unknownComposition"`
	CrossClusterCount  int            `json:"crossClusterCount"`
	NakedPodCount      int            `json:"nakedPodCount"`
	UnmanagedPodCount  int            `json:"unmanagedPodCount"`
	PolicyCoverage     float64        `json:"policyCoverage"`
}

// Reader 是 Fleet 数据的只读查询接口。
//
// handler 依赖本接口而非具体实现：将来接入 BigQuery 时实现同一接口，
// handler 一行不动。
type Reader interface {
	// DefaultWindow 返回调用方没有指定 from/to 时，这个集群适用的默认时间窗。
	//
	// **按集群问，而不是取一个装配时算好的常量**（design doc 2026-08-18 §3.1）。
	// 默认窗口是一个关于集群的结论 ——「我们实际能回答的是哪一段时间」—— 因此
	// 它必须由回答这个集群的那份数据自己给出：合成数据集给出自己的范围，采集
	// 数据给出这个集群最近一次摄入的窗口。一个跨来源共用的常量窗口会让某一类
	// 集群被问到一段与它无关的时间，而窗口里没被观测到的连接在下游读起来是
	// 「这条规则没有流量、可以收紧」—— 这个平台唯一那个单向的失败方向。
	//
	// 答不出窗口时返回错误，**不返回一个兜底区间**：兜底的那一段时间没有任何
	// 证据支持，而它算出来的判定与一份有证据的判定在界面上长得一模一样。
	// 一个还没有摄入过的集群的正确答案是「还没有可用的采集数据」。
	DefaultWindow(ctx context.Context, clusterID string) (TimeWindow, error)
	// Topology 返回指定集群的通信拓扑。集群不存在时返回错误。
	Topology(ctx context.Context, clusterID string, level TopologyLevel) (Topology, error)
	// Flows 按条件返回流量列表。筛选条件指向不存在的集群时返回错误。
	Flows(ctx context.Context, filter FlowFilter) (FlowPage, error)
	// Flow 返回单条流量的完整判定。不存在时第二个返回值为 false。
	Flow(ctx context.Context, id string) (Decision, bool, error)
	// Quality 返回指定集群的数据质量。集群不存在时返回错误。
	Quality(ctx context.Context, clusterID string) (Quality, error)
	// Security 返回指定集群的安全发现。集群不存在时返回错误。
	Security(ctx context.Context, clusterID string, window TimeWindow) (SecurityReport, error)
	// PolicyPreviewAtGranularity 按指定主体粒度生成候选策略并回放预测。
	// 集群不存在时返回错误。
	//
	// **粒度是必填参数，接口上没有省略它的那个重载。** 两个粒度是两批不同的
	// 策略，而一份 namespace 粒度的策略集配上 workload 粒度算出来的
	// WOULD_BREAK 描述的是另一套策略、且偏在让人放心的方向（粗化只会放宽，
	// 因此拦断更少）。写成可省略，就给了"忘了传"一个位置，而那一次的症状是
	// 屏幕上的策略与数字对不上，且不报错（design doc 2026-08-19 §3）。
	//
	// 未登记的取值由实现收敛到 WORKLOAD —— 那是更精确的那一侧，失败方向朝窄。
	PolicyPreviewAtGranularity(
		ctx context.Context, clusterID, namespace string, window TimeWindow,
		granularity policygen.Granularity,
	) (PolicyPreview, error)
	// EnsureRuleExists 重新生成一次候选集，校验该指纹确实出现在
	// (namespace, workload) 下，且这条决定不会必然失效。
	//
	// 前一半防的是过期页面：指纹对不上，写进去的覆盖不会报错，只会
	// 永远待在「已失效」那一节，而它从来就没生效过。后一半是同一个
	// 道理再往前挪一步 —— 禁用一条 BASELINE 规则在 policygen.Apply
	// 眼里本就必然失效（见 policygen.ErrBaselineNotDisablable），
	// 与其等它落库后才在预览页里显示失效，不如在写库前直接拒绝。
	//
	// window 显式传入而非取某个内部默认值：候选集的内容（含
	// Fingerprint）依赖观测到的流量，换一个窗口，规则可能连带消失或
	// 变化 ——「当前候选集」这个说法必须绑定到具体窗口才有意义。
	EnsureRuleExists(
		ctx context.Context, clusterID, namespace, workload, fingerprint string,
		decision policygen.OverrideDecision, window TimeWindow,
	) error
	// DeletionImpact 预测把 removed 这批 NetworkPolicy 从集群里移除的影响
	// （design doc 2026-08-24 §4.3）。
	//
	// **删除必须与新增走同一条求值路径。** 在这个方法之前，删除是平台唯一
	// 一类不被预测的变更 —— 而它恰恰是伤害最大的那一类：撤掉一条策略，
	// 那一片要么从「有规则」变回默认放行，要么反过来失去唯一那条放行。
	// 因此写回里的删除只在这个方法答得出影响时才被允许（2026-08-14
	// design doc §3 放开删除的两个前提之一）。
	DeletionImpact(
		ctx context.Context, clusterID string, window TimeWindow,
		removed []networkingv1.NetworkPolicy,
	) (DeletionImpactReport, error)
	// LivePolicies 返回**最近一次采集**看到的、这个集群里真实存在的
	// NetworkPolicy（design doc 2026-08-25 §4、§5）。
	//
	// 回答的是「集群里现在有什么」，两个消费方：
	//
	//   写回冲突判定 —— 平台要写的对象名已经被别人占了，就不能写
	//   （`candidate-` 是约定不是保留字，覆盖掉的是别人的放行规则）。
	//
	//   真实漂移 —— 仓库声明的对象到底有没有被 GitOps controller 落下去。
	//   既有的 driftResult 比的是「仓库 vs 平台最后写过的」，答不了这个。
	//
	// 用最近一次采集而不是某个窗口锚点：这两个问题都是关于**此刻**的，
	// 与 DeletionImpact 里 Live 那一半同源。
	LivePolicies(ctx context.Context, clusterID string) ([]networkingv1.NetworkPolicy, error)
	// Retirement 逐条评估集群现有策略能不能退休
	// （design doc 2026-08-25-existing-policies §6：接管模式）。
	//
	// **平台只报告，不删**：它对被管集群没有策略写权限（CLAUDE.md §3）。
	// 每一条的结论只描述"单独退休它"，不描述一次退休多条 —— 两条策略可能
	// 互相兜底，各自单删都没影响，一起删就断了。
	Retirement(ctx context.Context, clusterID string, window TimeWindow) (RetirementReport, error)
	// Reconciliation 把平台回放算出的判定与执行平面自己报的判定对账
	// （design doc 2026-08-25 §3）。
	//
	// **这是平台唯一一个能在生产流量上度量的可信度指标。** 求值正确性可以被
	// golden test 与一致性测试证明，但那是一组手写用例；一致率量的是同一个
	// 引擎在真实集群、真实流量上与执行平面差了多少。
	//
	// 只有报判定的来源才对得起来（Hubble 报，NODE_CONNTRACK 不报）：来源没报
	// 的连接落进 SOURCE_SILENT，不进一致率的分母。
	Reconciliation(
		ctx context.Context, clusterID string, window TimeWindow,
	) (ReconciliationReport, error)
}
