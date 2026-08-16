package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// CollectionReader 读一个集群最近一次采集运行的摘要。
//
// 收窄成一个方法而不是直接依赖 *snapshotstore.Store：可见面要问的只有
// "最近一次采集看见了什么"，而那个 Store 还带着整批快照的写入路径。
// 边界层拿不到写方法，就不存在"某次改动顺手让 API 写了一次快照"这条形状。
type CollectionReader interface {
	// Latest 返回最近一次采集运行的摘要；这个集群从未被采集过时
	// 返回 snapshotstore.ErrNoRun。
	Latest(ctx context.Context, clusterID string) (snapshotstore.CollectionSummary, error)
}

// collectionFailureReasons 是允许出现在响应里的失败原因，封闭枚举。
//
// 白名单而不是原样透传，是因为 collection_run_failure.reason 在库里只是
// 一列 VARCHAR(32)，封闭性只由写入侧的 Go 常量保证。一次
// `Reason: snapshot.FailureReason(err.Error())` 形状的笔误会让 apiserver
// 的原始报错落进这一列，而它短到放得下 —— 那正是这个响应绝不能带出去的
// 东西（规范 §19、§22）。透传只在取值确实是枚举成员时发生。
var collectionFailureReasons = map[string]bool{
	string(snapshot.FailureForbidden):   true,
	string(snapshot.FailureNotFound):    true,
	string(snapshot.FailureTimeout):     true,
	string(snapshot.FailureUnavailable): true,
	string(snapshot.FailureOther):       true,
}

// collectionErrorReasons 是允许出现在响应里的「这一轮没能开始」的原因。
//
// 与 collectionFailureReasons 分列而不是并成一张表：两者是两个封闭枚举，
// 合表会让一次写错列的改动（把资源失败的原因写进运行行）顺利通过校验，
// 而那正是这两列最容易被弄混的地方。
var collectionErrorReasons = map[string]bool{
	string(snapshot.RunErrorCredentialUnavailable): true,
	string(snapshot.RunErrorClientUnavailable):     true,
	string(snapshot.RunErrorReadOnlyUnproven):      true,
}

// reasonUnrecognized 是库里的取值不在封闭枚举内时交出去的原因。
//
// 不折成 OTHER：OTHER 的含义是"采集器判定不出更具体的原因"，而这里的
// 含义是"库里那个取值我们不认识"。把后者算进前者，统计口径上就再也看不出
// 有一类原因被漏登记了（CLAUDE.md §3）。真实取值只进日志。
const reasonUnrecognized = "UNRECOGNIZED"

// collectionResourceView 是一类资源在可见面上的形态：要么一个条数，
// 要么一个失败原因，不会两者都有，也不会两者都没有。
//
// 字段不导出、只由 MarshalJSON 二选一写出，与 snapshotstore.ResourceOutcome
// 同一个形状同一个理由（spec §4.2）：采集失败的资源在报文里根本没有 count
// 这个键，于是"渲染一个数字却不看失败"在前端写不出来 —— 数字不在报文里。
// 把这两个字段并排放进一个普通 struct，一次 `Count: 0` 的默认零值就会
// 把「我们没被授权看策略」显示成「这个集群没有任何策略」。
type collectionResourceView struct {
	resource string
	count    int
	// failureReason 非空表示这一类没有采到，此时 count 没有任何含义。
	failureReason string
}

// MarshalJSON 输出 count 与 failureReason 中的恰好一个。
func (v collectionResourceView) MarshalJSON() ([]byte, error) {
	if v.failureReason != "" {
		return json.Marshal(struct {
			Resource      string `json:"resource"`
			FailureReason string `json:"failureReason"`
		}{Resource: v.resource, FailureReason: v.failureReason})
	}
	return json.Marshal(struct {
		Resource string `json:"resource"`
		Count    int    `json:"count"`
	}{Resource: v.resource, Count: v.count})
}

// collectionWarningView 是一类采集告警的条数。
type collectionWarningView struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// collectionSummaryView 是 GET /clusters/{id}/collection 的响应体。
//
// 与 snapshotstore.CollectionSummary 分开而不是直接回传它，有两个理由，
// 缺一条都不够：
//
//  1. 那个类型的 FailureRecord 带着 Detail —— apiserver 的原始错误文本，
//     里面有内部主机名、内部地址与集群细节。它是给能读库的人排障用的，
//     不是给 HTTP 调用方的（规范 §19、§22）。
//  2. ObservedAt 是各 observed_* 表的 join 键，属于落库形态，调用方
//     用不到（规范 §20、§35：只回调用方需要的）。
//
// 复用领域类型会让上面两件事在某次给 CollectionSummary 加字段时自动
// 失效，而失效的方式是"多回传了一点东西"，评审里看不出来。
type collectionSummaryView struct {
	ClusterID  string    `json:"clusterId"`
	RunID      string    `json:"runId"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// Status 取值 OK / PARTIAL / FAILED。
	Status string `json:"status"`
	// ErrorReason 仅在这一轮根本没能开始采集时非空。
	//
	// 与 Resources 里的失败分列：那些说的是"某一类资源没采到"，这里说的
	// 是"这一轮从来没读到过集群"。合并会让"NetworkPolicy 被拒"与
	// "采集器连不上这个集群"在界面上落进同一句话。
	//
	// 空串用 omitempty 抹掉：正常的一轮报文里根本没有这个键，
	// 于是"渲染一个空的失败原因"在前端写不出来 —— 同 collectionResourceView。
	ErrorReason string `json:"errorReason,omitempty"`
	// Resources 是各类资源的结果，一类至多一条，顺序由读取侧决定。
	//
	// 不按枚举补齐：ResourceReplicaSet 在枚举里但从不计数（它只用于解
	// owner 链，不是被观测的资产），补齐会把它的缺席变成一条凭空捏造的
	// 失败（spec §4.2）。
	Resources []collectionResourceView `json:"resources"`
	// Warnings 是各类采集告警的条数。
	//
	// 与资源失败分列：告警说的是采到的事实与注册表登记不符，采集本身是
	// 成功的。合并会让一次成功的采集看起来像是出了故障。
	Warnings []collectionWarningView `json:"warnings"`
	// WarningTotal 是告警总条数。与逐类之和分开由读取侧查出，两者对不上
	// 说明有 kind 落在封闭枚举之外。
	WarningTotal int `json:"warningTotal"`
}

// handleCollection 返回一个集群最近一次采集运行的摘要。
//
// **这份数据不参与任何结论。** 拓扑、dry-run、候选策略、导出与写回全部
// 继续走合成数据集（spec §5.2）。真资产与合成流量拼在一起，会让 dry-run
// 对着真实 Pod 给出基于不存在的流量的阻断结论 —— 一个错误结论伪装成
// 体检报告。这条端点因此是只读的、独立的，界面上也必须把这件事说出来
// （见 web/src/pages/collectionView.ts 的 COLLECTION_FEEDS_NOTHING）。
func handleCollection(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Collection == nil {
			// 没有读取端不是"这个集群没有采集记录"：前者是本部署根本没装配
			// 采集读取端，后者是这个集群确实没被采过。两者都渲染成一张空表，
			// 一次装配缺失会看起来和一个刚注册的集群完全一样。
			//
			// 503 而非 200 + 空摘要：调用方与监控都该看见这是依赖缺失。
			response.WriteSystem(w, http.StatusServiceUnavailable, response.CodeDependencyUnavailable)
			return
		}

		// 先确认这个集群确实注册过，再去问采集记录。
		//
		// 读取端查的是 collection_run，而那张表里当然没有一个不存在的集群
		// 的行 —— 于是它对拼错的 ID 同样返回 ErrNoRun。少了这一关，一次
		// 拼写错误会答成"还没有采集记录"，操作者据此去查采集器为什么没跑。
		if !registeredCluster(w, r, d) {
			return
		}

		clusterID := chi.URLParam(r, "clusterID")
		summary, err := d.Collection.Latest(r.Context(), clusterID)
		switch {
		case errors.Is(err, snapshotstore.ErrNoRun):
			// 业务失败而不是 500：这个集群还没有被采集过是一个正常状态，
			// 计进服务错误率会让"还没接上"看起来像平台坏了。
			//
			// 用 CodeNoCollectionRun 而不是 CodeNotFound：上面那一关已经
			// 证明集群是存在的，这里说的是"存在、但没采过"。
			response.WriteBusiness(w, response.CodeNoCollectionRun)
			return
		case err != nil:
			d.Logger.Error("cannot read the latest collection run",
				"err", err, "request_id", RequestIDFrom(r.Context()), "cluster", clusterID)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}

		response.WriteOK(w, d.collectionView(r, summary))
	}
}

// collectionView 把读取侧的摘要收窄成响应体。
//
// 方法挂在 Deps 上只为拿到 logger：不认识的失败原因必须留下一条能查的
// 记录，而它的真实取值不能进响应。
func (d Deps) collectionView(r *http.Request, s snapshotstore.CollectionSummary) collectionSummaryView {
	out := collectionSummaryView{
		ClusterID:    s.ClusterID,
		RunID:        s.RunID,
		StartedAt:    s.StartedAt,
		FinishedAt:   s.FinishedAt,
		Status:       s.Status,
		ErrorReason:  d.errorReason(r, s.ErrorReason),
		Resources:    make([]collectionResourceView, 0, len(s.Resources)),
		Warnings:     make([]collectionWarningView, 0, len(s.Warnings)),
		WarningTotal: s.WarningTotal,
	}

	for _, outcome := range s.Resources {
		// 先问失败：Count() 在失败时返回的 0 没有任何含义，把它当条数用
		// 就是把"没被授权"说成"没有东西"。
		if failure, failed := outcome.Failure(); failed {
			out.Resources = append(out.Resources, collectionResourceView{
				resource:      outcome.Resource,
				failureReason: d.failureReason(r, outcome.Resource, failure.Reason),
			})
			continue
		}
		count, _ := outcome.Count()
		out.Resources = append(out.Resources, collectionResourceView{
			resource: outcome.Resource,
			count:    count,
		})
	}

	for _, w := range s.Warnings {
		out.Warnings = append(out.Warnings, collectionWarningView{Kind: w.Kind, Count: w.Count})
	}
	return out
}

// errorReason 把「这一轮没能开始」的原因收窄到封闭枚举。
//
// 空串原样放行：那表示这一轮真的读到了集群，是一个正常取值，不是一次
// 认不出来的原因。把它也换成 UNRECOGNIZED 会让每一次正常采集都在界面上
// 顶着一句"原因不明"。
func (d Deps) errorReason(r *http.Request, reason string) string {
	if reason == "" || collectionErrorReasons[reason] {
		return reason
	}
	d.Logger.Error("collection run error reason is not in the closed enum",
		"request_id", RequestIDFrom(r.Context()), "reason", reason)
	return reasonUnrecognized
}

// failureReason 把库里的原因收窄到封闭枚举。
//
// 不在枚举里的取值一律换成 reasonUnrecognized，真实取值只进日志：这一列
// 的邻座就是 apiserver 的原始错误文本，而一次写错列或写错类型的改动会让
// 那段文本落到这里（规范 §19、§22）。
func (d Deps) failureReason(r *http.Request, resource, reason string) string {
	if collectionFailureReasons[reason] {
		return reason
	}
	// 真实取值进日志而不是响应：日志是服务端的，排障的人需要看见库里到底
	// 写了什么才知道是漏登记了一个原因，还是有别的东西写进了这一列。
	d.Logger.Error("collection failure reason is not in the closed enum",
		"request_id", RequestIDFrom(r.Context()), "resource", resource, "reason", reason)
	return reasonUnrecognized
}
