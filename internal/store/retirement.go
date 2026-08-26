package store

import (
	"context"
	"fmt"
	"time"
)

// RetirementReport 回答"集群里哪些旧策略已经可以退休"
// （design doc 2026-08-25-existing-policies §6：接管模式）。
//
// **平台只报告，不删。** 它对被管集群没有、也不该有策略写权限（CLAUDE.md §3）；
// 退休一条策略要么由人做，要么走 GitOps。这份报告给出的是判断依据，
// 不是一个可以点下去的动作。
//
// 判据是"删掉它之后，观测窗口里有没有连接从通变成不通"—— 与写回的删除影响
// 同一个算法（DeletionImpact），只是逐条施加在集群现有策略上。
type RetirementReport struct {
	Cluster string     `json:"cluster"`
	Window  TimeWindow `json:"window"`
	// Eligible 表示这份报告能不能给出退休建议。
	//
	// **为 false 时 Candidates 恒为空**，而不是给一份"看起来都能退休"的清单：
	// 观测不足时算出来的"删掉没影响"只说明那段时间没看见，不说明没有。
	Eligible bool `json:"eligible"`
	// IneligibleReason 说明为什么给不出建议；Eligible 为 true 时是空串。
	IneligibleReason string `json:"ineligibleReason"`
	// Candidates 是集群里每一条**非平台所写**的策略，以及它现在还撑着多少连接。
	//
	// 逐条都列出来，不只列可退休的：一条"还撑着 12 条连接"的策略正是操作者
	// 要看的东西 —— 它告诉他候选集还差什么，而一份只有可退休项的清单读起来
	// 像"其余的平台没看过"。
	Candidates []RetirementCandidate `json:"candidates"`
	// Truncated 表示集群里的策略数超过上限，这份清单不完整。
	//
	// **不完整的清单不得被读成完整的**：漏掉的那几条会显得像"平台认为它们
	// 不存在"。
	Truncated bool `json:"truncated"`
}

// RetirementCandidate 是一条集群现有策略的退休判断。
type RetirementCandidate struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Retirable 表示删掉它不会让任何**观测到的**连接从通变成不通。
	//
	// **它不等于"删掉是安全的"。** dry-run 只评估见过的连接：一条只在月结
	// 那天走的放行，在这个窗口里看不见，删掉它下一次就会断。这正是
	// Eligible 那道学习期门槛存在的理由 —— 它把风险与一个有人签字的判断
	// 绑在一起，而不是消除风险。
	Retirable bool `json:"retirable"`
	// WouldBreak 是删掉它之后会断的连接数；Retirable 为 true 时为 0。
	WouldBreak int `json:"wouldBreak"`
	// CoveredBy 是候选集里接手了这条策略职责的主体数。
	//
	// 报出来是为了让"可以退休"有一个正面的理由：不是"删了看起来没事"，
	// 而是"这些主体的候选规则已经覆盖了它管的范围"。为 0 时说明这条策略
	// 管的主体根本不在候选集里 —— 那时即使 WouldBreak 是 0，也只是因为
	// 那些主体在这个窗口里没有流量。
	CoveredBy int `json:"coveredBy"`
}

// RetirementNotObserved 是观测不足时的说明。
const RetirementNotObserved = "这段窗口里没有任何观测流量。删除影响算不出来 —— " +
	"每一条策略都会显示成「删掉没影响」，而那个结论没有任何证据支撑。"

// RetirementCycleNotCovered 是学习期未满时的说明模板参数。
//
// 与写回那道门同源（design doc 2026-08-25 §5）：一条只在月结那天走的放行，
// 在观测窗口之外就看不见，而删掉它下一次就会断。观测没覆盖一轮业务周期时，
// 这份报告里每一条"可以退休"都建立在一段看不全的证据上。
const RetirementCycleNotCovered = "观测还没覆盖这个集群声明的业务周期，" +
	"因此给不出退休建议：窗口之外的流量不在这份计算里，而一条只在月结那天" +
	"走的放行，删掉之后要到下个月才会表现出来。"

// RetirementNoBusinessCycle 是集群没有登记业务周期时的说明。
const RetirementNoBusinessCycle = "这个集群还没有登记业务周期 —— " +
	"也就是「多久能看全一轮流量」。没有它就无从判断这段观测够不够长，" +
	"而退休一条策略正是最需要这个判断的操作。请在集群登记里填上它与判断依据。"

// ObservedLongEnough 报告这段观测有没有覆盖一个完整的业务周期。
//
// covered 是实际观测覆盖时长（不是首末跨度，见 §5 与 ObservedCoverage），
// cycle 是集群登记的业务周期。cycle 为 0 表示没有登记过。
func ObservedLongEnough(covered, cycle time.Duration) bool {
	return cycle > 0 && covered >= cycle
}

// Retirement 对合成数据集答"给不出退休建议"。
//
// **合成数据集没有真实的策略演进史。** 退休建议的说服力全部来自"这段观测
// 里删掉它不会断"，而 fixture 的观测是造出来的 —— 拿它算出来的"可以退休"
// 会让人以为平台在真集群上也能这么答。与对账那一条同一处置：说不出就说不出。
func (r *FixtureReader) Retirement(
	ctx context.Context, clusterID string, window TimeWindow,
) (RetirementReport, error) {
	// **两道门，与 generate 一致**：集群既要在合成数据集里，也要在（已被
	// 收窄成只含 FIXTURE 的）注册表里。只查第一道的话，一个登记为 COLLECTED
	// 的集群会从这里拿到一份合成数据算出来的报告 —— 而这份报告指向的动作是
	// 删掉它集群里正在生效的策略。
	if _, ok := r.fleet.Cluster(clusterID); !ok {
		return RetirementReport{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}
	_, ok, err := r.registeredCluster(ctx, clusterID)
	if err != nil {
		return RetirementReport{}, err
	}
	if !ok {
		return RetirementReport{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}
	return RetirementReport{
		Cluster: clusterID, Window: window,
		Candidates:       []RetirementCandidate{},
		IneligibleReason: RetirementFixture,
	}, nil
}

// RetirementFixture 是合成数据集的说明。
const RetirementFixture = "这是演示数据集，给不出退休建议：" +
	"退休一条策略的依据是「这段观测里删掉它不会断」，而合成观测证明不了任何" +
	"关于真实集群的事。接上采集数据的集群才会有这份报告。"
