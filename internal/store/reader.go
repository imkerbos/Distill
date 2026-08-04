// Package store 定义 Fleet 数据的查询接口及其 fixture 实现。
package store

import (
	"context"

	"github.com/imkerbos/Distill/internal/replay"
)

// ClusterSummary 是集群概览。
type ClusterSummary struct {
	ID             string `json:"id"`
	NamespaceCount int    `json:"namespaceCount"`
	PodCount       int    `json:"podCount"`
	FlowCount      int    `json:"flowCount"`
	CCNPPresent    bool   `json:"ccnpPresent"`
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
}

// FlowFilter 是流量列表的筛选条件。空字段表示不筛选。
type FlowFilter struct {
	Cluster    string
	Verdict    string
	Confidence string
	Limit      int
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
	ID            string `json:"id"`
	SourceLabel   string `json:"sourceLabel"`
	DestLabel     string `json:"destLabel"`
	Protocol      string `json:"protocol"`
	Port          int32  `json:"port"`
	Verdict       string `json:"verdict"`
	Confidence    string `json:"confidence"`
	UnknownReason string `json:"unknownReason"`
	CrossCluster  bool   `json:"crossCluster"`
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
	// Clusters 返回全部集群概览。
	Clusters(ctx context.Context) ([]ClusterSummary, error)
	// Topology 返回指定集群的通信拓扑。集群不存在时返回错误。
	Topology(ctx context.Context, clusterID string) (Topology, error)
	// Flows 按条件返回流量列表。筛选条件指向不存在的集群时返回错误。
	Flows(ctx context.Context, filter FlowFilter) (FlowPage, error)
	// Flow 返回单条流量的完整判定。不存在时第二个返回值为 false。
	Flow(ctx context.Context, id string) (Decision, bool, error)
	// Quality 返回指定集群的数据质量。集群不存在时返回错误。
	Quality(ctx context.Context, clusterID string) (Quality, error)
}
