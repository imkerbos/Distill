// Package flow 定义流量摄入的契约：一个时间窗里看见了哪些连接，以及
// 这次观测漏掉了多少。
//
// 纯逻辑：零 I/O、零框架依赖，只直接依赖标准库与 internal/identity。
// 连 Hubble relay、读 BigQuery 都在实现方，本包只有类型与接口。
//
// 每个类型都围着同一件事设计：**没看见不等于不存在**（spec §2）。平台的
// 核心结论是"这批规则会拦断哪些现有连接"，它成立的前提是观测到的连接
// 集合接近真实存在的集合 —— 而流量数据天然不满足：采样、摄入中断、
// 一条每天只跑一次的连接落在窗口外，都会让观测少掉一条。少掉的失败方向
// 是单向的：漏一条 → 平台认为它不存在 → 覆盖它的规则被判"无流量、可
// 收紧" → 推荐一份切断它的策略；永远不会朝反方向错。
//
// 因此这里的取值一律往"不知道"的方向倒：零值读作未知而不是完整，采样率
// 取不到就是取不到（不是 1.0），来源没报判定不等于放行，一条连接可以
// 完全没有身份。让人放心的那个读法正是危险的那个。
package flow

import (
	"context"
	"time"
)

// SourceKind 是流量来源的种类，封闭枚举。
//
// 逐条落到连接上（spec §4）：完整度元数据是按来源给的，"这条连接是谁
// 看见的"决定了它该按哪一份元数据解释。F 轮接 VPC flow logs 时在这里
// 加取值，并同步统计口径。
type SourceKind string

// SourceHubble 是集群内 Hubble relay 的实时流量。
const SourceHubble SourceKind = "HUBBLE"

// Valid 判断该来源是否已登记。
//
// 显式 switch 而非查表：新增一个取值却没在这里列出来，它就不是一个合法
// 来源，而这一行的缺失在评审里看得见。零值不合法 —— 一批说不出自己
// 从哪来的连接，也就说不出它的完整度该按什么解释。
func (k SourceKind) Valid() bool {
	switch k {
	case SourceHubble:
		return true
	default:
		return false
	}
}

// Window 是一个左闭右开的摄入时间窗 [From, To)。
//
// 半开而非闭区间：相邻窗口按序切分同一段时间时，闭区间会让边界上的连接
// 被两个窗口各计一次，而对账的分母正是靠窗口累加得出的 —— 与
// store.TimeWindow 同一条理由。不 import 它，是因为本包只直接依赖标准库
// （与 internal/identity 重新声明 RunStatus 同源），而 internal/store 拉着
// k8s 类型与半个数据面。
type Window struct {
	From time.Time
	To   time.Time
}

// Valid 报告窗口是否可用。零值不合法：一个没有边界的窗口无从判断覆盖。
func (w Window) Valid() bool {
	return !w.From.IsZero() && !w.To.IsZero() && w.From.Before(w.To)
}

// Covers 报告 w 是否完整包住 other。
//
// 任一方不合法就是 false：说不出边界的窗口不得被读成"包住了"，那正是
// 把"不知道"读成"没漏"的那个方向。
func (w Window) Covers(other Window) bool {
	if !w.Valid() || !other.Valid() {
		return false
	}
	return !w.From.After(other.From) && !w.To.Before(other.To)
}

// Source 是一次时间窗内的流量摄入。
//
// 返回 IngestResult 而不是一串连接：一次摄入的结论不只是"看见了这些"，
// 还有"这段时间看得全不全"，两者必须一起交出去，否则调用方拿到的就是
// 一份看起来完整的观测。
//
// clusterID 是参数而不是实现里的字段：不同集群 Pod CIDR 可能重叠，一批
// 不带集群的连接落到事实层会 join 到别的集群的 Pod 上且不报错
// （CLAUDE.md §4）。
//
// 接口不假设流量自带身份：Hubble 的流量带 Pod 标签，VPC flow logs 只有
// 地址，后者必须走 C 轮的解析器还原（spec §3）。把身份写成必填，第二个
// 来源就只能编一个身份来满足契约。
type Source interface {
	Ingest(ctx context.Context, clusterID string, window Window) (IngestResult, error)
}
