// Package collectstore 用一个集群**真实采集到的**资产与流量回答读请求。
//
// 接入按"错了有多严重"分阶段（design doc 2026-08-17 §7）：Topology 与 Quality
// 只描述、不推荐，先接；Flows / Flow / Security 展示逐条判定与风险发现，其次；
// PolicyPreview 驱动推荐与 dry-run，是唯一一个错了会直接导致生产阻断建议的，
// 留到最后，在这里明确拒绝，**不返回空结果**。
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

// ErrReadNotCollectedYet 表示这个读方法还没有接到采集数据上。
//
// 分阶段接入的中间态必须是一次明确的拒绝，不是一份空结果，也不是一份
// 合成数据（design doc §7）：一个 COLLECTED 集群的读取路径上不得存在
// 任何通往 fixture 的分支，而"暂时没实现"最容易被顺手填成那条分支。
var ErrReadNotCollectedYet = errors.New("collectstore: this read is not backed by collected data yet")

// maxIntervalRows 是一次读取允许载入的身份区间条数上限。
//
// 区间表的行数与 Pod 生命周期次数同阶（migrations/000012），到期与快照
// 一起清理，因此它有上界；但那个上界取决于保留期与集群规模，而不是取决于
// 这次查询。超出即报错、**不截断**：截断会让一部分地址凭空变成"解析不出
// 主体"，于是那些连接被算进 UNKNOWN —— 一个纯粹的读取上限伪装成一次
// 关于集群的观测结论（安全规范 §24）。
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

	window, err := r.latestFlowWindow(ctx, clusterID)
	if err != nil {
		return described{}, err
	}
	return r.describeAt(ctx, clusterID, window)
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
func (r *Reader) latestFlowWindow(ctx context.Context, clusterID string) (flow.Window, error) {
	var w flow.Window
	err := r.db.QueryRowContext(ctx,
		`SELECT window_start, window_end
		   FROM flow_ingest_run
		  WHERE cluster_id = ? AND status <> ?
		  ORDER BY window_start DESC, window_end DESC
		  LIMIT 1`,
		clusterID, string(snapshotstore.IngestFailed)).Scan(&w.From, &w.To)
	if errors.Is(err, sql.ErrNoRows) {
		return flow.Window{}, fmt.Errorf("%w: cluster %s has no flow ingest", ErrNoCollection, clusterID)
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
	err := r.db.QueryRowContext(ctx,
		`SELECT MAX(observed_at) FROM collection_run
		  WHERE cluster_id = ? AND observed_at <= ? AND status <> ?`,
		clusterID, at, string(snapshot.RunFailed)).Scan(&anchor)
	if err != nil {
		return time.Time{}, fmt.Errorf("collectstore: read asset anchor: %w", err)
	}
	if !anchor.Valid {
		return time.Time{}, fmt.Errorf(
			"%w: cluster %s has no collection run covering that moment", ErrNoCollection, clusterID)
	}
	return anchor.Time, nil
}
