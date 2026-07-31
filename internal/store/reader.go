// Package store 定义 Fleet 数据的查询接口及其 fixture 实现。
package store

import "context"

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
}

// Topology 是一个集群的通信拓扑。
type Topology struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
}

// FlowFilter 是流量列表的筛选条件。空字段表示不筛选。
type FlowFilter struct {
	Cluster    string
	Verdict    string
	Confidence string
	Limit      int
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
type Quality struct {
	Cluster            string         `json:"cluster"`
	TotalFlows         int            `json:"totalFlows"`
	TrustedRate        float64        `json:"trustedRate"`
	UnknownRate        float64        `json:"unknownRate"`
	DegradedRate       float64        `json:"degradedRate"`
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
	// Flows 按条件返回流量列表。
	Flows(ctx context.Context, filter FlowFilter) ([]FlowRecord, error)
	// Flow 返回单条流量的完整判定。不存在时第二个返回值为 false。
	Flow(ctx context.Context, id string) (Decision, bool, error)
	// Quality 返回指定集群的数据质量。集群不存在时返回错误。
	Quality(ctx context.Context, clusterID string) (Quality, error)
}
