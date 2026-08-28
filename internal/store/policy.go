package store

import (
	"context"
	"fmt"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
)

// RuleEvidence 是一条规则跨窗口累积下来的观测证据。
//
// 与 policygen.Rule.FlowCount 是两件事：那个数是**这一个窗口**里的条数，
// 每次重新生成都会变；这里的数只增不减，描述的是"我们看这条规则看了多久"。
type RuleEvidence struct {
	// FirstSeen / LastSeen 取自**窗口边界**，不是记录写入的时刻：
	// 补采一段历史应当把首次观测往前推，而不是标成"刚刚才看到"。
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// CompleteWindows 是其中完整度为 COMPLETE 的窗口数。
	//
	// **与 Windows 必须同时呈现。** 一条规则在二十个"证明不了看全"的窗口里
	// 出现过，说明的是"我们看了很多次"，不是"我们看全了"；只给总数会让前者
	// 被读成后者（spec 2026-08-25-trust-engineering §4 的 completeness 一项）。
	CompleteWindows int `json:"completeWindows"`
	// Windows 是这条规则出现过的采集窗口数。
	//
	// 与 Observations 分开保留：一个窗口里刷了十万条的规则，与十个窗口里
	// 每次都出现几条的规则，前者证据更弱 —— 一次压测就能造出前者。
	Windows int `json:"windows"`
	// Observations 是累计观测到的流量条数。
	Observations int64 `json:"observations"`
}

// PolicyPreview 是一次候选策略预览的完整产物。
//
// 四块同源返回而非拆成两个端点：拆开会让界面有机会展示"来自计算 A 的
// 策略"配"来自计算 B 的预测"。fixture 上两次计算必然一致，但接真存储后
// 窗口边界一漂就会出现策略与预测互相矛盾的一屏，而这种不一致不会报错。
type PolicyPreview struct {
	// Cluster 是目标集群。
	Cluster string `json:"cluster"`
	// Namespace 是筛选的命名空间；为空表示全集群。
	Namespace string `json:"namespace"`
	// Window 是实际生效的查询时间窗，必须回显。
	Window TimeWindow `json:"window"`
	// TrafficObserved 表示这份预演背后有没有真实观测。
	//
	// **为 false 时 Prediction 的每一项都是 0，而那个 0 不是一次评估的结果，
	// 是没有评估过。** 「会拦断 0 条连接」读起来是「可以放心下发」——
	// 这个平台最不能给出的正是那种错觉。
	//
	// 候选集本身仍然是真的：Baseline 按 workload 无条件注入，依据是资产而
	// 不是流量（policygen.Input.Pods 的说明）。因此这一屏没有流量时照样
	// 给得出「建议加哪些策略」，只是给不出「加了会拦断什么」。
	TrafficObserved bool `json:"trafficObserved"`
	// WindowCompleteness 是这段观测窗口的完整度，必须回显。
	//
	// **它决定 Prediction 的计数该怎么读，因此不能靠推断。** 窗口不是
	// COMPLETE 时，policygen 会把每一条观测判为 DEGRADED_EVIDENCE 而学不出
	// 任何放行规则（internal/policygen/aggregate.go 的 classify），于是候选集
	// 只剩 Baseline，`Counts[WOULD_BREAK]` 会逼近整个窗口的连接数。
	//
	// 那个数字**不是一次关于上线影响的预测**：它量的是"候选集为空时有多少
	// 连接会被 Baseline 拦下"。方向朝关（不会造成一次不安全的批准），但把它
	// 当成"上线会断多少条"去读会得出一个大得离谱又毫无依据的结论。
	//
	// 在这个字段之前，调用方要判断这件事只能去数
	// `Prediction.DegradedCount == TotalEvaluated` —— 那是一条**推断**，
	// 而一条前端必须自己推对的结论不是契约说出来的事实。两个 Reader 都填。
	//
	// 复用 flow.Completeness 而不是另起一套：完整度只该有一个封闭枚举，
	// 同值枚举并存迟早漂移（与 baseline.Rule.Direction 复用 replay.Direction
	// 同一条理由）。
	WindowCompleteness flow.Completeness `json:"windowCompleteness"`
	// Candidates 是候选策略。
	Candidates []policygen.CandidatePolicy `json:"candidates"`
	// Granularity 是本次预览的主体粒度，回显请求里那个值。
	//
	// 必须回显：一份不说明自己粒度的策略集，操作者无从判断屏幕上那 42 份是
	// "折叠过的"还是"这个集群只有 42 个 workload"，而两者对下一步做什么的
	// 含义完全不同（design doc 2026-08-19 §8）。
	Granularity policygen.Granularity `json:"granularity"`
	// Widening 是折叠到 namespace 粒度多放出去的量，按 namespace 一条。
	//
	// **粗化只会放宽**：一条原本只属于某个 workload 的放行，折叠之后该
	// namespace 里每个 Pod 都拿到了。这是一次刻意的取舍，但不得无声发生 ——
	// 因此它与策略同屏返回（design doc §2）。
	//
	// workload 粒度下恒为空切片（不是 nil）：空清单是"这个粒度不涉及折叠"，
	// null 是"没人算过"，两者对读的人不同。
	Widening []policygen.Widening `json:"widening"`
	// MissingBaselines 是尚未齐备的 Baseline 类型。
	//
	// 五类齐备是进入 Enforcing 的前提（spec §7.3 G3）。缺失必须与
	// 候选策略同屏，否则一份"看起来完整"的推荐会掩盖入口中断的风险。
	//
	// **这份清单装的是全部尚未齐备的类，一个都不减。** 其中哪几类是因为
	// 我们压根没看过依据，由 NotAssessedBaselines 单独标注 —— 那是一个
	// **叠加的**说明，不是一次从本清单里的摘除。理由见那个字段。
	MissingBaselines []policygen.MissingBaseline `json:"missingBaselines"`
	// NotApplicableBaselines 是那些在该 namespace 里**没有推导对象**、
	// 因而无需放行的 Baseline 类型（design doc 2026-08-18-baseline-applicability）。
	//
	// **与 MissingBaselines 互斥**，与 NotAssessedBaselines 也互斥：
	// 「看过了、这个 namespace 没有暴露面」不是「需要但没有」，也不是
	// 「没看过」。三栏各答一个问题。
	//
	// 报出来而不是让那一行凭空消失，理由与 Kinds 同源：一份空缺失与一次
	// 根本没做的校验必须区分得开。
	//
	// **恒为非 nil**，理由同 NotAssessedBaselines。
	NotApplicableBaselines []policygen.MissingBaseline `json:"notApplicableBaselines"`
	// NotAssessedBaselines 是依据资源这次采集没有拿回来、因而无从判断
	// 缺不缺的 Baseline 类型（design doc 2026-08-17 §11）。
	//
	// **它与 MissingBaselines 重叠，这是刻意的。** 一个未评估的类同时出现在
	// 两栏：缺失清单回答"还差哪几类"，本清单回答"其中哪几类是我们没看过"。
	// 操作者据此仍然分得清「没看过」与「看过了、集群里就是没有」，也仍然
	// 知道该去补 RBAC 还是补策略 —— §11 那个区分完整保留。
	//
	// **为什么是叠加而不是摘除。** 摘除会让只读 MissingBaselines 的消费方
	// 看见比实际更少的阻塞项。而依据采集一旦 403 或超时，DNS 这种要紧的类
	// 会**间歇性**地离开那份清单 —— 一个从没验证过 DNS 依据的集群于是被
	// 放行进 Enforcing，方向从"多报一条该修的"翻成"少报一条该挡的"。
	//
	// 门禁代码今天还不存在，而写它的人最自然的写法就是只读缺失清单。
	// 因此这条不能留给未来的实现者去记得 —— 它必须在数据形状上成立。
	//
	// **恒为非 nil。** 一份空清单是"五类依据我们都检查过，都在"，与
	// "这个 Reader 根本没回答过这个问题"必须能区分：前者序列化成 []，
	// 后者是 null。两个 Reader 都要说得出这同一句话。
	//
	// 不随 namespace 裁剪：它讲的是那一次采集拿回了什么，与在看哪个
	// namespace 无关 —— 与 Ungeneratable / ExcludedWorkloads 同理。
	NotAssessedBaselines []baseline.Kind `json:"notAssessedBaselines"`
	// UnattachedImports 是挂不到任何主体上、因而没有进候选集的人工导入。
	//
	// **与候选集一起返回，不省。** 一条导入进来了却没出现在候选集里，操作者
	// 会以为它生效了 —— 而它恰恰是用来补那条平台看不见的连接的（月结批处理、
	// 灾备链路），"以为补上了"比"知道没补上"危险得多，因为 dry-run 报不出
	// 这个缺口：它只评估见过的连接。
	UnattachedImports []policygen.UnattachedImport `json:"unattachedImports"`
	// UnattachedBaselines 是推导出来、却挂不到任何 workload 上的带 Subject
	// 的 Baseline 规则（今天只有 EXPOSED_INGRESS 会产生）。
	//
	// 与 UnattachedImports 同一条纪律，理由更迫切：这里描述的是集群
	// **已经真实存在**的对外暴露，不是操作者自己补的东西。MissingBaselines
	// 是 kind 粒度的——同一个 namespace 里只要有一个 Service 正常挂上了，
	// 这个 kind 就不再"缺失"，而另一个判不出主体的 Service 依然什么放行
	// 都没有，且没有任何信号（design review NC1/NC2，2026-08-28）。
	//
	// 不随 namespace 裁剪，与 UnattachedImports/Ungeneratable/
	// ExcludedWorkloads 同理。
	UnattachedBaselines []policygen.UnattachedBaselineRule `json:"unattachedBaselines"`
	// ExposureWidenings 是每一条挂上了 workload 的暴露型规则，在 Service
	// selector 与 workload podSelector 之间放宽了多少个 Pod。
	//
	// Service selector 可以点名单个 Pod（StatefulSet 的
	// statefulset.kubernetes.io/pod-name），候选策略却是 workload 粒度的——
	// 生成的规则覆盖这个 workload 的全部 Pod。不报出来，操作者读到的是
	// 「按 Service 放行」，实际下发的是「按 workload 放行」。
	//
	// 不随 namespace 裁剪，与 UnattachedBaselines 同理；namespace 折叠也不
	// 改变某个 Service 当初点没点名单个 Pod，因此两种粒度下取的是同一份。
	//
	// **恒为非 nil**，理由同 UnattachedBaselines：空清单是"算过，没有一条
	// 放宽"，null 是"没人算过"。
	ExposureWidenings []policygen.ExposureWidening `json:"exposureWidenings"`
	// UnobservedRules 是从累积证据并进来、但**本次求值窗口内没有出现**的规则。
	//
	// 单独一栏而不是混进候选集：dry-run 的四类计数报不出它们——那几个数比较
	// 的是观测到的流量在两套策略下的判定，而这些规则放行的流量本窗口里根本
	// 没出现，不产生任何 change kind。于是策略集放行的比本窗口证据支持的多，
	// 而没有一个数字会动（design doc 2026-08-29 §3.4）。
	UnobservedRules []policygen.UnobservedRule `json:"unobservedRules"`
	// ExcludedNamespaces 是**整片**没有生成候选策略的命名空间。
	//
	// 今天唯一的原因是"这是 Kubernetes 内置系统命名空间"：候选集会给每个
	// workload 装上 default-deny，而一份下发到 kube-dns 的 default-deny 会让
	// 全集群失去 DNS。对最容易搞挂集群的那部分，平台默认不碰。
	//
	// **必须报出来**：一个悄悄不见的命名空间，在界面上与"它没有 workload"
	// 长得一样，而操作者据此以为覆盖是完整的。
	ExcludedNamespaces []policygen.ExcludedNamespace `json:"excludedNamespaces"`
	// Ungeneratable 是无法表达为规则的流量。
	Ungeneratable []policygen.UngeneratableItem `json:"ungeneratable"`
	// ExcludedWorkloads 是从未进入候选策略花名册的 Pod（hostNetwork、
	// 无可识别 workload 标签），与 Ungeneratable 同理不受 namespace
	// 过滤影响：一个从未进入名册的 Pod 在哪个 namespace 视图下都同样
	// 缺失，按视图裁剪会让这个缺口随筛选条件时隐时现。
	ExcludedWorkloads []policygen.ExcludedWorkload `json:"excludedWorkloads"`
	// Prediction 是 dry-run 预测结果，**只跑候选集、不含集群已有策略**。
	//
	// 它回答的是"如果把旧策略也清理掉会怎样"，那是接管路线的终点。
	// 要看合并之后的真实影响，读 PredictionWithExisting。
	Prediction predict.Report `json:"prediction"`
	// PredictionWithExisting 是**集群已有策略 ∪ 候选集**的预测，也就是
	// "合并这个 PR 之后实际会拦断什么"（design doc
	// 2026-08-25-existing-policies §3）。
	//
	// **这一份才是操作者点下去会发生的事。** 平台的下发方式是只加不删：
	// 写回把候选写进仓库，GitOps 把它们 apply 进集群，而集群里原有的策略
	// 一条都不会因此消失。只跑候选集那一份会把旧策略额外放行的部分算成
	// "会被拦断"，于是一份实际无害的写回看起来要打断几十条连接 —— 而反复
	// 出现的假警报，最终会让真的那次也没人看。
	//
	// 两份并列而不是替换：两者的差额就是"旧策略额外放行了多少"，
	// 差额大说明旧策略比新候选集宽得多，值得进入清理流程。
	PredictionWithExisting predict.Report `json:"predictionWithExisting"`
	// Kinds 是必备 Baseline 的全集，随报告返回。
	//
	// 与 RiskPortCatalog 同理：缺失清单为空时，使用者必须能看到
	// "我们检查了哪五类"，否则一份空缺失与一次根本没做的校验无法区分。
	Kinds []baseline.Kind `json:"baselineKinds"`
	// Overrides 是当前生效的人工决定。
	Overrides []registry.RuleOverride `json:"overrides"`
	// StaleOverrides 是已失效的确认。
	//
	// 单独报出而不是静默丢弃：只说「已失效」等于告诉人「你上周的
	// 工作没了，自己去查」。
	StaleOverrides []policygen.StaleOverride `json:"staleOverrides"`
	// Evidence 是每条候选规则跨窗口累积的观测证据，按规则指纹索引。
	//
	// **候选集是现算的，它只描述当前这一个窗口。** 一条规则在屏幕上写着
	// 「12 条流量」，读的人无从知道这是「观察了三周，一直如此」还是
	// 「刚才那一个小时里第一次出现」—— 而这两者能不能下发，结论相反。
	//
	// **为 nil 表示没有记录过证据**（fixture 集群、或这个集群从未跑过采集），
	// 不是「证据为零」。空 map 才是后者。界面必须把两者分开渲染，理由与
	// TrafficObserved 相同：一个读起来像「证据不足」的空白，实际含义是
	// 「我们没在记」。
	//
	// **证据不解锁任何门禁。** 它只回答「看了多久」，不回答「看全了没有」——
	// 一条规则在一个始终不完整的窗口里被观测一百次，仍然可能漏掉真正会被
	// 拦断的那条连接。放行与否照旧由学习窗与一致率两道门决定
	// （design doc 2026-08-25-trust-engineering §P1）。
	Evidence map[string]RuleEvidence `json:"evidence"`
	// Overridden 是应用人工决定之后的版本。
	//
	// 嵌套而非平铺同名字段：前端拿到两个结构相同的块，并列视图用
	// 同一个组件渲染，不会出现「哪个字段属于哪一套」的混淆。
	Overridden OverriddenView `json:"overridden"`
}

// OverriddenPrediction 返回应用人工决定之后、**只跑候选集**的那一份预测。
//
// 没有人工确认时 Overridden.Prediction 缺席，含义是「与顶层那一份恒等」，
// 因此回落到 Prediction。**取数方必须走这两个方法，不得直接点字段** ——
// 直接点会在没有覆盖时解引用一个 nil，而那两条路径正是导出与写回。
func (p PolicyPreview) OverriddenPrediction() predict.Report {
	if p.Overridden.Prediction != nil {
		return *p.Overridden.Prediction
	}
	return p.Prediction
}

// OverriddenPredictionWithExisting 返回应用人工决定之后、并入集群已有策略的
// 那一份预测 —— **写回的提交信息与导出的注释头取的就是它**。
//
// 缺席时回落到顶层的 PredictionWithExisting，理由同 OverriddenPrediction。
func (p PolicyPreview) OverriddenPredictionWithExisting() predict.Report {
	if p.Overridden.PredictionWithExisting != nil {
		return *p.Overridden.PredictionWithExisting
	}
	return p.PredictionWithExisting
}

// OverriddenView 是应用人工决定之后的候选策略与预测。
type OverriddenView struct {
	// Candidates 是应用覆盖后的候选策略。
	Candidates []policygen.CandidatePolicy `json:"candidates"`
	// Prediction 是应用覆盖后的四类变化，**只跑候选集**。
	//
	// **没有任何人工确认时为 nil、且不出现在 JSON 里。** 那时它与顶层的
	// Prediction 恒等（policygen.Apply 对空覆盖只是深拷贝），照旧算一遍再
	// 序列化一遍，等于把一次预览的预测量与响应体都翻倍 —— UAT 实测一份预览
	// 15.4 MB，其中 46% 正是这两份重复的明细，而前端在没有覆盖时一条都不读。
	// 与本结构体里 Enabled 标 json:"-" 是同一条理由。
	//
	// **缺席的含义是「与基础份恒等」，不是「零拦断」**，因此是指针而不是
	// 零值：一份全零的预测读起来是「应用它什么都不会断」，那是这一屏最不能
	// 给出的错觉。取数方必须显式回落到顶层那一份。
	Prediction *predict.Report `json:"prediction,omitempty"`
	// PredictionWithExisting 是应用覆盖后、并入集群已有策略的四类变化。
	//
	// **写回的提交信息与文件注释头取这一份**：它是合并之后的真实影响，
	// 而评审人读到的就是那几个数字。取只跑候选集的那一份，会把旧策略额外
	// 放行的部分算成"会被拦断"，让一次实际无害的写回看起来要断几十条连接
	// —— 而反复出现的假警报，最终会让真的那次也没人看。
	// 与 Prediction 同：没有人工确认时为 nil，含义是「与顶层那一份恒等」。
	PredictionWithExisting *predict.Report `json:"predictionWithExisting,omitempty"`
	// Enabled 是 Candidates 里启用规则渲染成的 NetworkPolicy，供导出使用。
	//
	// 挂在这里而不是让导出端点自己再生成一次，是导出这件事唯一真正的风险
	// 所在（design doc 2026-08-14 §2）：第二条生成路径会产出一份与
	// Prediction 不对应的策略集，而操作者应用的是文件、看过的是数字，
	// 两者对不上时屏幕上没有任何迹象。同一个结构体里带出来，导出与预测
	// 就是同一次计算的两种呈现。
	//
	// 由 Candidates 渲染而来，不是由未裁剪的 Result 渲染：屏幕上那一份
	// 候选集经过 namespace 裁剪，文件必须与它逐条对应。
	//
	// json:"-"：它与 Candidates 是同一份内容的两种形状，序列化出去只会让
	// 预览响应翻倍，也给前端制造了第二个可选的取数来源。
	Enabled []networkingv1.NetworkPolicy `json:"-"`
}

// candidateSet 是重新生成一次候选策略集所需的全部中间产物。
//
// PolicyPreview 与 EnsureRuleExists 共用同一个 generate 函数，而不是
// 各自拼装生成输入：两个端点都要回答"当前候选集长什么样"，分别拼装
// 只要有一处漂移（比如漏传 Pods、漏过滤某类流量），两个端点就会对着
// 不同的候选集给出互相矛盾的答案。
type candidateSet struct {
	cluster      fixture.Cluster
	observations []policygen.Observation
	result       policygen.Result
}

// generate 重新计算一次候选策略集。集群未注册或时间窗无效时返回错误。
func (r *FixtureReader) generate(
	ctx context.Context, clusterID string, window TimeWindow,
) (candidateSet, error) {
	if !window.Valid() {
		return candidateSet{}, ErrWindowRequired
	}
	c, ok := r.fleet.Cluster(clusterID)
	if !ok {
		return candidateSet{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}
	reg, ok, err := r.registeredCluster(ctx, clusterID)
	if err != nil {
		return candidateSet{}, err
	}
	if !ok {
		return candidateSet{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}

	// 集群元数据来自注册信息，其余快照仍来自 fixture：Services、
	// Endpoints、Gateways、ScrapeTargets、NodeAgents 属于采集层的职责，
	// 不在本轮迁移范围内。
	assets := c.Assets
	assets.Registry = reg.ToSnapshot()
	assets.APIServers = reg.APIServerSnapshots()

	flows, err := r.visibleFlows(ctx)
	if err != nil {
		return candidateSet{}, err
	}
	obs := make([]policygen.Observation, 0, len(flows))
	for _, f := range flows {
		if !window.Contains(f.Flow.Timestamp) || !involvesCluster(f.Flow, clusterID) {
			continue
		}
		d := r.decide(f)
		obs = append(obs, policygen.Observation{
			FlowID: f.ID, Flow: f.Flow, Decision: d,
			// 合成数据集没有窗口完整度这回事，因此求值引擎自己的可信度
			// 就是身份可信度：mesh / CCNP 降级的那些仍然一条都学不到
			// （design doc 2026-08-18-learn-from-incomplete-evidence §5）。
			IdentityTrusted: d.Confidence == replay.ConfidenceTrusted,
		})
	}

	// 生成一律跑整个集群，namespace 只在调用方裁剪展示范围。
	//
	// 若把 namespace 传进生成器，预测就会拿到全量流量配一份被裁剪过的
	// 策略集：目的地在其他 namespace 的流量因为对应策略被滤掉而落到
	// ALLOW，凭空造出 WOULD_OPEN，同时 WOULD_BREAK 被低估 —— 两个方向
	// 同时错，且都朝着让人放心的方向（spec §5）。
	// 导入对演示集群同样生效，与人工覆盖同一条理由：登记在注册表里的决定
	// 不该因为数据来源是合成的就被忽略 —— 那会让这条路在 demo 上演示不出来，
	// 而演示正是它被理解的方式。
	imports, err := r.candidateImports(ctx, clusterID)
	if err != nil {
		return candidateSet{}, err
	}

	gen := policygen.Generate(policygen.Input{
		ClusterID: clusterID,
		Assets:    assets, Namespaces: c.Namespaces,
		// Pods 必须传入：候选策略按 workload 花名册生成而非按流量生成，
		// 缺了它，流量全 DEGRADED（mesh 内）或全 UNKNOWN（策略写坏）的
		// workload 会从候选集里悄悄消失，连带绕过它们的强制 Baseline 注入。
		Pods:         c.Pods,
		Observations: obs,
		Imports:      imports,
	})

	return candidateSet{cluster: c, observations: obs, result: gen}, nil
}

// PolicyPreview 生成候选策略并回放预测。集群或命名空间不存在时返回错误。
func (r *FixtureReader) PolicyPreview(
	ctx context.Context, clusterID, namespace string, window TimeWindow,
) (PolicyPreview, error) {
	return r.PolicyPreviewAtGranularity(
		ctx, clusterID, namespace, window, policygen.GranularityWorkload)
}

// PolicyPreviewAtGranularity 同 PolicyPreview，但指定主体粒度。
//
// 折叠与两次预测都在这里发生：策略与预测必须同粒度，一份 namespace 粒度的
// 策略集配上 workload 粒度的 WOULD_BREAK 描述的是另一套策略
// （design doc 2026-08-19 §3）。
func (r *FixtureReader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, window TimeWindow,
	granularity policygen.Granularity,
) (PolicyPreview, error) {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return PolicyPreview{}, err
	}
	c, gen := cs.cluster, cs.result
	if namespace != "" && !hasNamespace(c.Namespaces, namespace) {
		return PolicyPreview{}, fmt.Errorf("%w: %s/%s", ErrNamespaceNotFound, clusterID, namespace)
	}

	stored, err := r.source.RuleOverrides(ctx, clusterID)
	if err != nil {
		return PolicyPreview{}, err
	}
	pgOverrides := make([]policygen.Override, 0, len(stored))
	for _, o := range stored {
		pgOverrides = append(pgOverrides, o.ToPolicygen())
	}
	// Apply 建在同一次 Generate 的输出上，两套预测因此必然可比 ——
	// 这是结构性保证，不是约定。
	overridden, stale := policygen.Apply(gen, pgOverrides)

	// **覆盖先于折叠**：确认记在 workload 粒度，折叠取的是确认之后仍然启用
	// 的那个集合（design doc §4）。折叠先于预测：预测跑的必须是屏幕上那一套。
	widening := []policygen.Widening{}
	if granularity == policygen.GranularityNamespace {
		gen, _ = gen.AtNamespaceGranularity()
		overridden, widening = overridden.AtNamespaceGranularity()
	}

	// 一处装配，四次调用：四份预测只在策略集这一维上不同，其余每一项都
	// 必须逐字相同 —— 差在别处的话，两份报告之间的差额就不再是"策略集不同"
	// 造成的，而那正是这几个数字唯一要表达的东西。
	run := func(policies []networkingv1.NetworkPolicy) predict.Report {
		return predict.Run(predict.Input{
			ClusterID:  clusterID,
			Policies:   policies,
			Namespaces: c.Namespaces,
			// 合成数据集只有 CCNP 这一个第二平面的概念，没有真集群的
			// ANP —— 这里给的就是它的全部求值上下文。
			EvalOptions:  []replay.Option{replay.WithForeignPlane(c.CCNPPresent)},
			Observations: cs.observations,
			// 展示名复用流量列表那一套，两个界面必须用同一个名字指同一个 Pod。
			Label: endpointLabel,
		})
	}
	enabled := gen.EnabledPolicies()
	report := run(enabled)
	// 合成数据集的"集群已有策略"就是它自己那份 Policies —— 与判定用的是
	// 同一批（decide 走的正是 c.Policies 构造的求值器）。
	reportWithExisting := run(predict.WithExisting(c.Policies, enabled))

	// 没有人工确认时不算、也不带：与上面两份恒等（见 OverriddenView.Prediction）。
	// 两个 Reader 在这一点上必须一致 —— 前端只有一套取数逻辑。
	var overriddenReport, overriddenWithExisting *predict.Report
	if len(pgOverrides) > 0 {
		overriddenEnabled := overridden.EnabledPolicies()
		a := run(overriddenEnabled)
		b := run(predict.WithExisting(c.Policies, overriddenEnabled))
		overriddenReport, overriddenWithExisting = &a, &b
	}

	// 一份裁剪结果，两处使用：屏幕上的候选集与导出的文件必须是同一个
	// 切片渲染出来的，各裁一次就又有了两个可以互相分歧的选择点。
	overriddenCandidates := FilterCandidates(overridden.Policies, namespace)

	return PolicyPreview{
		// fixture 数据集自带合成流量，因此这一份预演确实评估过
		// （design doc 2026-08-18）。**显式写 true，不靠零值**：零值是 false，
		// 而 false 的含义是"没有观测过" —— 演示集群会因此被挂上一条本不该
		// 有的警告，而它的 dry-run 数字是真算过的。
		TrafficObserved: true,
		Cluster:         clusterID, Namespace: namespace, Window: window,
		Granularity: effectiveGranularity(granularity),
		Widening:    widening,
		// 合成数据集不是一次观测，没有采样、没有丢弃、没有覆盖不满的窗口 ——
		// 它就是完整的。填 COMPLETE 而不是留空：空值不在 flow.Completeness
		// 的封闭枚举里，前端拿到它只能猜，而这个字段存在的理由正是不让人猜。
		WindowCompleteness: flow.CompletenessComplete,
		Candidates:         FilterCandidates(gen.Policies, namespace),
		MissingBaselines:   FilterMissing(gen.MissingBaselines, namespace),
		// 与缺失清单同样按 namespace 裁剪展示：两栏并排读，一栏跟着筛选走、
		// 另一栏不跟，会让人以为别的 namespace 也不适用。
		NotApplicableBaselines: nonNilMissing(FilterMissing(gen.NotApplicableBaselines, namespace)),
		// 合成数据集把五类依据都带齐了（Services / Endpoints / Gateways /
		// ScrapeTargets / NodeAgents 都在 fixture.Cluster.Assets 里），因此
		// 这里恒为空。**非 nil**：空清单要读作"五类都检查过、都在"，而不是
		// 读作"这个 Reader 没回答"（见字段说明）。
		NotAssessedBaselines:   []baseline.Kind{},
		Ungeneratable:          gen.Ungeneratable,
		UnattachedImports:      gen.UnattachedImports,
		UnattachedBaselines:    gen.UnattachedBaselines,
		ExposureWidenings:      gen.ExposureWidenings,
		ExcludedNamespaces:     gen.ExcludedNamespaces,
		ExcludedWorkloads:      gen.ExcludedWorkloads,
		Prediction:             report,
		PredictionWithExisting: reportWithExisting,
		Kinds:                  baseline.AllKinds(),
		Overrides:              stored,
		StaleOverrides:         stale,
		Overridden: OverriddenView{
			Candidates:             overriddenCandidates,
			Prediction:             overriddenReport,
			PredictionWithExisting: overriddenWithExisting,
			// 复用 EnabledPolicies 而不是另写一段渲染：「哪些规则算启用」
			// 只能有一个定义，预测跑的正是这个函数的输出。
			Enabled: policygen.Result{Policies: overriddenCandidates}.EnabledPolicies(),
		},
	}, nil
}

// EnsureRuleExists 校验一条即将落库的人工决定在当前候选集里仍然成立。
//
// 指纹对不上候选集中 (namespace, workload) 下任何一条规则时，返回
// registry.NewInvalidError：调用方拿着一个过期页面提交，写进去的覆盖
// 不会报错，只会永远待在「已失效」那一节，而它从来就没生效过。
//
// 指纹对上了，但目标规则是 BASELINE 来源且决定是 DISABLE 时，返回
// policygen.ErrBaselineNotDisablable：policygen.Apply 面对同一种输入
// 本就会把它判定为失效（见 override.go 的 staleBaselineProtected），
// 这里只是把同一个必然结论挪到写库前，好过写进去再显示"从未生效"。
func (r *FixtureReader) EnsureRuleExists(
	ctx context.Context, clusterID, namespace, workload, fingerprint string,
	decision policygen.OverrideDecision, window TimeWindow,
) error {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return err
	}
	for _, p := range cs.result.Policies {
		if p.Namespace != namespace || p.Workload != workload {
			continue
		}
		for _, rule := range p.Rules {
			if rule.Fingerprint != fingerprint {
				continue
			}
			if rule.Origin == policygen.OriginBaseline && decision == policygen.DecisionDisable {
				return policygen.ErrBaselineNotDisablable
			}
			return nil
		}
	}
	return registry.NewInvalidError("指纹与当前候选规则不匹配，页面可能已过期")
}

// hasNamespace 判断命名空间是否存在于该集群的快照里。
func hasNamespace(nss []replay.NamespaceRef, name string) bool {
	for _, ns := range nss {
		if ns.Name == name {
			return true
		}
	}
	return false
}

// FilterCandidates 按 namespace 裁剪候选策略的展示范围；空表示全集群。
//
// 导出给别的 Reader 用，而不是各自再写一份：生成恒为全集群、namespace
// 只裁展示，这条规则若在两个 Reader 里各写一次，其中一次哪天顺手把
// namespace 传进生成器，预测就会拿全量流量配一份被裁剪过的策略集 ——
// 凭空造出 WOULD_OPEN，同时低估 WOULD_BREAK（spec §5）。
func FilterCandidates(in []policygen.CandidatePolicy, namespace string) []policygen.CandidatePolicy {
	if namespace == "" {
		return in
	}
	out := make([]policygen.CandidatePolicy, 0, len(in))
	for _, p := range in {
		if p.Namespace == namespace {
			out = append(out, p)
		}
	}
	return out
}

// nonNilMissing 保证一份清单序列化成 [] 而不是 null。
//
// 空清单是一句话（"没有哪一类是不适用的"），null 是"这个 Reader 没回答过"。
// 两者对读的人完全不同，理由与 NotAssessedBaselines 同源。
func nonNilMissing(in []policygen.MissingBaseline) []policygen.MissingBaseline {
	if in == nil {
		return []policygen.MissingBaseline{}
	}
	return in
}

// FilterMissing 按 namespace 裁剪缺失清单的展示范围；空表示全集群。
//
// 与候选策略一样只裁展示：缺失是按整个集群算出来的，筛选视图不该改变
// 某个 namespace 到底缺不缺什么。
func FilterMissing(in []policygen.MissingBaseline, namespace string) []policygen.MissingBaseline {
	if namespace == "" {
		return in
	}
	out := make([]policygen.MissingBaseline, 0, len(in))
	for _, m := range in {
		if m.Namespace == namespace {
			out = append(out, m)
		}
	}
	return out
}

// effectiveGranularity 把未登记的取值收敛到 WORKLOAD。
//
// 失败方向朝窄（安全规范 §49）：WORKLOAD 是现状、也是更精确的那一侧。
// 落到 NAMESPACE 会把一份本该只选中一个 workload 的策略变成选中整个命名
// 空间，而那个方向不该靠一个零值走到。
//
// 两个 Reader 各有一份同名函数，是刻意的：它们分属两个包，而这条收敛规则
// 是各自边界上的判断。合成一份要么让 store 依赖 collectstore（反了），
// 要么再开一个包放三行代码。
func effectiveGranularity(g policygen.Granularity) policygen.Granularity {
	if g == policygen.GranularityNamespace {
		return policygen.GranularityNamespace
	}
	return policygen.GranularityWorkload
}

// DeletionImpactReport 是「把某几份 NetworkPolicy 从集群里移除」的预测结论
// （design doc 2026-08-24 §4.3）。
//
// 删除是这个平台能造成的最大伤害的那一类变更：删掉一条策略，那一片从「有规则」
// 变回默认放行，或者反过来把一条 default-deny 撤掉之后再没有东西拦。因此它
// 必须和新增走同一条求值路径、产出同一套四类计数，而不是一句「删了应该没事」。
type DeletionImpactReport struct {
	// TrafficObserved 表示这段窗口里到底有没有观测。
	//
	// 为 false 时 Counts 全是 0，而那个 0 不是评估结果，是没有评估过 ——
	// 调用方据此**不提供删除入口**，不是显示一个让人放心的零。
	TrafficObserved bool `json:"trafficObserved"`
	// Window 是实际生效的窗口，必须回显。
	Window TimeWindow `json:"window"`
	// Counts 是移除之后的四类计数。
	Counts map[predict.ChangeKind]int `json:"counts"`
	// Removed 是被问到的策略份数。
	Removed int `json:"removed"`
	// Live 是其中在**最近一次采集**里仍然存在于集群的份数。
	//
	// 回答的是「现在还在不在」。删除是一个此刻要做的决定，因此这一问必须用
	// 现状回答，而不是用观测窗口那一刻的快照 —— 一条在窗口之后才被下发的
	// 策略，用窗口口径去看是「集群里没有」，据此标成「删掉没影响」，
	// 而它其实正在生效（2026-08-24 实测发现）。
	Live int `json:"live"`
	// InWindow 是其中出现在**观测窗口锚点**策略集里的份数。
	//
	// 回答的是「算不算得出删除影响」。Counts 是拿窗口内的观测流量重放出来的，
	// 只有当这些策略在那一刻就已经存在，删掉它们的影响才有意义；否则重放的
	// 是一次「删掉一个当时并不存在的东西」，结果恒为「无变化」。
	//
	// **与 Live 分开两个字段，不合并**：它们回答两个不同的问题，合并之后
	// 「现在有、但那时没有」这种情形只能落到其中一边，而它恰恰是唯一需要
	// 平台说「我算不出来」的情形。
	InWindow int `json:"inWindow"`
}

// DeletionImpact 预测把 removed 这批策略从集群里移除会发生什么。
//
// 与候选集预测走同一条路径（predict.Run），只是策略集取的是「集群当前策略集
// 减去 removed」：删除是策略集的一次变更，另写一套判定就又多了一个两份结论
// 可以分歧的位置。
func (r *FixtureReader) DeletionImpact(
	ctx context.Context, clusterID string, window TimeWindow,
	removed []networkingv1.NetworkPolicy,
) (DeletionImpactReport, error) {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return DeletionImpactReport{}, err
	}
	// 合成数据集只有一份静态快照，「现在」与「窗口那一刻」是同一份 ——
	// 因此两个计数取同一个值。这不是偷懒：fixture 集群本来就没有时间维度，
	// 把它伪造出两个不同的值才是编造。
	kept, present := WithoutPolicies(cs.cluster.Policies, removed)
	report := predict.Run(predict.Input{
		ClusterID:    clusterID,
		Policies:     kept,
		Namespaces:   cs.cluster.Namespaces,
		EvalOptions:  []replay.Option{replay.WithForeignPlane(cs.cluster.CCNPPresent)},
		Observations: cs.observations,
		Label:        endpointLabel,
	})
	return DeletionImpactReport{
		// 合成数据集自带流量，与 PolicyPreview 里那条注释同源。
		TrafficObserved: true,
		Window:          window,
		Counts:          report.Counts,
		Removed:         len(removed),
		Live:            present,
		InWindow:        present,
	}, nil
}

// WithoutPolicies 返回 base 去掉 removed 之后的策略集，以及 removed 里确实
// 出现在 base 中的份数。
//
// 导出而不是各 Reader 各写一份：两个来源的删除影响必须按同一条匹配规则算，
// 各写一次就给了「一边比内容、一边比名字」一个位置，而那会让同一次删除在
// 两种集群上得出不同的结论。
//
// 按 (namespace, name) 匹配，不比内容：Config Sync 删掉的是那个对象，
// 而仓库里那份与集群里那份可能已经不同 —— 比内容会让一份被人手工改过的
// 策略变成「集群里没有」，于是删除影响被算成零。
func WithoutPolicies(
	base, removed []networkingv1.NetworkPolicy,
) ([]networkingv1.NetworkPolicy, int) {
	drop := make(map[[2]string]struct{}, len(removed))
	for _, p := range removed {
		drop[[2]string{p.Namespace, p.Name}] = struct{}{}
	}
	kept := make([]networkingv1.NetworkPolicy, 0, len(base))
	hit := map[[2]string]struct{}{}
	for _, p := range base {
		key := [2]string{p.Namespace, p.Name}
		if _, gone := drop[key]; gone {
			hit[key] = struct{}{}
			continue
		}
		kept = append(kept, p)
	}
	return kept, len(hit)
}

// LivePolicies 返回合成数据集里这个集群的策略。
//
// fixture 只有一份静态快照，"最近一次采集"与"任何时刻"是同一份 ——
// 与 DeletionImpact 里两个计数取同一个值同源。
func (r *FixtureReader) LivePolicies(
	ctx context.Context, clusterID string,
) ([]networkingv1.NetworkPolicy, error) {
	// 门禁与其它读方法一致：集群必须既在 fleet 里、也在注册表里 ——
	// 一个没登记的集群 ID 的正确答案是「没有这个集群」，不是一份空清单。
	c, ok := r.fleet.Cluster(clusterID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}
	if _, ok, err := r.registeredCluster(ctx, clusterID); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}
	out := make([]networkingv1.NetworkPolicy, len(c.Policies))
	copy(out, c.Policies)
	return out, nil
}

// ReconciliationReport 是一次对账的结论，连同它的口径。
//
// Window 与 SourceReportsVerdicts 必须回显：一个 0.98 的一致率，在"来源根本
// 不报判定、只有 3 条可比对连接"的情况下毫无意义。指标与它的口径必须一起走，
// 否则它会被单独截图放进汇报里。
type ReconciliationReport struct {
	// Cluster 是目标集群。
	Cluster string `json:"cluster"`
	// Window 是实际生效的查询时间窗。
	Window TimeWindow `json:"window"`
	// SourceReportsVerdicts 表示这段观测的来源到底报不报判定。
	//
	// 为 false 时整份报告只有 SOURCE_SILENT —— 那不是"平台全错"，是
	// "这条接入方式对不了账"（NODE_CONNTRACK 恒为此）。
	SourceReportsVerdicts bool `json:"sourceReportsVerdicts"`
	// Report 是分类计数与按 workload 的聚合。
	Report reconcile.Report `json:"report"`
	// Samples 是两类分歧的抽样证据，渲染成可读形式。
	//
	// **与 reconcile.Sample 分开**：那个类型带的是 replay.Flow，而 k8s 与
	// 引擎的内部结构一旦进了响应体，界面就得自己解释端点语义 —— 与
	// policygen.Rule 只出 Peers/Ports 视图是同一条纪律。
	//
	// 门禁按分歧率拦人，而一个只有比率的界面给不出下一步：操作者要看的是
	// 哪几条连接对不上，才能判断平台漏了什么（多半是它不解释的另一个策略
	// 平面）。
	Samples []ReconciliationSample `json:"samples"`
}

// ReconciliationSample 是一条分歧证据的展示视图。
type ReconciliationSample struct {
	Subject reconcile.Subject `json:"subject"`
	Class   reconcile.Class   `json:"class"`
	// Source 与 Dest 是渲染好的端点：解析得出身份时是 namespace/name，
	// 否则是 IP。
	//
	// **两者必须可区分**：一条对端是 IP 的分歧，与一条对端是具体 Pod 的
	// 分歧，排查方向完全不同 —— 前者多半是外部地址或身份解析没跟上，
	// 后者才是策略语义的问题。
	Source string `json:"source"`
	Dest   string `json:"dest"`
	// Port 与 Protocol 形如 5432、TCP。
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"`
	// At 是**连接发生的时刻**，不是记录写入的时刻：下钻要按它去对齐历史
	// 快照，用写入时刻会把人带到错误的那一份 Pod 名册上。
	At time.Time `json:"at"`
}

// ReconciliationSamplesOf 把纯包给出的样本渲染成展示视图。
//
// 一处渲染，两个 Reader 共用：两边各写一遍，同一条分歧在采集集群与演示
// 集群上会有两种写法，而读的人无从知道哪一种是真的。
func ReconciliationSamplesOf(samples []reconcile.Sample) []ReconciliationSample {
	out := make([]ReconciliationSample, 0, len(samples))
	for _, s := range samples {
		out = append(out, ReconciliationSample{
			Subject: s.Subject,
			Class:   s.Class,
			// 复用流量列表那一个渲染（fixturestore.endpointLabel）：同一个
			// 端点在两屏上必须是同一种写法，各写一份迟早会分家，而读的人
			// 无从知道哪一种是真的。身份解析不出来时它回落到 IP —— 那不是
			// 留空，"这一端没认出是谁"本身就是排查线索。
			Source:   endpointLabel(s.Flow.Source),
			Dest:     endpointLabel(s.Flow.Dest),
			Port:     s.Flow.Port,
			Protocol: string(s.Flow.Protocol),
			At:       s.Flow.Timestamp.UTC(),
		})
	}
	return out
}

// Reconciliation 对合成数据集答「对不了账」。
//
// **合成数据集没有执行平面。** 没有独立的第二方报判定，就没有可以对账的
// ground truth —— 拿引擎自己的输出与自己比，一致率恒为 1，那是一个纯粹
// 编出来的数字，而且是朝让人放心的方向编。
//
// 因此这里把每条观测记成 SOURCE_SILENT，与 NODE_CONNTRACK 接入同一处置：
// 不是"平台全错"，是"这条接入方式对不了账"。
func (r *FixtureReader) Reconciliation(
	ctx context.Context, clusterID string, window TimeWindow,
) (ReconciliationReport, error) {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return ReconciliationReport{}, err
	}
	obs := make([]reconcile.Observation, 0, len(cs.observations))
	for _, o := range cs.observations {
		obs = append(obs, reconcile.Observation{
			Subject:  SubjectOfEndpoint(o.Flow.Source),
			Platform: o.Decision.Verdict,
			Reported: false,
		})
	}
	rep := reconcile.Run(obs)
	return ReconciliationReport{
		Cluster: clusterID, Window: window,
		SourceReportsVerdicts: false,
		Report:                rep,
		// 恒为空数组，不是 null：合成数据集对不了账，因此不会有分歧，
		// 而"没有分歧证据"与"这一栏没人算过"在界面上必须能分开。
		Samples: ReconciliationSamplesOf(rep.Samples),
	}, nil
}

// SubjectOfEndpoint 取一条连接的源端主体。
//
// 导出而不是各 Reader 各写一份：两个来源必须按同一个主体聚合，否则
// 「一致率按 A 分组、门禁按 B 拦」这件事会在某天悄悄成立。
//
// 取源端而不是目的端：候选规则按源端 workload 生成，门禁也按它拦
// （design doc 2026-08-25 §3.4）。两处主体不一致时，一个分歧率高的 workload
// 照样能把它的推荐推出去。
//
// 解不出主体（Pod 为空、或标签里没有任何一个归属键）时落到空主体上 ——
// 它仍然进整集群计数，只是不挂在任何 workload 名下。丢掉它会让整集群的
// 一致率与各 workload 之和对不上，而对不上的数字没有人会信。
func SubjectOfEndpoint(ep replay.Endpoint) reconcile.Subject {
	if ep.Pod == nil {
		return reconcile.Subject{}
	}
	_, workload, ok := policygen.WorkloadOf(ep.Pod.Labels)
	if !ok {
		return reconcile.Subject{Namespace: ep.Pod.Namespace}
	}
	return reconcile.Subject{Namespace: ep.Pod.Namespace, Workload: workload}
}

// candidateImports 读出这个集群要补进候选集的人工导入。
//
// 与 collectstore 那一份同形、同判据（只取 CANDIDATE_ADDITION、坏记录跳过）：
// 两个 Reader 对"哪些导入算数"必须是同一个答案，否则同一条导入在演示集群与
// 采集集群上会有两种命运，而读的人无从知道哪一种是对的。
func (r *FixtureReader) candidateImports(
	ctx context.Context, clusterID string,
) ([]policygen.ImportedPolicy, error) {
	stored, err := r.source.PolicyImports(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	out := make([]policygen.ImportedPolicy, 0, len(stored))
	for _, imp := range stored {
		if imp.Role != registry.RoleCandidateAddition {
			continue
		}
		parsed, err := registry.ParseImport(imp.YAML)
		if err != nil {
			continue
		}
		out = append(out, policygen.ImportedPolicy{
			ImportID: imp.ImportID, Policy: parsed.Policy,
		})
	}
	return out, nil
}
