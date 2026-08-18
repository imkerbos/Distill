package store

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
)

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
	// MissingBaselines 是尚未齐备的 Baseline 类型。
	//
	// 五类齐备是进入 Enforcing 的前提（spec §7.3 G3）。缺失必须与
	// 候选策略同屏，否则一份"看起来完整"的推荐会掩盖入口中断的风险。
	//
	// **这份清单装的是全部尚未齐备的类，一个都不减。** 其中哪几类是因为
	// 我们压根没看过依据，由 NotAssessedBaselines 单独标注 —— 那是一个
	// **叠加的**说明，不是一次从本清单里的摘除。理由见那个字段。
	MissingBaselines []policygen.MissingBaseline `json:"missingBaselines"`
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
	// Ungeneratable 是无法表达为规则的流量。
	Ungeneratable []policygen.UngeneratableItem `json:"ungeneratable"`
	// ExcludedWorkloads 是从未进入候选策略花名册的 Pod（hostNetwork、
	// 无可识别 workload 标签），与 Ungeneratable 同理不受 namespace
	// 过滤影响：一个从未进入名册的 Pod 在哪个 namespace 视图下都同样
	// 缺失，按视图裁剪会让这个缺口随筛选条件时隐时现。
	ExcludedWorkloads []policygen.ExcludedWorkload `json:"excludedWorkloads"`
	// Prediction 是 dry-run 预测结果。
	Prediction predict.Report `json:"prediction"`
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
	// Overridden 是应用人工决定之后的版本。
	//
	// 嵌套而非平铺同名字段：前端拿到两个结构相同的块，并列视图用
	// 同一个组件渲染，不会出现「哪个字段属于哪一套」的混淆。
	Overridden OverriddenView `json:"overridden"`
}

// OverriddenView 是应用人工决定之后的候选策略与预测。
type OverriddenView struct {
	// Candidates 是应用覆盖后的候选策略。
	Candidates []policygen.CandidatePolicy `json:"candidates"`
	// Prediction 是应用覆盖后的四类变化。
	Prediction predict.Report `json:"prediction"`
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
		obs = append(obs, policygen.Observation{
			FlowID: f.ID, Flow: f.Flow, Decision: r.decide(f),
		})
	}

	// 生成一律跑整个集群，namespace 只在调用方裁剪展示范围。
	//
	// 若把 namespace 传进生成器，预测就会拿到全量流量配一份被裁剪过的
	// 策略集：目的地在其他 namespace 的流量因为对应策略被滤掉而落到
	// ALLOW，凭空造出 WOULD_OPEN，同时 WOULD_BREAK 被低估 —— 两个方向
	// 同时错，且都朝着让人放心的方向（spec §5）。
	gen := policygen.Generate(policygen.Input{
		ClusterID: clusterID,
		Assets:    assets, Namespaces: c.Namespaces,
		// Pods 必须传入：候选策略按 workload 花名册生成而非按流量生成，
		// 缺了它，流量全 DEGRADED（mesh 内）或全 UNKNOWN（策略写坏）的
		// workload 会从候选集里悄悄消失，连带绕过它们的强制 Baseline 注入。
		Pods:         c.Pods,
		Observations: obs,
	})

	return candidateSet{cluster: c, observations: obs, result: gen}, nil
}

// PolicyPreview 生成候选策略并回放预测。集群或命名空间不存在时返回错误。
func (r *FixtureReader) PolicyPreview(
	ctx context.Context, clusterID, namespace string, window TimeWindow,
) (PolicyPreview, error) {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return PolicyPreview{}, err
	}
	c, gen := cs.cluster, cs.result
	if namespace != "" && !hasNamespace(c.Namespaces, namespace) {
		return PolicyPreview{}, fmt.Errorf("%w: %s/%s", ErrNamespaceNotFound, clusterID, namespace)
	}

	report := predict.Run(predict.Input{
		ClusterID:    clusterID,
		Policies:     gen.EnabledPolicies(),
		Namespaces:   c.Namespaces,
		CCNPPresent:  c.CCNPPresent,
		Observations: cs.observations,
		// 展示名复用流量列表那一套，两个界面必须用同一个名字指同一个 Pod。
		Label: endpointLabel,
	})

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
	overriddenReport := predict.Run(predict.Input{
		ClusterID:    clusterID,
		Policies:     overridden.EnabledPolicies(),
		Namespaces:   c.Namespaces,
		CCNPPresent:  c.CCNPPresent,
		Observations: cs.observations,
		Label:        func(ep replay.Endpoint) string { return endpointLabel(ep) },
	})

	// 一份裁剪结果，两处使用：屏幕上的候选集与导出的文件必须是同一个
	// 切片渲染出来的，各裁一次就又有了两个可以互相分歧的选择点。
	overriddenCandidates := FilterCandidates(overridden.Policies, namespace)

	return PolicyPreview{
		Cluster: clusterID, Namespace: namespace, Window: window,
		// 合成数据集不是一次观测，没有采样、没有丢弃、没有覆盖不满的窗口 ——
		// 它就是完整的。填 COMPLETE 而不是留空：空值不在 flow.Completeness
		// 的封闭枚举里，前端拿到它只能猜，而这个字段存在的理由正是不让人猜。
		WindowCompleteness: flow.CompletenessComplete,
		Candidates:         FilterCandidates(gen.Policies, namespace),
		MissingBaselines:   FilterMissing(gen.MissingBaselines, namespace),
		// 合成数据集把五类依据都带齐了（Services / Endpoints / Gateways /
		// ScrapeTargets / NodeAgents 都在 fixture.Cluster.Assets 里），因此
		// 这里恒为空。**非 nil**：空清单要读作"五类都检查过、都在"，而不是
		// 读作"这个 Reader 没回答"（见字段说明）。
		NotAssessedBaselines: []baseline.Kind{},
		Ungeneratable:        gen.Ungeneratable,
		ExcludedWorkloads:    gen.ExcludedWorkloads,
		Prediction:           report,
		Kinds:                baseline.AllKinds(),
		Overrides:            stored,
		StaleOverrides:       stale,
		Overridden: OverriddenView{
			Candidates: overriddenCandidates,
			Prediction: overriddenReport,
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
