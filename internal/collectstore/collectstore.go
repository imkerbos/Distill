// Package collectstore 用一个集群**真实采集到的**资产与流量回答读请求。
//
// 接入按"错了有多严重"分阶段（design doc 2026-08-17 §7）：Topology 与 Quality
// 只描述、不推荐，先接；Flows / Flow / Security 展示逐条判定与风险发现，其次；
// PolicyPreview 驱动推荐与 dry-run，是唯一一个错了会直接导致生产阻断建议的，
// 留到最后 —— 现已接上（policy.go）。仍未接的只剩写路径的前置校验
// EnsureRuleExists，它在 notyet.go 里**明确拒绝**，不返回空结果。
//
// 本包守两条性质，两条各自都有自己的危害：
//
//  1. **没有采集不等于空集群。** 一份空拓扑是在说"这个集群里什么都没有"，
//     那是一句没有人确认过的话，而且读起来让人放心。没有可用采集时一律
//     返回 ErrNoCollection —— 与事实层的 ErrNoIngestWindow、采集摘要的
//     ErrNoRun 同一个形状（design doc §2）。
//
//  2. **资产按被问到的那个时刻从区间表取，不取最新快照。** Pod 地址会被
//     回收，拿今天的快照解释昨天的一条连接会把它算到另一个工作负载头上，
//     而且答得出、不报错（CLAUDE.md §4）。区间表就是为这件事建的。
//
// 解析器答 AMBIGUOUS / NOT_COVERED / NO_DATA 时主体就是未知，按 design doc §3
// 归入 UNKNOWN 并带上既有的封闭原因枚举，**绝不回退到最近的那个区间**。
package collectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// ErrNoCollection 表示这个集群还没有可用的采集，因而没有任何可描述的事实。
//
// 与"采集过、这段时间什么都没有"必须区分（design doc §2）：后者是一个关于
// 集群的结论，前者只是我们还没看过它。把两者都渲染成一张空表，会让一次
// 持续的采集故障与一个真正安静的集群长得一模一样 —— 而空表读起来是让人
// 放心的那一个。
//
// 做成 error 而不是结果里的一个布尔：调用方拿不到 Topology / Quality，
// 也就写不出"先取结果、再顺手忽略那个布尔"的代码。
var ErrNoCollection = errors.New("collectstore: no usable collection for this cluster")

// ErrNoFlowIngest 表示这个集群采过资产，但一次流量摄入都没有。
//
// **包着 ErrNoCollection**（用 %w 构造），因此原先用 errors.Is 判 ErrNoCollection
// 的调用方行为不变；要区分的那几处再多判一次这一个。
//
// 分出来是因为「一无所知」与「资产有、流量没有」的处置完全不同：前者只能拒绝，
// 我们对这个集群什么都不知道；后者答得出资产已经能回答的那部分
// （design doc 2026-08-18 §3）。合并之后，前者会走上按资产作答那条路，而那条
// 路上没有资产可用 —— 失败方式会从一次明确的拒绝变成一份空报告。
var ErrNoFlowIngest = fmt.Errorf("%w: no flow ingest", ErrNoCollection)

// ErrWindowPurged 表示这个窗口早于保留水位，那段连接已经被清理掉了。
//
// **与"这段时间没有流量"必须分开。** 清理之后那段窗口查出来是零条，而
// 零条会被读成一个关于集群的结论：那时没有人在通信。它不是 —— 我们只是
// 不再留着那段证据（同 identity 的 NOT_COVERED / NO_DATA 那条纪律，
// 也是 design doc 2026-09-02 §2.2 立这套水位的全部理由）。
//
// 是窗口级的错误而不是 replay.UnknownReason：那个枚举挂在每一条流量上，
// 而窗口被清理之后根本没有流量可挂。
var ErrWindowPurged = errors.New("collectstore: the flow window has been purged by retention")

// ErrReadNotCollectedYet 表示这个读方法还没有接到采集数据上。
//
// 分阶段接入的中间态必须是一次明确的拒绝，不是一份空结果，也不是一份
// 合成数据（design doc §7）：一个 COLLECTED 集群的读取路径上不得存在
// 任何通往 fixture 的分支，而"暂时没实现"最容易被顺手填成那条分支。
var ErrReadNotCollectedYet = errors.New("collectstore: this read is not backed by collected data yet")

// maxIntervalRows 是一次读取允许载入的身份区间条数上限。
//
// 区间表的行数与 Pod 生命周期次数同阶（migrations/000012）。超出即报错、
// **不截断**：截断会让一部分地址凭空变成"解析不出主体"，于是那些连接被算进
// UNKNOWN —— 一个纯粹的读取上限伪装成一次关于集群的观测结论（安全规范 §24）。
//
// **今天这个数没有上界。** 仓库里没有任何回收或老化区间行的代码，因此行数
// 单调增长，越过这条线之后本集群的六个读方法**同时**不可读。到达条件与代价
// 记在 design doc 2026-08-17 §10。
const maxIntervalRows = 50_000

// maxSnapshotRows 是一次读取允许载入的快照行数上限（Pod、NetworkPolicy 各自）。
const maxSnapshotRows = 50_000

// Reader 用采集数据实现 store.Reader。
//
// 持有注册表读取面而不是启动时读出的一份集群清单：集群是否受平台管理、
// 以及它登记的数据来源，必须在每个请求上现查 —— 与 FixtureReader 同一条
// 理由，也与"来源是登记出来的、不是推断出来的"同一条理由。
type Reader struct {
	db *sql.DB
	// facts 是事实层的读取面。连接与它的完整度只能从这里取，不在本包重写
	// 一遍那段 SQL：那份完整度是由证据算出来的，复制一份就多一个会与证据
	// 对不上的位置。
	facts *snapshotstore.Store
	src   store.ClusterSource
}

// New 用已建立的连接池与注册表读取面构造 Reader。
//
// 事实层在这里自己装而不是由调用方传进来：两者必须是同一个库，
// 拆成两个参数只会给装配方一个传进两个不同数据库的位置。
func New(db *sql.DB, src store.ClusterSource) *Reader {
	return &Reader{db: db, facts: snapshotstore.New(db), src: src}
}

// 编译期确认本类型仍然满足 store 要的形状。
var _ store.Reader = (*Reader)(nil)

// collectedCluster 现查一个集群，并确认它登记的来源确实是 COLLECTED。
//
// 这是"COLLECTED 集群不得通往 fixture"那条纪律的镜像另一半：装配层
// （cmd/distill-api/reader.go）按来源选 Reader，而这里再独立确认一次自己
// 服务的集群确实登记为 COLLECTED。**即便有人把装配层那个 switch 拨反**，
// 一个 FIXTURE 集群在这里也只会拿到 ErrClusterNotFound，而不是一份用采集
// 数据拼出来、却写着演示集群名字的报告。两道各自独立，拆掉任何一道另一道
// 仍然成立。
//
// 顺带把登记内容交回调用方：判定要用到 CCNPPresent，再查一次注册表只会多出
// 一个"门禁看到的集群"与"判定用到的集群"不是同一行的位置。
func (r *Reader) collectedCluster(ctx context.Context, clusterID string) (registry.Cluster, error) {
	c, ok, err := r.src.Cluster(ctx, clusterID)
	if err != nil {
		return registry.Cluster{}, err
	}
	if !ok || c.DataSource != registry.DataSourceCollected {
		return registry.Cluster{}, fmt.Errorf("%w: %s", store.ErrClusterNotFound, clusterID)
	}
	return c, nil
}

// described 是一个集群在"最近一次真正观测到流量的那个窗口"里的全部事实。
//
// 一次读请求解析一次，Topology 与 Quality 共用：两个页面描述同一个集群时
// 必须描述同一段时间，各自现取会让两屏之间隔着一次采集，而差额无人解释。
type described struct {
	clusterID string
	// window 是被描述的观测窗口。
	window flow.Window
	// at 是解释资产的时刻，取窗口起点。
	//
	// 窗口是左闭右开的，起点是这段观测确实在看的第一个时刻。事实层按窗口
	// 而不是按报文存连接（internal/flow 的 Connection 没有时间戳），因此这
	// 一整批连接只能用同一个时刻解释；地址在窗口中途换过主体时，
	// subjectAt 把它判成 AMBIGUOUS 而不是任选一端 —— 见那里的说明。
	at time.Time
	// anchor 是覆盖 at 的那次采集运行的 observed_at。
	//
	// 取"不晚于 at 的最近一次"，不是最新一次：拿今天的快照解释昨天的连接
	// 正是 CLAUDE.md §4 禁止的那件事。
	anchor       time.Time
	conns        []flow.Connection
	completeness flow.Completeness
	// intervals 是这个集群全部身份区间，按地址分组。
	//
	// 按地址取**全部**而不只取覆盖 at 的那些：identity.Resolve 要靠一个地址
	// 的完整区间集合才分得清"那时这个 IP 没有 Pod"（NOT_COVERED）与"平台
	// 那段时间没在看"（NO_DATA），subjectAt 也要靠它才看得出这个地址在窗口
	// 里换没换过手。只喂覆盖那一刻的区间，两个判断都会退化成"没覆盖"。
	intervals map[string][]identity.Interval
	// living 是覆盖 at 的区间，即那一刻活着的 Pod。
	living []identity.Interval
}

// describe 解析一次读请求要描述的那段时间与那一刻的全部事实。
//
// 顺序是刻意的：先确认集群、再确认有没有可用采集，最后才读事实。任何一步
// 答不出来都返回 ErrNoCollection，**不进入下一步拼一份空结果**。
func (r *Reader) describe(ctx context.Context, clusterID string) (described, error) {
	if _, err := r.collectedCluster(ctx, clusterID); err != nil {
		return described{}, err
	}

	window, err := r.latestFlowWindowAt(ctx, clusterID, time.Time{})
	if err != nil {
		return described{}, err
	}
	return r.describeAt(ctx, clusterID, window)
}

// describeAssets 解析一个集群**最近一次采集**那一刻的资产事实。
//
// 与 describe 的区别只有一处：不要求有过流量摄入。用于回答那些本来就只依赖
// 资产的问题（拓扑的节点、裸奔 Pod）——它们今天被挡住，只是因为同一屏上
// 另一部分没有数据（design doc 2026-08-18 §1）。
//
// 锚点取**最近一次成功的采集运行**，不是「不晚于某个时刻的最近一次」：没有
// 窗口，也就没有「那时候」，要描述的就是最新的现状。
//
// 集群从没被采过时仍然返回 ErrNoCollection —— 那时我们一无所知，给什么都是编的。
func (r *Reader) describeAssets(ctx context.Context, clusterID string) (described, error) {
	if _, err := r.collectedCluster(ctx, clusterID); err != nil {
		return described{}, err
	}
	anchor, err := r.assetAnchor(ctx, clusterID, time.Now())
	if err != nil {
		return described{}, err
	}

	intervals, err := r.readIntervals(ctx, clusterID)
	if err != nil {
		return described{}, err
	}

	d := described{
		clusterID: clusterID,
		at:        anchor,
		anchor:    anchor,
		// conns 为空、completeness 为 UNKNOWN：**不是** COMPLETE。
		// 一次都没观测过，说不出这段时间漏没漏 —— 而 UNKNOWN 正是
		// 「证据本身取不到」那一档（flow-ingestion spec §2）。
		completeness: flow.CompletenessUnknown,
		intervals:    intervals,
	}
	for _, ivs := range intervals {
		for _, iv := range ivs {
			if iv.Covers(anchor) {
				d.living = append(d.living, iv)
			}
		}
	}
	return d, nil
}

// describeAt 解析一段**指定**窗口的全部事实。
//
// 与 describe 拆开，是因为 Flows / Security 的接口上带着调用方选定的窗口，
// 而 Topology / Quality 没有、只能取最近一次观测。两者之后的每一步必须完全
// 一致：窗口从哪来是调用方的事，"这一段时间里是什么样"只能有一份算法，
// 各写一份会让同一个集群在概览页与列表页给出两套主体。
//
// 调用方必须已经确认过集群登记为 COLLECTED —— 本函数不再查一次注册表，
// 于是"门禁"只有一个位置，不会出现一条绕过它的路径。
func (r *Reader) describeAt(
	ctx context.Context, clusterID string, window flow.Window,
) (described, error) {
	at := window.From

	// 保留水位排在最前，**在读连接之前**：读完再判的话，一个被清理干净的
	// 窗口会先产出一份零条的结果，而任何一处忘了检查这个错误的调用方
	// 都会把它当成"这段时间没有流量"用下去。
	if err := r.refuseIfPurged(ctx, clusterID, window); err != nil {
		return described{}, err
	}

	anchor, err := r.assetAnchor(ctx, clusterID, at)
	if err != nil {
		return described{}, err
	}

	wf, err := r.facts.FlowWindow(ctx, clusterID, window)
	if err != nil {
		// 事实层自己就区分"没摄入过"与"摄入过、没有连接"，这里只是把它的
		// 那个哨兵翻译成本包的同一句话，不把它降级成一份空结果。
		if errors.Is(err, snapshotstore.ErrNoIngestWindow) {
			return described{}, fmt.Errorf("%w: cluster %s", ErrNoCollection, clusterID)
		}
		return described{}, err
	}
	conns, completeness := wf.Connections()

	intervals, err := r.readIntervals(ctx, clusterID)
	if err != nil {
		return described{}, err
	}

	d := described{
		clusterID:    clusterID,
		window:       window,
		at:           at,
		anchor:       anchor,
		conns:        conns,
		completeness: completeness,
		intervals:    intervals,
	}
	for _, ivs := range intervals {
		for _, iv := range ivs {
			if iv.Covers(at) {
				d.living = append(d.living, iv)
			}
		}
	}
	return d, nil
}

// DefaultWindow 返回这个集群最近一次摄入窗口，作为未指定 from/to 时的默认。
//
// **直接复用 describe / readLatestTraffic 已经在用的那一个取窗口的地方**
// （latestFlowWindow），不另写一条查 flow_ingest_run 的 SQL：两条各自演化的
// 查询会让「页面按最近一次摄入作答」与「默认窗口是最近一次摄入」在某次改动
// 之后指向两段不同的时间，而两边都答得出、都不报错（design doc 2026-08-18
// §3.1）。所以这里只是把那个既有取法在包外露出来。
//
// 先过来源门禁：一个登记为 FIXTURE 的集群在这里也只会拿到 ErrClusterNotFound，
// 与本包其余读方法同一条判据 —— 装配层那个 switch 被拨反时，这一道仍然成立。
//
// 一次摄入都没有时返回 ErrNoCollection（由 latestFlowWindow 给出），调用方
// 据此答「这个集群还没有可用的采集数据」。**不回退到任何别的窗口**：一段
// 没有摄入证据的时间里，"没有观测到连接"会被下游读成"这条规则没有流量、
// 可以收紧"，而那是这个平台唯一那个单向的失败方向。
func (r *Reader) DefaultWindow(ctx context.Context, clusterID string) (store.TimeWindow, error) {
	return r.DefaultWindowAt(ctx, clusterID, time.Time{})
}

// DefaultWindowAt 同 DefaultWindow，但只看 at 之前结束的那些摄入窗口。
// at 为零值时不设上界，也就是 DefaultWindow 的行为。见接口上的说明。
func (r *Reader) DefaultWindowAt(
	ctx context.Context, clusterID string, at time.Time,
) (store.TimeWindow, error) {
	if _, err := r.collectedCluster(ctx, clusterID); err != nil {
		return store.TimeWindow{}, err
	}
	w, err := r.latestFlowWindowAt(ctx, clusterID, at)
	if err != nil {
		return store.TimeWindow{}, err
	}
	return store.TimeWindow{From: w.From, To: w.To}, nil
}

// latestFlowWindow 取这个集群最近一次**真正观测到流量**的窗口。
//
// Topology 与 Quality 的接口上没有时间窗参数，而一个描述性的答案必须说得出
// 自己描述的是哪一段时间。取最近一次摄入的窗口，而不是"最近 N 小时"这类
// 相对区间：后者在采集停摆时会安静地滑出全部数据，于是页面从"最近一次观测"
// 变成"什么都没有"，而这两件事在界面上长得一样。
//
// 排除 FAILED 的摄入：那次运行根本没拿到数据，用它的窗口去查连接必然得到
// 零条，而零条会被读成"这段时间没有流量"（snapshotstore.validateIngestRun
// 拒绝无原因的失败运行，正是同一条理由的写入侧）。
func (r *Reader) latestFlowWindowAt(
	ctx context.Context, clusterID string, at time.Time,
) (flow.Window, error) {
	var w flow.Window
	// at 为零值时上界取一个永远成立的值，SQL 保持一条，不分叉成两句：
	// 两句各自演化的结果是"带上界"与"不带上界"在别的地方慢慢不一样。
	upper := at
	if upper.IsZero() {
		// 一个仍然落在 DATETIME 范围内的上界。取 time.Unix(1<<62, 0) 这类
		// 值会溢出 MySQL 的 DATETIME，整条查询直接失败 —— 而失败的是
		// "不设上界"那一支，也就是除写回之外的所有调用方。
		upper = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	err := r.db.QueryRowContext(ctx,
		`SELECT window_start, window_end
		   FROM flow_ingest_run
		  WHERE cluster_id = ? AND status <> ? AND window_end <= ?
		  ORDER BY window_start DESC, window_end DESC
		  LIMIT 1`,
		clusterID, string(snapshotstore.IngestFailed), upper.UTC()).Scan(&w.From, &w.To)
	if errors.Is(err, sql.ErrNoRows) {
		return flow.Window{}, fmt.Errorf("%w: cluster %s", ErrNoFlowIngest, clusterID)
	}
	if err != nil {
		return flow.Window{}, fmt.Errorf("collectstore: read latest flow window: %w", err)
	}
	if !w.Valid() {
		// 库里存着一个说不出边界的窗口。拒绝而不是就着它查：一个不合法的
		// 窗口在事实层那边会被拒，在这里放行只会让错误换个地方爆。
		return flow.Window{}, fmt.Errorf(
			"collectstore: cluster %s carries a flow ingest window that is not a valid interval", clusterID)
	}
	return w, nil
}

// assetAnchor 找出覆盖时刻 at 的那次采集运行的 observed_at。
//
// **不晚于 at 的最近一次**，不是最新一次（CLAUDE.md §4）：策略与 Pod 标签
// 都要用那一刻的快照解释，拿今天的快照回答昨天的问题会得出一个答得出、
// 又不报错的错误结论。
//
// 一次都没有则是 ErrNoCollection：那一刻我们根本没在看这个集群，
// 而"没在看"与"那时什么都没有"必须分开。
func (r *Reader) assetAnchor(ctx context.Context, clusterID string, at time.Time) (time.Time, error) {
	var anchor sql.NullTime
	// **判定必需的那几类资源没采回来的运行，不能当锚点**
	// （design doc 2026-08-25 §7）。
	//
	// 在这条判断之前，锚点只排除 FAILED。于是一次「策略被 403、其余照采」的
	// 运行会落成 PARTIAL 并被当成事实：策略集读出来是空的，而空策略集在
	// NetworkPolicy 语义下等于**全部放行** —— 真集群上实测的表现是所有 DENY
	// 判定凭空消失，且置信度不降级（2026-08-25）。
	//
	// 三类都必需，理由各不相同：
	//   NETWORKPOLICY  少一条 default-deny，判定直接翻转
	//   NAMESPACE      少了标签，namespaceSelector 命不中，放行被读成拦断
	//   POD            少了主体，selector 无从判断
	//
	// 回退到更早一次完整采集；没有就答 ErrNoCollection。方向是「用旧的完整
	// 快照、或者答不出」，不是「用新的残缺快照」——后者是这个平台唯一那个
	// 自信地答错的方向。
	err := r.db.QueryRowContext(ctx,
		`SELECT MAX(run.observed_at) FROM collection_run AS run
		  WHERE run.cluster_id = ? AND run.observed_at <= ? AND run.status <> ?
		    AND NOT EXISTS (
		          SELECT 1 FROM collection_run_failure AS fail
		           JOIN collection_run AS peer
		             ON peer.cluster_id = fail.cluster_id AND peer.run_id = fail.run_id
		           WHERE peer.cluster_id = run.cluster_id
		             AND peer.observed_at = run.observed_at
		             AND fail.resource IN (?, ?, ?))`,
		clusterID, at, string(snapshot.RunFailed),
		string(snapshot.ResourceNetworkPolicy),
		string(snapshot.ResourceNamespace),
		string(snapshot.ResourcePod)).Scan(&anchor)
	if err != nil {
		return time.Time{}, fmt.Errorf("collectstore: read asset anchor: %w", err)
	}
	if !anchor.Valid {
		return time.Time{}, fmt.Errorf(
			"%w: cluster %s has no collection run covering that moment", ErrNoCollection, clusterID)
	}
	return anchor.Time, nil
}

// refuseIfPurged 在窗口起点早于保留水位时拒绝作答。
//
// 判的是窗口**起点**：起点之后的部分可能还在，但那份结果描述的已经不是
// 调用方问的那段时间了，而它自己看不出来。宁可拒绝一次答得出一部分的
// 查询，也不给一份说不清覆盖了多少的答案。
//
// 从未清理过（水位未设）时什么都不做 —— 那是绝大多数集群的常态。
func (r *Reader) refuseIfPurged(
	ctx context.Context, clusterID string, window flow.Window,
) error {
	from, ok, err := r.facts.RetainedFrom(ctx, clusterID)
	if err != nil {
		return err
	}
	if !ok || !window.From.Before(from) {
		return nil
	}
	return fmt.Errorf("%w: cluster %s window starts at %s, retained from %s",
		ErrWindowPurged, clusterID,
		window.From.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339))
}
