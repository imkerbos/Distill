package collectstore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/store"
)

// candidateSet 是一次候选策略生成的全部中间产物。
//
// 与 FixtureReader.candidateSet 同形、同理由：预览与将来的写前校验都要
// 回答"当前候选集长什么样"，各自拼装生成输入只要有一处漂移，两个答案就会
// 指向不同的候选集。
type candidateSet struct {
	traffic
	// trafficObserved 表示这次生成背后有没有真实观测。
	//
	// 没有时候选集仍然是真的（Baseline 依据资产），但预测的每一项都是 0，
	// 而那个 0 是「没有评估过」不是「评估出来是 0」。
	trafficObserved bool
	observations    []policygen.Observation
	result          policygen.Result
	// notAssessed 是依据资源这次采集根本没有枚举的那几类 Baseline。
	notAssessed []baseline.Kind
	// inapplicable 是被操作者显式声明"这个集群不需要"的那几类。
	//
	// 与 notAssessed 是两件事：那一栏说"我们没看过"，这一栏说"看过了、
	// 不需要"，而后者是一次有记录的人工判断（登记在集群上，写审计）。
	inapplicable []baseline.Kind
}

// PolicyPreview 用真实采集到的资产与流量生成候选策略并回放预测。
//
// **判定复用流量那条路径**（readTraffic / attribute），不另起一条：屏幕上的
// 流量列表与这份预览必须是同一次计算的两种呈现。两条路径今天算得一样，
// 明天窗口边界一漂就会给出互相矛盾的一屏，而这种不一致不会报错。
//
// 生成恒为整个集群，namespace 只裁展示（store.FilterCandidates 的说明）。
func (r *Reader) PolicyPreview(
	ctx context.Context, clusterID, namespace string, window store.TimeWindow,
) (store.PolicyPreview, error) {
	// 默认 workload 粒度 —— 最细的那一层，也是人工确认挂靠的那一层。
	return r.PolicyPreviewAtGranularity(
		ctx, clusterID, namespace, window, policygen.GranularityWorkload)
}

// PolicyPreviewAtGranularity 同 PolicyPreview，但指定主体粒度。
//
// **策略与预测必须同粒度。** 两个粒度是两批不同的策略，一份 namespace 粒度的
// 策略集配上 workload 粒度算出来的 WOULD_BREAK 描述的是另一套策略，且偏在
// 让人放心的方向（粗化只会放宽，因此拦断更少）—— 与写回拒绝 namespace 筛选
// 是同一条理由（design doc 2026-08-19 §3）。因此折叠与两次预测都在这里发生，
// 不交给调用方拼。
func (r *Reader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, window store.TimeWindow,
	granularity policygen.Granularity,
) (store.PolicyPreview, error) {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return store.PolicyPreview{}, err
	}
	if namespace != "" && !slices.ContainsFunc(cs.namespaces,
		func(ns replay.NamespaceRef) bool { return ns.Name == namespace }) {
		return store.PolicyPreview{}, fmt.Errorf(
			"%w: %s/%s", store.ErrNamespaceNotFound, clusterID, namespace)
	}

	gen := cs.result

	stored, err := r.src.RuleOverrides(ctx, clusterID)
	if err != nil {
		return store.PolicyPreview{}, err
	}
	pgOverrides := make([]policygen.Override, 0, len(stored))
	for _, o := range stored {
		pgOverrides = append(pgOverrides, o.ToPolicygen())
	}
	// Apply 建在同一次 Generate 的输出上，两套预测因此必然可比。
	//
	// **覆盖先于折叠。** 人工确认记在 workload 粒度（rule_override 的键），
	// 折叠取的是确认之后仍然启用的那个集合。反过来做就没有 workload 可以
	// 挂键了，而且一条只为某个 workload 禁用的决定会变成对整个 namespace
	// 生效 —— 爆炸半径完全不同（design doc §4）。
	overridden, stale := policygen.Apply(gen, pgOverrides)

	// 折叠放在两次预测**之前**：预测跑的必须是屏幕上那一套策略。
	widening := []policygen.Widening{}
	if granularity == policygen.GranularityNamespace {
		gen, _ = gen.AtNamespaceGranularity()
		overridden, widening = overridden.AtNamespaceGranularity()
	}

	// 证据读在生成之后、返回之前：它按指纹关联，与这一批候选算出来的
	// 指纹必须来自同一次生成。
	//
	// **读不到就是读不到，不降级成空 map。** 空 map 的含义是「记过，还没有
	// 证据」，而查询失败时真实情况是「不知道」—— 把两者压平会让一条其实
	// 已经观察了三周的规则显示成刚出现，从而被当作不可信而搁置。
	stored2, err := r.facts.RuleEvidenceOf(ctx, clusterID)
	if err != nil {
		return store.PolicyPreview{}, err
	}
	// 主体一并进键：指纹不含主体，两个 workload 上的同一条放行是两份证据
	// （snapshotstore.EvidenceKey）。
	evidence := make(map[string]store.RuleEvidence, len(stored2))
	for k, e := range stored2 {
		evidence[k] = store.RuleEvidence{
			FirstSeen: e.FirstSeen, LastSeen: e.LastSeen,
			Windows: e.Windows, CompleteWindows: e.CompleteWindows,
			Observations: e.Observations,
		}
	}

	// 两份预测，同一批输入（design doc 2026-08-25-existing-policies §3）：
	//
	//   只跑候选集    —— 如果把旧策略也清理掉会怎样（接管路线的终点）
	//   并上已有策略  —— 合并这个 PR 之后实际会拦断什么
	//
	// **已有策略取的是 cs.policies，也就是窗口锚点那一刻的那一批**，不是
	// LivePolicies（最近一次采集）。当前判定用的就是锚点那一批，两份预测
	// 必须与它可比 —— 混进另一个时刻的策略，差异会来自"策略换了"而不是
	// "候选集加了规则"，而那正是这两个数字要区分的东西（CLAUDE.md §4：
	// 禁止用当前状态解释历史数据）。
	enabled := gen.EnabledPolicies()
	overriddenEnabled := overridden.EnabledPolicies()
	report := cs.predictWith(enabled)
	overriddenReport := cs.predictWith(overriddenEnabled)
	reportWithExisting := cs.predictWith(predict.WithExisting(cs.policies, enabled))
	overriddenWithExisting := cs.predictWith(predict.WithExisting(cs.policies, overriddenEnabled))

	// 一份裁剪结果，两处使用：屏幕上的候选集与导出的文件必须是同一个切片
	// 渲染出来的，各裁一次就又有了两个可以互相分歧的选择点。
	overriddenCandidates := store.FilterCandidates(overridden.Policies, namespace)

	return store.PolicyPreview{
		Cluster: clusterID, Namespace: namespace, Window: window,
		// 粒度回显：一份不说明自己粒度的策略集，操作者无从判断屏幕上那 42 份
		// 是"折叠过的"还是"这个集群只有 42 个 workload"。
		Granularity:     effectiveGranularity(granularity),
		Widening:        widening,
		TrafficObserved: cs.trafficObserved,
		// 完整度照实回显，不由调用方从 DegradedCount 推断（design doc §4）。
		WindowCompleteness: cs.completeness,
		Candidates:         store.FilterCandidates(gen.Policies, namespace),
		Evidence:           evidence,
		// 未评估的那几类**照旧留在缺失清单里**，不摘走（design doc §11）。
		//
		// 摘走会让只读 MissingBaselines 的消费方看见比实际更少的阻塞项，而
		// 依据采集一旦 403 或超时，DNS 这种要紧的类会**间歇性**地从清单里
		// 消失 —— 一个从没验证过 DNS 依据的集群于是被放行进 Enforcing。
		// 两栏重叠是刻意的：缺失清单回答"还差哪几类"，未评估清单回答
		// "其中哪几类是我们没看过"，后者不减少前者。
		// 被显式声明"不适用"的那一类从两栏同时摘掉（design doc
		// 2026-08-18-node-agent-applicability §2）。**这是两栏里唯一允许的
		// 摘除**：未评估那条纪律说的是"不减少缺失"，而它针对的是
		// 「我们没看过」—— 那时缺口仍然存在，只是我们不知道。这里不同：
		// 有人看过了，并且写下了为什么不需要，那条记录在审计里。
		MissingBaselines:     dropInapplicable(store.FilterMissing(gen.MissingBaselines, namespace), cs.inapplicable),
		NotAssessedBaselines: dropKinds(cs.notAssessed, cs.inapplicable),
		// 只装**推导出来**的不适用（namespace 里没有推导对象），不含人工
		// 声明的那一类：后者带着一条写下来的理由与一行审计，把它混进来会让
		// 一个平台自己推出来的结论看上去也有人签过字。
		NotApplicableBaselines: store.FilterMissing(gen.NotApplicableBaselines, namespace),
		Ungeneratable:          gen.Ungeneratable,
		UnattachedImports:      gen.UnattachedImports,
		ExcludedWorkloads:      gen.ExcludedWorkloads,
		Prediction:             report,
		PredictionWithExisting: reportWithExisting,
		Kinds:                  baseline.AllKinds(),
		Overrides:              stored,
		StaleOverrides:         stale,
		Overridden: store.OverriddenView{
			Candidates:             overriddenCandidates,
			Prediction:             overriddenReport,
			PredictionWithExisting: overriddenWithExisting,
			// 复用 EnabledPolicies 而不是另写一段渲染：「哪些规则算启用」
			// 只能有一个定义，预测跑的正是这个函数的输出。
			Enabled: policygen.Result{Policies: overriddenCandidates}.EnabledPolicies(),
		},
	}, nil
}

// generate 解析窗口内的事实并跑一次候选策略生成。
func (r *Reader) generate(
	ctx context.Context, clusterID string, window store.TimeWindow,
) (candidateSet, error) {
	c, err := r.collectedCluster(ctx, clusterID)
	if err != nil {
		return candidateSet{}, err
	}

	// 这个集群一次流量都没摄入过时，仍然给得出候选：**Baseline 按 workload
	// 无条件注入，依据是资产而不是流量**（policygen.Input.Pods 的说明）。
	// 挡住它的只是这里要一个流量窗口 —— 而那正是操作者问「那你推荐我加
	// 什么策略」时最需要的一屏（design doc 2026-08-18）。
	//
	// 只放行「从没摄入过」这一种：调用方点名了一段没有数据的时间时照旧
	// 拒绝，按资产回答等于悄悄换掉了他问的那个问题。
	var t traffic
	trafficObserved := true
	if _, werr := r.latestFlowWindow(ctx, clusterID); errors.Is(werr, ErrNoFlowIngest) {
		trafficObserved = false
		d, derr := r.describeAssets(ctx, clusterID)
		if derr != nil {
			return candidateSet{}, derr
		}
		if t, err = r.trafficOf(ctx, c, d); err != nil {
			return candidateSet{}, err
		}
	} else if werr != nil {
		return candidateSet{}, werr
	} else {
		// 先于集群校验：缺时间窗是调用方用错了接口，与查哪个集群无关。
		if !window.Valid() {
			return candidateSet{}, store.ErrWindowRequired
		}
		if t, err = r.readTraffic(ctx, clusterID, flow.Window{From: window.From, To: window.To}); err != nil {
			return candidateSet{}, err
		}
	}

	assets, err := r.assetsAt(ctx, t)
	if err != nil {
		return candidateSet{}, err
	}
	evidence, err := r.readRunEvidence(ctx, t.described)
	if err != nil {
		return candidateSet{}, err
	}

	// 每一条连接都进观测清单，包括判不出来的：丢掉它们会让观测集合小于
	// 真实集合，于是覆盖它们的规则被判"无流量、可收紧"（design doc §3）。
	// FlowID 与流量列表用同一个函数算，两屏点的是同一条连接。
	obs := make([]policygen.Observation, 0, len(t.conns))
	for i, c := range t.conns {
		a := t.attribute(c)
		obs = append(obs, policygen.Observation{
			FlowID: flowIDOf(t.clusterID, t.window, i, c), Flow: a.flow, Decision: a.decision,
			// 传导完整度之前的那个可信度，见 attributed.identityTrusted。
			IdentityTrusted: a.identityTrusted,
		})
	}

	// 先算未评估，再喂给生成：Derive 要靠它区分"集群里就是没有"与
	// "我们没看过"，而两者在资产里长得一模一样。
	notAssessed := notAssessedBaselines(evidence)

	// 人工导入的补充规则进候选集（design doc 2026-08-25-existing-policies §3）。
	//
	// **读失败就整次失败，不降级成"没有导入"**：那些规则补的正是观测看不见
	// 的连接，静默丢掉会让一份缺了月结批处理放行的策略集看起来完整 —— 而
	// dry-run 报不出这个缺口，它只评估见过的连接。
	imports, err := r.candidateImports(ctx, clusterID)
	if err != nil {
		return candidateSet{}, err
	}

	return candidateSet{
		traffic:         t,
		trafficObserved: trafficObserved,
		observations:    obs,
		result: policygen.Generate(policygen.Input{
			ClusterID: clusterID,
			Assets:    assets, Namespaces: t.namespaces,
			// Pods 必须传入：候选策略按 workload 花名册生成而非按流量生成，
			// 缺了它，流量全 DEGRADED（mesh 内）或全 UNKNOWN（策略写坏）的
			// workload 会从候选集里悄悄消失，连带绕过它们的强制 Baseline 注入。
			Pods:                t.roster(),
			Observations:        obs,
			UnassessedBaselines: notAssessed,
			Imports:             imports,
		}),
		notAssessed:  notAssessed,
		inapplicable: inapplicableBaselines(c),
	}, nil
}

// predictWith 用给定的策略集回放一次预测，并把窗口完整度传导上去。
//
// 两次预测（覆盖前 / 覆盖后）走同一个函数：完整度必须对两份都成立，各写
// 一次就给了"其中一份忘了传导"一个位置，而屏幕上两份并排显示，一份写着
// TRUSTED、一份写着 DEGRADED 时没有人知道该信哪一份。
func (cs candidateSet) predictWith(policies []networkingv1.NetworkPolicy) predict.Report {
	return degradeByCompleteness(cs.completeness, predict.Run(predict.Input{
		ClusterID:    cs.clusterID,
		Policies:     policies,
		Namespaces:   cs.namespaces,
		ForeignPlane: cs.registered.EffectivePlanes().Degrades(),
		Observations: cs.observations,
		// 展示名复用流量列表那一套，两个界面必须用同一个名字指同一个 Pod。
		Label: cs.previewLabel,
	}))
}

// previewLabel 渲染预测报告里的端点展示名。
//
// 与 traffic.endpointLabel 逐字一致：Pod 为 nil 恰好就是那边 outcome 不是
// RESOLVED 的情形（attribute 只在两端都解得开时才填 Pod）。解不出来时只给
// 地址，不给一个写着 "cluster//" 的空主体 —— 它在界面上看起来像个真东西。
func (cs candidateSet) previewLabel(ep replay.Endpoint) string {
	if ep.Pod == nil {
		return ep.IP
	}
	return fmt.Sprintf("%s/%s/%s", cs.clusterID, ep.Pod.Namespace, ep.Pod.Name)
}

// roster 把锚点那一刻的 Pod 快照变成候选策略的生成名册。
//
// 只带生成用得上的几项（安全规范 §20 / §35）：selector 比的是标签，
// hostNetwork 决定这个 Pod 能不能被 podSelector 表达。IP 不带 —— 名册回答
// 的是"有哪些 workload"，不是"它们当时在哪个地址上"。
func (t traffic) roster() []replay.PodRef {
	out := make([]replay.PodRef, 0, len(t.pods))
	for key, p := range t.pods {
		out = append(out, replay.PodRef{
			ClusterID: t.clusterID, Namespace: key.namespace, Name: key.name,
			Labels: p.labels, HostNetwork: p.hostNetwork,
		})
	}
	return out
}

// assetsAt 装出 Baseline 推导要用的那份资产快照。
//
// 集群登记（网段、apiserver 端点）来自注册表，其余来自锚点那一次采集 ——
// 不是最新一次（CLAUDE.md §4）。
//
// **ScrapeTargets 由「登记的抓取端 × 观测到的被抓端」拼出**
// （design doc 2026-08-18-metrics-scrape-evidence §4）：抓取端观测不出来，
// 被抓端登记不出来，两半各有各的来源。没有登记抓取端时它是空的，
// METRICS_SCRAPE 照旧进缺失清单 —— 不补占位数据。
//
// **NodeAgents 同样来自登记**（design doc 2026-08-18-node-agent-applicability §3）：
// 平台看得见集群里有哪些 hostNetwork DaemonSet，但看不见它们往哪连 —— agent
// 连不连工作负载、连哪个端口，写在它自己的配置里。没有登记时它是空的，
// NODE_AGENT 照旧进缺失清单，除非操作者显式声明了这个集群不需要（§4.3）。
func (r *Reader) assetsAt(ctx context.Context, t traffic) (snapshot.Assets, error) {
	services, err := r.readServicesAt(ctx, t.described)
	if err != nil {
		return snapshot.Assets{}, err
	}
	endpoints, err := r.readEndpointsAt(ctx, t.described)
	if err != nil {
		return snapshot.Assets{}, err
	}
	gateways, err := r.readGatewaysAt(ctx, t.described)
	if err != nil {
		return snapshot.Assets{}, err
	}
	pods, err := r.readPodsAt(ctx, t.described)
	if err != nil {
		return snapshot.Assets{}, err
	}
	// 只把这一半用得上的两个字段搬过去：ScrapeTargetSnapshots 要的是
	// 「这个 Pod 在哪、它声明了什么」，不需要整行观测。
	declared := make([]snapshot.Pod, 0, len(pods))
	for _, p := range pods {
		if len(p.scrapeAnnotations) == 0 {
			continue
		}
		declared = append(declared, snapshot.Pod{
			Namespace: p.namespace, Name: p.name, ScrapeAnnotations: p.scrapeAnnotations,
		})
	}

	return snapshot.Assets{
		ClusterID:     t.clusterID,
		Services:      services,
		Endpoints:     endpoints,
		Gateways:      gateways,
		APIServers:    t.registered.APIServerSnapshots(),
		ScrapeTargets: t.registered.ScrapeTargetSnapshots(declared),
		// 被抓端的声明单独带出去，不只喂给 ScrapeTargetSnapshots：
		// 它决定 METRICS_SCRAPE 这一类**适不适用**，而 ScrapeTargets 决定
		// 推不推得出规则。拿后者当适用性判据的话，一个还没登记抓取端的集群
		// 每个 namespace 都会显得"不需要放行抓取流量"，下发之后真正的
		// Prometheus 会被挡（design doc 2026-08-18-baseline-applicability §4.2）。
		ScrapeDeclarations: scrapeDeclarationsOf(t.clusterID, declared),
		NodeAgents:         t.registered.NodeAgentSnapshots(),
		Registry:           t.registered.ToSnapshot(),
	}, nil
}

// scrapeDeclarationsOf 把声明了抓取意愿的 Pod 转成适用性判据。
//
// 端口不带过来：那属于"怎么抓"，由登记的抓取端给出。这里只回答
// "这个 namespace 里有没有东西会因为 default-deny 丢掉抓取流量"。
func scrapeDeclarationsOf(clusterID string, declared []snapshot.Pod) []snapshot.ScrapeDeclaration {
	out := make([]snapshot.ScrapeDeclaration, 0, len(declared))
	for _, p := range declared {
		// scrape=false 是一句"别抓我"，不是一次声明 —— 与 scrapePortOf
		// 同一条判据，两处不得分家。
		if !p.DeclaresScrape() {
			continue
		}
		out = append(out, snapshot.ScrapeDeclaration{
			ClusterID: clusterID, Namespace: p.Namespace, PodName: p.Name,
		})
	}
	return out
}

// degradeByCompleteness 把窗口完整度传导到**整份**预测（design doc §4）。
//
// 观测不全时说的"不会拦断任何连接"，与观测完整时的同一句话含义完全不同。
// 少了这一步，一个已知漏了记录的窗口会输出一份 TRUSTED 的预测 —— 比不给
// 结论更糟，因为它读起来是可信的。
//
// **verdict 一个字也不改**（CLAUDE.md §3）：一次判定可以同时是"会拦断"与
// "不可信"，两者各占一个字段。把降级写成"改判 UNKNOWN"会让 WOULD_BREAK
// 整类塌进 UNKNOWN，于是一个 DEGRADED 窗口的预览报"会拦断 0 条"。
//
// **只降不升**：只改本来写着 TRUSTED 的那些。求值引擎自己因为 mesh 或 CCNP
// 降下来的（replay.confidenceFor）已经是 DEGRADED，不用碰；UnratedCount 那
// 一档也不碰 —— 它记的是"回放层给了一个枚举外的可信度"，折进 DEGRADED 就把
// 一个该被看见的异常抹平了（predict.Report.UnratedCount 的说明）。
func degradeByCompleteness(c flow.Completeness, rep predict.Report) predict.Report {
	if confidenceOf(c) != replay.ConfidenceDegraded {
		return rep
	}
	for kind, flows := range rep.Changes {
		for i := range flows {
			if flows[i].Confidence == string(replay.ConfidenceTrusted) {
				flows[i].Confidence = string(replay.ConfidenceDegraded)
			}
		}
		rep.Changes[kind] = flows
	}
	rep.DegradedCount += rep.TrustedCount
	rep.TrustedCount = 0
	return rep
}

// inapplicableBaselines 挑出被这个集群显式声明"不需要"的那几类 Baseline。
//
// 今天只有 NODE_AGENT 有这条路径（design doc 2026-08-18-node-agent-applicability）。
// 别的类都推得出依据，或者依据缺失本身就是要修的东西 —— 只有节点 agent 这一类，
// 「这个集群根本没有需要放行的 agent」是一个合理且常见的事实，而平台观测不出来。
//
// **判据是理由非空，不是一个布尔**：一次没有理由的声明在写入侧就会被拒
// （registry.ValidateCluster），因此这里读到非空就意味着有人写下过为什么。
func inapplicableBaselines(c registry.Cluster) []baseline.Kind {
	if c.NoNodeAgentsReason == "" {
		return nil
	}
	return []baseline.Kind{baseline.KindNodeAgent}
}

// dropKinds 从一份清单里去掉指定的那几类。
func dropKinds(in, drop []baseline.Kind) []baseline.Kind {
	if len(drop) == 0 {
		return in
	}
	out := make([]baseline.Kind, 0, len(in))
	for _, k := range in {
		if !slices.Contains(drop, k) {
			out = append(out, k)
		}
	}
	return out
}

// dropInapplicable 从缺失清单里去掉不适用的那几类。
//
// 逐个 namespace 处理并**丢掉变空的那一项**：一个"缺失 0 类"的条目在界面上
// 与"这个命名空间有缺口"长得一样，而它什么缺口都没有。
func dropInapplicable(in []policygen.MissingBaseline, drop []baseline.Kind) []policygen.MissingBaseline {
	if len(drop) == 0 {
		return in
	}
	out := make([]policygen.MissingBaseline, 0, len(in))
	for _, m := range in {
		kinds := dropKinds(m.Kinds, drop)
		if len(kinds) == 0 {
			continue
		}
		out = append(out, policygen.MissingBaseline{Namespace: m.Namespace, Kinds: kinds})
	}
	return out
}

// effectiveGranularity 把未登记的取值收敛到 WORKLOAD。
//
// 失败方向朝窄（安全规范 §49）：WORKLOAD 是现状、也是更精确的那一侧。
// 落到 NAMESPACE 会把一份本该只选中一个 workload 的策略变成选中整个
// 命名空间，而那个方向不该靠一个零值走到。
func effectiveGranularity(g policygen.Granularity) policygen.Granularity {
	if g == policygen.GranularityNamespace {
		return policygen.GranularityNamespace
	}
	return policygen.GranularityWorkload
}

// DeletionImpact 预测把 removed 这批策略从集群里移除会发生什么
// （design doc 2026-08-24 §4.3）。
//
// 策略集取的是**锚点那一刻集群里真实跑着的那一份**（traffic.policies）减去
// removed，而不是候选集：删除说的是「Config Sync 会把这些对象从集群里拿掉」，
// 拿掉的是集群现状里的那几条，不是平台建议里的那几条。
//
// 完整度照样传导（degradeByCompleteness）：一段漏过记录的窗口算出来的删除
// 影响同样不可信，而删除恰恰是最不能凭一份不可信结论去做的那类变更。
func (r *Reader) DeletionImpact(
	ctx context.Context, clusterID string, window store.TimeWindow,
	removed []networkingv1.NetworkPolicy,
) (store.DeletionImpactReport, error) {
	cs, err := r.generate(ctx, clusterID, window)
	if err != nil {
		return store.DeletionImpactReport{}, err
	}
	kept, inWindow := store.WithoutPolicies(cs.policies, removed)
	report := cs.predictWith(kept)

	// 「现在还在不在」必须用最近一次采集回答，不能用窗口锚点那一份
	// （2026-08-24 实测发现）：一条在窗口之后才被 GitOps 下发的策略，用窗口
	// 口径去看是「集群里没有」，据此会被标成「删掉没影响」—— 而它正在生效。
	// 分类朝让人放心的方向错，是这个平台最不能犯的那一类。
	live, err := r.livePolicyCount(ctx, clusterID, removed)
	if err != nil {
		return store.DeletionImpactReport{}, err
	}
	return store.DeletionImpactReport{
		TrafficObserved: cs.trafficObserved,
		Window:          window,
		Counts:          report.Counts,
		Removed:         len(removed),
		Live:            live,
		InWindow:        inWindow,
	}, nil
}

// livePolicyCount 数 removed 里有几份仍然存在于**最近一次采集**看到的集群里。
//
// 与预测那一半走两条不同的锚点，是刻意的：预测要与观测流量同期（CLAUDE.md §4
// 禁止用当前状态解释历史数据），而「现在还在不在」是一个关于此刻的问题。
func (r *Reader) livePolicyCount(
	ctx context.Context, clusterID string, removed []networkingv1.NetworkPolicy,
) (int, error) {
	parsed, err := r.LivePolicies(ctx, clusterID)
	if err != nil {
		return 0, err
	}
	_, live := store.WithoutPolicies(parsed, removed)
	return live, nil
}

// LivePolicies 返回最近一次采集看到的、这个集群里真实存在的 NetworkPolicy。
//
// 锚点取**最近一次成功的采集**（describeAssets），不取某个观测窗口：三个
// 消费方问的都是「现在有什么」—— 删除影响里的 Live、写回的冲突判定、
// 真实漂移（design doc 2026-08-25 §4、§5）。
func (r *Reader) LivePolicies(
	ctx context.Context, clusterID string,
) ([]networkingv1.NetworkPolicy, error) {
	latest, err := r.describeAssets(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	observed, err := r.readPoliciesAt(ctx, latest)
	if err != nil {
		return nil, err
	}
	return parsePolicies(observed)
}

// Reconciliation 把平台回放算出的判定与执行平面自己报的判定对账
// （design doc 2026-08-25 §3）。
//
// 与 Flows 走同一条判定路径（readTraffic → attribute）：对账要有意义，
// 比较的必须是**屏幕上那一份判定**，另起一次求值就等于在拿两个引擎互相验证。
func (r *Reader) Reconciliation(
	ctx context.Context, clusterID string, window store.TimeWindow,
) (store.ReconciliationReport, error) {
	t, err := r.readTraffic(ctx, clusterID, flow.Window{From: window.From, To: window.To})
	if err != nil {
		return store.ReconciliationReport{}, err
	}

	obs := make([]reconcile.Observation, 0, len(t.conns))
	reported := false
	for _, c := range t.conns {
		a := t.attribute(c)
		v, ok := c.Verdict()
		if ok {
			reported = true
		}
		obs = append(obs, reconcile.Observation{
			Subject:  store.SubjectOfEndpoint(a.flow.Source),
			Platform: a.decision.Verdict,
			Observed: v, Reported: ok,
			// 连接本身带上：分歧被抽成样本时要拿它渲染证据。带的是**这一次
			// 求值用的那条流量**，不是回头再查一次 —— 窗口边界一漂，二次查询
			// 拿到的就是另一批连接。
			Flow: a.flow,
		})
	}
	rep := reconcile.Run(obs)
	return store.ReconciliationReport{
		Cluster: clusterID, Window: window,
		SourceReportsVerdicts: reported,
		Report:                rep,
		Samples:               store.ReconciliationSamplesOf(rep.Samples),
	}, nil
}

// candidateImports 读出这个集群要补进候选集的人工导入。
//
// **只取 CANDIDATE_ADDITION。** 另一个角色 BASELINE_CURRENT 描述的是"集群
// 当前跑着什么"，它属于回放的 current 侧，与候选集是两件事；混进来会让一条
// 用于描述现状的策略变成一条平台推荐下发的规则。
//
// 解析失败**跳过那一条并继续**，不是整次失败：导入在写入时已经过
// registry.ParseImport 校验，走到这里还解析不了说明那条记录本身坏了（库被
// 手工改过、或迁移出过问题）。为一条坏记录让整个集群的预览打不开，处置成本
// 远高于它本身；而它不会被静默当成"没有这条规则"—— 挂不上的导入由
// policygen 报进 UnattachedImports，坏记录则在日志里留痕。
//
// **不做 YAML 缓存**：导入随时会被增删，而一次预览要的是"此刻登记的是什么"。
func (r *Reader) candidateImports(
	ctx context.Context, clusterID string,
) ([]policygen.ImportedPolicy, error) {
	stored, err := r.src.PolicyImports(ctx, clusterID)
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
