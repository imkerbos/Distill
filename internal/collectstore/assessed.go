package collectstore

import (
	"context"
	"fmt"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// maxResourceRows 是一次读取允许载入的资源枚举行数上限。
//
// 一次采集一类资源一行，量级是封闭枚举的大小，远够用。留一道上限是因为
// 它与其余读取同一条纪律（安全规范 §24）：越界报错、不截断 —— 截断会让
// 一类实际枚举过的资源凭空消失，于是一个采到的 Baseline 依据被报成
// "没看过"，而那是这份清单唯一不能出的错。
const maxResourceRows = 256

// baselineEvidence 登记每一类必备 Baseline 的推导依据落在哪些**被采资源**上。
//
// 这不是"哪两类采不到"的结论表 —— 结论由 collection_run_resource 里那次
// 采集**实际枚举了什么**给出（design doc §11）。这张表只回答上游那个问题：
// 某一类 Baseline 是从哪一类资源推出来的。取值逐条对得上 internal/baseline
// 的推导代码：
//
//   - DNS 取 kube-dns 的 spec.selector 与它的后端地址（derive_dns.go）；
//   - LB_HEALTH_CHECK 取入口暴露对象与它后端 Service 的 targetPort
//     （derive_infra.go），健康检查网段来自集群登记；
//   - CONTROL_PLANE 只取集群登记的 apiserver 端点，不依赖任何被采资源，
//     因此恒可评估 —— 空切片是这句话的显式写法，不是漏填；
//   - METRICS_SCRAPE 取 snapshot.ScrapeTargets，NODE_AGENT 取
//     snapshot.NodeAgents。这两类**目前采集层没有采**，因此它们的资源名
//     今天不会出现在任何一次采集的枚举清单里，两类于是落进"未评估"。
//
// 后两条的资源名直接取自 baseline 那份既有的来源枚举（SourceScrapeTarget /
// SourceNodeAgent），不另起一个字面量：两个封闭枚举指的是同一样东西，让它
// 们各写一份名字，就是给未来那次"采集层补上了、这里没跟着改"留一个位置。
// 采集层补上这两类之后，collection_run_resource 里出现同名的行，本表一个字
// 不用动，判据自动收敛到"缺失"那一支。
//
// **采集层补这两类时的前置条件**：必须同时把它们登记进 snapshot.ResourceKind
// 并加进 Observation.Counts()。只加采集、不加计数行的话，它们会永远留在
// "未评估"里，从缺失清单中消失 —— 那是不安全的那个方向。今天这两个取值是从
// baseline 的枚举转换出来的，还不是 ResourceKind 的成员。
var baselineEvidence = map[baseline.Kind][]snapshot.ResourceKind{
	baseline.KindDNS:          {snapshot.ResourceService, snapshot.ResourceEndpointSlice},
	baseline.KindLBHealth:     {snapshot.ResourceIngress, snapshot.ResourceService},
	baseline.KindControlPlane: {},
	baseline.KindMetrics:      {snapshot.ResourceKind(baseline.SourceScrapeTarget)},
	baseline.KindNodeAgent:    {snapshot.ResourceKind(baseline.SourceNodeAgent)},
}

// runEvidence 是锚点那个时刻的采集在各类资源上的结果。
//
// 两张表分开记而不是合成一个布尔：它们回答的是两个不同的问题，而把它们
// 折在一起正是上一版的缺陷 —— 见 cameBack 的说明。
type runEvidence struct {
	// enumerated 是留下了计数行的资源类型。
	enumerated map[snapshot.ResourceKind]bool
	// failed 是在 collection_run_failure 里留下失败记录的资源类型。
	failed map[snapshot.ResourceKind]bool
}

// cameBack 报告这一类资源在那次采集里**真的拿回来了**。
//
// 三种情形，只有第三种算数（design doc §11）：
//
//  1. **从来没枚举过** —— 一行计数都没有。平台压根不采这一类
//     （今天的 ScrapeTarget / NodeAgent）。
//  2. **枚举了，但那一类失败了** —— 有计数行，同时 collection_run_failure
//     里有对应的失败记录（FORBIDDEN / NOT_FOUND / TIMEOUT / UNAVAILABLE /
//     OTHER）。
//  3. **枚举了，且拿回来了** —— 有计数行、没有失败记录。计数为 0 落在这里，
//     它是一句关于集群的话：尝试过了，集群里就是没有。
//
// 第二种是上一版漏掉的那一种，而它恰恰是最常见的：`snapshotstore.insertRun`
// 无条件遍历 `snapshot.Observation.Counts()`，那是一张**固定七项**的 map
// （run.go 的 Counts），既不看 Failures 也不看这一类有没有被尝试。于是一个
// Service 被 403 挡掉的集群照样留下 `SERVICE, 0` 一行 —— 按"有没有行"判，
// 它会被当成"看过了、集群里没有 kube-dns"，页面报 **DNS 缺失**，运维去写一条
// DNS 策略，而真正要改的是 RBAC。那正是 §11 要消灭的那句话。
//
// 不按 reason 分档：这里唯一要知道的是这一类**没回来**，而"哪种失败算严重"
// 是一个我们没有依据回答的问题。FORBIDDEN 与 TIMEOUT 对这份清单是同一件事。
func (e runEvidence) cameBack(k snapshot.ResourceKind) bool {
	return e.enumerated[k] && !e.failed[k]
}

// readRunEvidence 读出锚点那个时刻的采集在各类资源上的结果。
//
// **按 observed_at 取，因此可能覆盖不止一次运行。** collection_run 的主键是
// (cluster_id, run_id)，observed_at 上只有非唯一索引，重跑与补采完全可以落在
// 同一个时刻。取并集是刻意的，但**两侧的理由不是同一条，别把它们混着记**：
//
//   - **枚举那一侧**与资产读同口径：readPodsAt / readPoliciesAt /
//     readNamespacesAt 与三条资产读都按 (cluster_id, observed_at) 取并集，
//     证据改成"挑其中一个 run_id"就会出现"资产是 A∪B、证据只描述 A"的错位。
//   - **失败那一侧是取或，因此比资产侧更保守，而不是与它同批。** 同一时刻
//     A 采到了 Service、B 的 Service 被 403 时，observed_service 里有 A 的行
//     （DNS 推得出来），而这里仍然判 SERVICE 没回来。两个视图确实不是同一批
//     —— 这是刻意选的方向，不是待修的不一致。
//
// **所以不要把它"修"成挑一个 run_id。** 在上面那个反例里，挑 run_id 有一半
// 概率挑到 A，于是一份**不完整的** Service 列表被当成一次完整观测 —— 那正是
// design doc §11 与 C 轮那条"采集失败不得被当成一次关于集群的观测"要禁的形状。
// 并集的代价是一次瞬时 TIMEOUT 会让 DNS 在那个锚点上进未评估；未评估既不
// 伪造缺失、也不减少缺失（notAssessedBaselines 的说明），所以这个代价是安全的。
//
// LEFT JOIN 而不是两趟查询：一趟就答得出"有没有计数行"与"有没有失败记录"
// 这两件事，两趟之间会多出一个两张表看到不同运行集合的位置。
//
// 只有失败记录、没有计数行的资源不会出现在结果里 —— 那种资源本来就没枚举过，
// cameBack 已经答 false，多带一行不改变任何结论。
//
// 走 (cluster_id, observed_at) 定位运行，再走 (cluster_id, run_id) 前缀取资源行
// 与失败行，三段都是主键或索引前缀，不做全表扫描（design doc §9）。
func (r *Reader) readRunEvidence(ctx context.Context, d described) (runEvidence, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT res.resource, fail.resource IS NOT NULL
		   FROM collection_run_resource AS res
		   JOIN collection_run AS run
		     ON run.cluster_id = res.cluster_id AND run.run_id = res.run_id
		   LEFT JOIN collection_run_failure AS fail
		     ON fail.cluster_id = res.cluster_id AND fail.run_id = res.run_id
		    AND fail.resource = res.resource
		  WHERE res.cluster_id = ? AND run.observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxResourceRows+1)
	if err != nil {
		return runEvidence{}, fmt.Errorf("collectstore: read run evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := runEvidence{
		enumerated: map[snapshot.ResourceKind]bool{},
		failed:     map[snapshot.ResourceKind]bool{},
	}
	// 数的是**行数**，不是去重后的种类数：同一 observed_at 上堆着多次运行时，
	// 种类数恒为那几种，用它判越界会让 LIMIT 静默截断 —— 而截断会让一类实际
	// 拿回来的资源凭空消失，于是一个采到的依据被报成"没看过"。与本包其余
	// 几处读取的判据一致。
	total := 0
	for rows.Next() {
		var (
			kind   string
			failed bool
		)
		if err := rows.Scan(&kind, &failed); err != nil {
			return runEvidence{}, fmt.Errorf("collectstore: scan run evidence: %w", err)
		}
		out.enumerated[snapshot.ResourceKind(kind)] = true
		if failed {
			out.failed[snapshot.ResourceKind(kind)] = true
		}
		total++
	}
	if err := rows.Err(); err != nil {
		return runEvidence{}, fmt.Errorf("collectstore: iterate run evidence: %w", err)
	}
	if total > maxResourceRows {
		return runEvidence{}, fmt.Errorf(
			"collectstore: cluster %s recorded more than %d resource outcomes at %s",
			d.clusterID, maxResourceRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// notAssessedBaselines 挑出依据资源这次没有拿回来的那几类 Baseline。
//
// 顺序跟 baseline.AllKinds() 的登记顺序，与缺失清单同一套口径。
//
// **这份清单不从缺失清单里减任何东西**（design doc §11）：一个未评估的类
// 照旧留在 MissingBaselines 里，本清单只补上"其中哪几类是我们没看过"。
// 减法会让只读缺失清单的消费方看见比实际更少的阻塞项，而依据采集一旦
// 403 或超时，DNS 这种要紧的类会间歇性地从那份清单里消失。
//
// 一个没有登记依据的新 Kind 会落进"可评估"那一支，于是推导不出来时照常
// 只报缺失 —— 方向朝关：宁可多报一条该修的，也不要把它藏进"我们没看过"。
func notAssessedBaselines(e runEvidence) []baseline.Kind {
	kinds := baseline.AllKinds()
	out := make([]baseline.Kind, 0, len(kinds))
	for _, k := range kinds {
		for _, resource := range baselineEvidence[k] {
			if !e.cameBack(resource) {
				out = append(out, k)
				break
			}
		}
	}
	return out
}
