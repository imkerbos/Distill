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
	report := cs.predictWith(gen.EnabledPolicies())

	stored, err := r.src.RuleOverrides(ctx, clusterID)
	if err != nil {
		return store.PolicyPreview{}, err
	}
	pgOverrides := make([]policygen.Override, 0, len(stored))
	for _, o := range stored {
		pgOverrides = append(pgOverrides, o.ToPolicygen())
	}
	// Apply 建在同一次 Generate 的输出上，两套预测因此必然可比。
	overridden, stale := policygen.Apply(gen, pgOverrides)
	overriddenReport := cs.predictWith(overridden.EnabledPolicies())

	// 一份裁剪结果，两处使用：屏幕上的候选集与导出的文件必须是同一个切片
	// 渲染出来的，各裁一次就又有了两个可以互相分歧的选择点。
	overriddenCandidates := store.FilterCandidates(overridden.Policies, namespace)

	return store.PolicyPreview{
		Cluster: clusterID, Namespace: namespace, Window: window,
		TrafficObserved: cs.trafficObserved,
		// 完整度照实回显，不由调用方从 DegradedCount 推断（design doc §4）。
		WindowCompleteness: cs.completeness,
		Candidates:         store.FilterCandidates(gen.Policies, namespace),
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
		Ungeneratable:        gen.Ungeneratable,
		ExcludedWorkloads:    gen.ExcludedWorkloads,
		Prediction:           report,
		Kinds:                baseline.AllKinds(),
		Overrides:            stored,
		StaleOverrides:       stale,
		Overridden: store.OverriddenView{
			Candidates: overriddenCandidates,
			Prediction: overriddenReport,
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
			Pods:         t.roster(),
			Observations: obs,
		}),
		notAssessed:  notAssessedBaselines(evidence),
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
		CCNPPresent:  cs.registered.CCNPPresent,
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
		NodeAgents:    t.registered.NodeAgentSnapshots(),
		Registry:      t.registered.ToSnapshot(),
	}, nil
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
