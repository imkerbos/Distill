package collectstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/store"
)

// defaultFlowLimit 是未指定条数上限时返回的条数。
//
// 与 FixtureReader 取同一个数，但理由不同：那边的数据集刻意小到能被整个
// 装下，这边装不下 —— 一个真实集群单小时约 4000 条连接（design doc §9）。
// 因此这里的截断是常态，FlowPage.Total 与 Limit 必须照实回显，否则界面会把
// "我只给你看了 500 条"显示成"一共就这些"。
const defaultFlowLimit = 500

// maxFlowLimit 是调用方能要到的最大条数。
//
// 调用方选的上限不得无界（安全规范 §23 / §24）：一次 limit=999999999 会把
// 整个窗口的连接连同每一条的求值一起装进内存。取值与 httpapi 那道一致，
// 但两道各自独立 —— handler 那道拦的是 HTTP 调用方，这道拦的是所有调用方，
// 拆掉任何一道另一道仍然成立。
const maxFlowLimit = 1000

// Flows 返回一个集群在指定窗口里的流量，逐条带上判定。
//
// **无法归属的连接照样在列表里**，以 UNKNOWN 加一个封闭枚举里的原因出现
// （design doc §3）。它们不是噪声：观测集合一旦小于真实集合，覆盖那部分
// 流量的规则就会被判成"无流量、可收紧"。
func (r *Reader) Flows(ctx context.Context, filter store.FlowFilter) (store.FlowPage, error) {
	// 先于集群校验：缺时间窗是调用方用错了接口，与查哪个集群无关。
	if !filter.Window.Valid() {
		return store.FlowPage{}, store.ErrWindowRequired
	}
	if filter.Cluster == "" {
		// 本 Reader 只服务登记为 COLLECTED 的集群，而"不筛集群"要求跨集群
		// 聚合 —— 那是 design doc §8 明确排除的。拒绝而不是悄悄返回空列表：
		// 空列表读起来是"这段时间没有流量"。
		return store.FlowPage{}, fmt.Errorf(
			"collectstore: a flow list must name a cluster; this reader answers for one cluster at a time")
	}
	// 取值不在封闭枚举里就拒绝，不静默返回空列表：一个拼错的 verdict 会让
	// 界面把一次输入错误显示成一个关于集群的结论（store.ValidVerdict）。
	if filter.Verdict != "" && !store.ValidVerdict(filter.Verdict) {
		return store.FlowPage{}, fmt.Errorf("collectstore: unregistered verdict %q", filter.Verdict)
	}
	if filter.Confidence != "" && !store.ValidConfidence(filter.Confidence) {
		return store.FlowPage{}, fmt.Errorf("collectstore: unregistered confidence %q", filter.Confidence)
	}

	t, err := r.readTraffic(ctx, filter.Cluster, flow.Window{From: filter.Window.From, To: filter.Window.To})
	if err != nil {
		return store.FlowPage{}, err
	}

	limit := boundedFlowLimit(filter.Limit)
	items := make([]store.FlowRecord, 0, len(t.conns))
	for i, c := range t.conns {
		rec := t.record(i, t.attribute(c))
		if filter.Verdict != "" && rec.Verdict != filter.Verdict {
			continue
		}
		if filter.Confidence != "" && rec.Confidence != filter.Confidence {
			continue
		}
		items = append(items, rec)
	}
	sortFlowRecords(items)

	total := len(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return store.FlowPage{Items: items, Total: total, Limit: limit, Window: filter.Window}, nil
}

// Flow 返回单条流量的完整判定。不存在时第二个返回值为 false。
//
// ID 自带集群与窗口（见 flowid.go），因此这里重新解析的是**那一段**窗口，
// 不是最近一次观测：拿最近一次窗口回答一个指向上周的链接，会答出另一条
// 连接的判定，而它答得出、不报错。
//
// 集群不存在、ID 解不开、指纹对不上，一律答"没有这条流量"：把三者合并成
// 同一个响应，调用方才无法靠响应差异探出一个自己看不见的集群是否存在
// （与 FixtureReader.Flow 同一条理由）。
func (r *Reader) Flow(ctx context.Context, id string) (store.Decision, bool, error) {
	ref, ok := parseFlowID(id)
	if !ok {
		return store.Decision{}, false, nil
	}
	t, err := r.readTraffic(ctx, ref.clusterID, ref.window)
	if err != nil {
		if errors.Is(err, store.ErrClusterNotFound) {
			return store.Decision{}, false, nil
		}
		return store.Decision{}, false, err
	}
	if ref.index >= len(t.conns) {
		return store.Decision{}, false, nil
	}
	c := t.conns[ref.index]
	if flowFingerprint(c) != ref.fingerprint {
		// 这段窗口被重新摄入过，序号已经不指向当初那一条。答"没有"，
		// 不答一条别的：一个指着错误连接的下钻页没有任何症状。
		return store.Decision{}, false, nil
	}

	a := t.attribute(c)
	return store.Decision{
		FlowRecord: t.record(ref.index, a),
		Reason: store.DecisionReason{
			Direction:      string(a.decision.Reason.Direction),
			Isolated:       a.decision.Reason.Isolated,
			Unmanaged:      a.decision.Reason.Unmanaged,
			MatchedPolicy:  a.decision.Reason.MatchedPolicy,
			MatchedRuleIdx: a.decision.Reason.MatchedRuleIdx,
			Detail:         a.decision.Reason.Detail,
		},
	}, true, nil
}

// boundedFlowLimit 把调用方要的条数收进 [1, maxFlowLimit]。
//
// 收窄而不是报错，且由 FlowPage.Limit 回显生效值：截断本身必须是可见的，
// 而一个越界的 limit 通常是调用方想"要全部"，拒绝只会让它拿不到任何东西。
// 越界不可见才是危险的那一种 —— 那时列表看起来就是全部。
func boundedFlowLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultFlowLimit
	case limit > maxFlowLimit:
		return maxFlowLimit
	default:
		return limit
	}
}

// sortFlowRecords 给列表定一个稳定的顺序。
//
// 必须与内容有关而不是与读取顺序有关：截断发生在排序之后，一个随读取顺序
// 变化的列表会让同一次查询的第一页在两次刷新之间换一批连接，而调用方无从
// 察觉。端口进排序键 —— 同一对主体之间的两个端口是两条不同的连接。
func sortFlowRecords(items []store.FlowRecord) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.SourceLabel != b.SourceLabel:
			return a.SourceLabel < b.SourceLabel
		case a.DestLabel != b.DestLabel:
			return a.DestLabel < b.DestLabel
		case a.Protocol != b.Protocol:
			return a.Protocol < b.Protocol
		case a.Port != b.Port:
			return a.Port < b.Port
		default:
			return a.ID < b.ID
		}
	})
}
