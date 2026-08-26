package main

import (
	networkingv1 "k8s.io/api/networking/v1"

	"context"
	"database/sql"
	"fmt"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/store"
)

// dispatchReader 是挂在 httpapi.Deps 上的那一个 store.Reader：它自己不读任何
// 数据，只在**每一次调用**上按集群登记的来源选出真正作答的 Reader 并转发。
//
// 做成一个 Reader 而不是让边界层拿两个 Reader 自己挑：挑选的判据是
// 「这个集群登记的来源是什么」，那是一件与 HTTP 无关的事。放进 handler
// 就要在六个读端点各写一遍，而漏掉的那一个不会有任何症状 —— 它会照常
// 作答，只是答的是另一份数据。
//
// **分派在每个请求上做，不在启动时做**（design doc 2026-08-18 §2）。理由与
// httpapi.Deps.Registry 传注册表而非快照相同：一个集群的来源在库里改掉之后，
// 进程不该继续按旧的答，直到重启。
type dispatchReader struct {
	// src 是注册信息的读取面，每次调用现查。
	src store.ClusterSource
	// collected 是采集侧的 Reader，取具体类型而不是 store.Reader，理由见
	// readerFor：一个装着 nil 指针的接口值不等于 nil，而这里判空的后果是
	// 「一个 COLLECTED 集群到底能不能被服务」。
	collected *collectstore.Reader
}

// newDispatchReader 装出按登记来源分派的 Reader。
func newDispatchReader(src store.ClusterSource, collected *collectstore.Reader) *dispatchReader {
	return &dispatchReader{src: src, collected: collected}
}

// newReader 装出挂到 httpapi.Deps.Reader 上的那一个 Reader。
//
// 单列一个函数，且**返回具体类型 `*dispatchReader` 而不是 store.Reader**：
// 返回类型这一笔是编译器守的 —— 把这里改成 `return newFixtureReader(reg)`
// 不是一次悄悄的退化，是一个编译错误。用接口作返回类型就没有这道保护，
// 那正是本轮之前 main.go 那一行的处境：改回 fixture-only 之后全部测试仍然
// 全绿（branch review I1）。
//
// 与之配套的是 TestAssemblyHandsTheDispatchingReaderToTheRouter：它从
// main.go 的语法树上确认 run() 交给 Deps.Reader 的确实是这个函数的返回值。
// 两道合起来才封住那一行 —— 编译器管「这个函数只能交出分派器」，语法树那条
// 管「装配层仍然在用这个函数」。
func newReader(db *sql.DB, reg store.ClusterSource) *dispatchReader {
	return newDispatchReader(reg, collectstore.New(db, reg))
}

// 编译期确认它满足边界层要的形状。放在这里而非测试里：接口对不上应当在
// 构建时失败，而不是等到装配时才发现。
var _ store.Reader = (*dispatchReader)(nil)

// readerOf 现查一个集群，并按它**登记的**来源选出作答的 Reader。
//
// 选择本身一概交给 readerFor，本方法只负责「把集群查出来」：那三条性质
// （只看登记的来源、COLLECTED 拿不到 fixture、未登记取值拒绝）已经在那里
// 各自有测试守着，在这里重写一遍就等于给它们准备了一个会各自漂移的副本。
//
// 集群查不到时报 ErrClusterNotFound 而不是随手挑一个 Reader：一个不存在的
// 集群 ID 的正确答案是「没有这个集群」，而任何一种兜底都会让一次拼写错误
// 得到一份看上去正常的报告（安全规范 §49）。
func (d *dispatchReader) readerOf(ctx context.Context, clusterID string) (store.Reader, error) {
	c, ok, err := d.src.Cluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", store.ErrClusterNotFound, clusterID)
	}
	return readerFor(c, d.src, d.collected)
}

// DefaultWindow 转发给这个集群登记的来源对应的 Reader。
//
// 默认时间窗走**这个**分派器，而不是在装配时算一个常量交给边界层
// （design doc 2026-08-18 §3.1）。判据与六个读方法逐字相同 —— 「这个集群
// 登记的来源是什么」—— 因此它属于同一个分派，不该在旁边另起一个形状不同的。
// 装配时算一次的那个常量取的是合成数据集自己的范围，于是一个 COLLECTED
// 集群的每一屏都被钉在去年那几十秒上，而窗口里没被观测到的连接在推荐里
// 就是「没有流量、可以收紧」。
//
// 不点名集群时拒绝，与 Flows 同一个哨兵：默认窗口是**按集群**推出来的结论，
// 没有集群就没有这个结论，而任何一个跨集群的兜底窗口都会让某一类集群被问到
// 一段与它无关的时间。落到 readerOf 上会答成「没有这个集群」，那是另一句话。
func (d *dispatchReader) DefaultWindow(
	ctx context.Context, clusterID string,
) (store.TimeWindow, error) {
	if clusterID == "" {
		return store.TimeWindow{}, store.ErrClusterRequired
	}
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.TimeWindow{}, err
	}
	return r.DefaultWindow(ctx, clusterID)
}

// Topology 转发给这个集群登记的来源对应的 Reader。
func (d *dispatchReader) Topology(
	ctx context.Context, clusterID string, level store.TopologyLevel,
) (store.Topology, error) {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.Topology{}, err
	}
	return r.Topology(ctx, clusterID, level)
}

// Quality 转发给这个集群登记的来源对应的 Reader。
func (d *dispatchReader) Quality(ctx context.Context, clusterID string) (store.Quality, error) {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.Quality{}, err
	}
	return r.Quality(ctx, clusterID)
}

// Security 转发给这个集群登记的来源对应的 Reader。
func (d *dispatchReader) Security(
	ctx context.Context, clusterID string, window store.TimeWindow,
) (store.SecurityReport, error) {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.SecurityReport{}, err
	}
	return r.Security(ctx, clusterID, window)
}

// PolicyPreviewAtGranularity 转发给这个集群登记的来源对应的 Reader。
func (d *dispatchReader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, window store.TimeWindow,
	granularity policygen.Granularity,
) (store.PolicyPreview, error) {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.PolicyPreview{}, err
	}
	return r.PolicyPreviewAtGranularity(ctx, clusterID, namespace, window, granularity)
}

// EnsureRuleExists 转发给这个集群登记的来源对应的 Reader。
//
// 它是写路径的前置校验，因此分派错了的后果比其余五个都重：拿另一份数据
// 校验出来的「这条规则存在」会让一条对不上任何候选集的覆盖决定落库。
// 采集侧至今**明确拒绝**这个方法（collectstore/notyet.go），于是一个
// COLLECTED 集群在这条路上的答案是拒绝，不是放行。
func (d *dispatchReader) EnsureRuleExists(
	ctx context.Context, clusterID, namespace, workload, fingerprint string,
	decision policygen.OverrideDecision, window store.TimeWindow,
) error {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return err
	}
	return r.EnsureRuleExists(ctx, clusterID, namespace, workload, fingerprint, decision, window)
}

// Reconciliation 按集群登记的来源转发一次对账。
func (d *dispatchReader) Reconciliation(
	ctx context.Context, clusterID string, window store.TimeWindow,
) (store.ReconciliationReport, error) {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.ReconciliationReport{}, err
	}
	return r.Reconciliation(ctx, clusterID, window)
}

// LivePolicies 按集群登记的来源转发一次「集群里现在有什么策略」的查询。
func (d *dispatchReader) LivePolicies(
	ctx context.Context, clusterID string,
) ([]networkingv1.NetworkPolicy, error) {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return r.LivePolicies(ctx, clusterID)
}

// DeletionImpact 按集群登记的来源转发一次删除影响预测。
//
// 与其余读方法同一条路：一个登记为 FIXTURE 的集群拿到的是合成数据集的答案，
// 一个 COLLECTED 集群拿到的是它自己的观测算出来的答案。分派在这里做一次，
// 不在调用方 —— 写回端点不该知道这个集群的数据来自哪儿。
func (d *dispatchReader) DeletionImpact(
	ctx context.Context, clusterID string, window store.TimeWindow,
	removed []networkingv1.NetworkPolicy,
) (store.DeletionImpactReport, error) {
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.DeletionImpactReport{}, err
	}
	return r.DeletionImpact(ctx, clusterID, window, removed)
}

// Flows 要求调用方点名集群，随后按那个集群登记的来源转发。
//
// **不点名集群一律拒绝**（design doc 2026-08-18 §3.2）。另外两种可能都不能要：
// 合并两个 Reader 的结果会让演示流量与真实流量同屏；只答 fixture 那一半会让
// 真集群的流量整体缺席，而缺席在这个平台上读起来是「这条规则没有流量、
// 可以收紧」—— 唯一那个单向的失败方向。
//
// 在这里拒绝而不是只靠 collectstore.Flows 那道：那一道只在已经选中采集侧
// Reader 之后才生效，而「不点名集群」恰恰是选不出 Reader 的那种输入。
func (d *dispatchReader) Flows(ctx context.Context, filter store.FlowFilter) (store.FlowPage, error) {
	if filter.Cluster == "" {
		return store.FlowPage{}, store.ErrClusterRequired
	}
	r, err := d.readerOf(ctx, filter.Cluster)
	if err != nil {
		return store.FlowPage{}, err
	}
	return r.Flows(ctx, filter)
}

// Flow 按 ID 自带的集群分派；ID 解不开时交给 fixture 的 Reader。
//
// 采集侧的 ID 把集群编进了载荷（collectstore.flowIDOf），合成数据集的 ID 则是
// `flow-0214` 这种形状、不带任何集群。于是解得开就按它点名的集群分派，解不开
// 就交给 fixture（design doc 2026-08-18 §3.1）。
//
// 第二步看起来像兜底，但它**不是**一条通往合成数据的漏路：newFixtureReader
// 装出来的 Reader 的数据源被 fixtureOnlySource 收窄过，而 FixtureReader.Flow
// 经由 visibleFlows 取数 —— 那里要求一条记录的**两端**都在收窄后的清单里，
// 一个 COLLECTED 集群不在。所以就算这里判错、把一个真集群的下钻交给 fixture，
// 得到的也是「没有这条」，不是一份合成判定。
//
// **两道各自独立**：拆掉这里的分派，收窄的数据源仍然守得住；拆掉收窄，
// 这里的分派仍然守得住。这是纵深防御在这条链路上的具体形态，不是重复。
func (d *dispatchReader) Flow(ctx context.Context, id string) (store.Decision, bool, error) {
	clusterID, ok := collectstore.ClusterOfFlowID(id)
	if !ok {
		return newFixtureReader(d.src).Flow(ctx, id)
	}
	r, err := d.readerOf(ctx, clusterID)
	if err != nil {
		return store.Decision{}, false, err
	}
	return r.Flow(ctx, id)
}
