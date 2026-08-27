package collectstore

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/store"
)

// traffic 是回答一次流量类读请求（Flows / Flow / Security）所需的全部东西。
//
// 一次读请求解析一次，三个方法共用：同一个集群同一段窗口，列表里的一条与
// 下钻看到的那一条、以及安全报告里点名的那一条，必须是同一次判定的结果。
// 各自现取会让三屏之间隔着一次采集，而差额无人解释 —— 与 described 存在的
// 理由相同，只是这一层多带了做判定要用的东西。
type traffic struct {
	described
	// eval 是锚点那一刻的策略集构成的求值器。
	//
	// 按被问到的时刻构造，不是最新一次快照：一条昨天的连接要用昨天的策略
	// 解释，拿今天的策略集回答昨天会得出一个答得出、又不报错的错误结论
	// （CLAUDE.md §4）。
	eval *replay.Evaluator
	// pods 是锚点那一刻的 Pod，按 (namespace, name) 索引。
	//
	// 标签只存在于快照里 —— 区间表存的是主体，不是主体的属性，而 selector
	// 比的正是属性。判定必须两者都有：区间表说"那一刻这个地址属于谁"，
	// 快照说"那个 Pod 当时带着什么标签"。
	pods map[podKey]observedPod
	// policies 是锚点那一刻的策略集，与 eval 出自同一次解析。
	//
	// 留一份而不是让安全发现自己再读一次：覆盖率与判定必须认同一批策略，
	// 各读各的就多出一个"判定认这条策略、裸奔统计不认"的位置。
	policies []networkingv1.NetworkPolicy
	// namespaces 是锚点那一刻的命名空间快照，与 eval 出自同一次读取。
	//
	// 留一份而不是让策略预览自己再读一次：候选策略的生成与回放、以及
	// "这个 namespace 存不存在"这句拒绝，必须认同一批 namespace 标签 ——
	// 各读各的会让预览页按一份标签生成、按另一份判定。
	namespaces []replay.NamespaceRef
	// registered 是注册表里的那一行，与门禁 collectedCluster 看到的是同一行。
	//
	// 带下来而不是再查一次：候选策略的 Baseline 依据（网段登记、apiserver
	// 端点）与判定的 CCNPPresent 都出自它，再查一次只会多出一个"门禁看到
	// 的集群"与"生成用到的集群"不是同一行的位置。
	registered registry.Cluster
	// fleet 是当前的网段登记，用来认出跨集群与出公网的对端。
	fleet *cluster.Registry
}

// readTraffic 解析一段窗口的事实，并把做判定要用的资产一并装好。
//
// 门禁在这里做一次（collectedCluster），之后一路不再查注册表：一个 FIXTURE
// 集群走到这里只会拿到 ErrClusterNotFound，而不是一份用采集数据拼出来、
// 却写着演示集群名字的报告（design doc §2）。
func (r *Reader) readTraffic(
	ctx context.Context, clusterID string, window flow.Window,
) (traffic, error) {
	c, err := r.collectedCluster(ctx, clusterID)
	if err != nil {
		return traffic{}, err
	}
	d, err := r.describeAt(ctx, clusterID, window)
	if err != nil {
		return traffic{}, err
	}
	return r.trafficOf(ctx, c, d)
}

// readLatestTraffic 解析"最近一次真正观测到流量的那个窗口"的事实并装好判定。
//
// 给接口上没有时间窗参数的调用方用（Quality）。窗口从哪来是调用方的事，
// 之后的每一步必须与 readTraffic 完全一致：判定只能有一份算法，各写一份
// 就会让同一个集群在质量页与列表页给出两套结论 —— 而两页对不上账时，
// 运维无从知道该信哪一页。
func (r *Reader) readLatestTraffic(ctx context.Context, clusterID string) (traffic, error) {
	c, err := r.collectedCluster(ctx, clusterID)
	if err != nil {
		return traffic{}, err
	}
	window, err := r.latestFlowWindow(ctx, clusterID)
	if err != nil {
		return traffic{}, err
	}
	d, err := r.describeAt(ctx, clusterID, window)
	if err != nil {
		return traffic{}, err
	}
	return r.trafficOf(ctx, c, d)
}

// trafficOf 给一份已经解析好的事实配上做判定要用的资产。
func (r *Reader) trafficOf(
	ctx context.Context, c registry.Cluster, d described,
) (traffic, error) {
	pods, err := r.readPodsAt(ctx, d)
	if err != nil {
		return traffic{}, err
	}
	policies, err := r.readPoliciesAt(ctx, d)
	if err != nil {
		return traffic{}, err
	}
	parsed, err := parsePolicies(policies)
	if err != nil {
		return traffic{}, err
	}
	// 人工导入的"现状"策略并进当前策略集
	// （registry.RoleBaselineCurrent；design doc 2026-08-25-existing-policies §3）。
	//
	// **它补的是平台采不到的那部分**：某个 namespace 没有 list 权限、或者策略
	// 由别的系统管着。不并进来，平台会以为那些连接没有任何策略约束，于是判
	// ALLOW —— 而集群里它们其实是被拦着的。
	//
	// **风险的另一面由对账兜住**：如果这条导入是填错的（集群里根本没有那条
	// 策略），平台会判 DENY 而执行平面报 ALLOWED，对账把它记成
	// DISAGREE_UNDER_PERMISSIVE，超阈之后那个 workload 被排除出写回
	// （trust-engineering §3.4）。人填错不会静默变成一次生产阻断。
	imported, err := r.currentImports(ctx, d)
	if err != nil {
		return traffic{}, err
	}
	// 同名时**采集到的赢**：平台亲眼看到的那一份才是集群里真正跑着的，
	// 导入只是一次人工声明。反过来会让一条过期的导入盖住真实策略。
	parsed = predict.WithExisting(imported, parsed)
	namespaces, err := r.readNamespacesAt(ctx, d)
	if err != nil {
		return traffic{}, err
	}
	fleet, err := r.fleetRegistry(ctx)
	if err != nil {
		return traffic{}, err
	}

	byKey := make(map[podKey]observedPod, len(pods))
	for _, p := range pods {
		byKey[podKey{namespace: p.namespace, name: p.name}] = p
	}

	var opts []replay.Option
	// **经由 EffectivePlanes 取值，不读那个 bool**（design doc 2026-08-25 §2）：
	// 「没查过」与「确认不存在」在一个布尔里长得一样，而前者必须降级。
	// 只有平台确认过的 NONE 才配得上满置信度。
	if c.EffectivePlanes().Degrades() {
		// 有没有第二平面是集群级的事实；**被它覆盖的是哪些主体**要按锚点
		// 那一刻的快照读 —— CNP 的覆盖范围会变，拿当前范围解释历史窗口，
		// 当时被覆盖的那一批就不会被降级（CLAUDE.md §4）。
		scopes, complete, err := r.facts.ForeignScopesAt(ctx, d.clusterID, d.anchor)
		switch {
		case err != nil:
			return traffic{}, err
		case complete:
			// 范围完整：降级收窄到真的被覆盖的那些主体。
			opts = append(opts, replay.WithForeignPlaneScopes(foreignScopesOf(scopes)))
		default:
			// **范围不完整就整片降级**，不拿读到的那一半作答：不完整意味着
			// 有主体被覆盖而我们不知道是哪些，漏掉一个就是把一条真的被管着
			// 的连接判成可信。读不到任何采集也走这一支（见 ForeignScopesAt）。
			opts = append(opts, replay.WithForeignPlane(true))
		}
	}
	// 管理面策略只在集群**声明**了它的 CNI 真的执行 ANP 时才解释。
	//
	// 装了 CRD 不等于执行：实测原生 Calico v3.30.4 执行 ANP，而 Cilium 1.19
	// 完全不实现它 —— 两种集群上都可能躺着 ANP 对象。按一个并不生效的平面
	// 求值，平台会以为某条连接被拦着、于是不生成放行规则，那条连接会在下发
	// 之后才真的断。所以默认不解释，要解释必须有人写明理由（EnforcedPlanes）。
	//
	// **这不改变置信度。** 探测到第二平面仍然整片降级 —— 那份降级里还含着
	// CNP 与 Calico 私有策略，而探测结果只落了一个三态、没记下是哪几个平面，
	// 现在还说不出"只剩 ANP 且它已被解释"。判定会因此更正确，置信度照旧
	// 保守（verdict 与 confidence 是两个独立字段，CLAUDE.md §3）。
	if c.Enforces(registry.PlaneAdminNetworkPolicy) {
		stored, err := r.readAdminPoliciesAt(ctx, d)
		if err != nil {
			return traffic{}, err
		}
		anps, banp, err := parseAdminPolicies(stored)
		if err != nil {
			return traffic{}, err
		}
		opts = append(opts, replay.WithAdminPolicies(anps, banp))
	}

	return traffic{
		described:  d,
		eval:       replay.NewEvaluator(d.clusterID, parsed, namespaces, opts...),
		pods:       byKey,
		policies:   parsed,
		namespaces: namespaces,
		registered: c,
		fleet:      fleet,
	}, nil
}

// attributed 是一条连接归属解析与判定之后的全部结果。
//
// 两端的解析结果一并留下：判定说"通不通"，解析结果说"这句话在讲谁"，
// 而后者决定这条连接能不能被表达成"工作负载 A 访问工作负载 B"（design
// doc §3）。只留判定，UNKNOWN 的那些就只剩一个原因字符串，界面再也说不出
// 是哪一端解不开。
type attributed struct {
	conn       flow.Connection
	src        identity.Identity
	dst        identity.Identity
	srcOutcome identity.Outcome
	dstOutcome identity.Outcome
	decision   replay.Decision
	// identityTrusted 是求值引擎对**主体身份**的可信度，取自传导窗口完整度
	// **之前**的那个结论。
	//
	// decision.Confidence 传导之后压着两件事：mesh / CCNP 让身份本身不可信，
	// 与窗口证明不了完整。前者学出的规则会挂到错的主体上，后者的规则本身
	// 没错、只是可能不够（design doc 2026-08-18-learn-from-incomplete-evidence §2）。
	// policygen 要分辨这两者，就必须拿到传导之前的那个值。
	identityTrusted bool
	// flow 是这条连接送进求值引擎的那个形状，**每条都有**，包括判不出来的。
	//
	// 存下来而不是让策略预览再拼一次：预览要把同一批连接喂给 policygen 与
	// predict，各拼一次就有了第二条构造路径，而两条路径分歧时屏幕上没有
	// 任何迹象 —— 那正是本轮反复点名的那个缺陷。
	//
	// 判不出来的那些同样带 flow（两端只有地址、Pod 为 nil）：不带就没法
	// 进观测清单，而丢掉它们会让观测集合小于真实集合，于是覆盖它们的规则
	// 被判"无流量、可收紧"（design doc §3）。
	flow replay.Flow
}

// attribute 解析一条连接的两端并给出判定。
//
// **无法归属的连接不进入求值，但也绝不丢弃**（design doc §3）：丢掉会让观测
// 到的连接集合小于真实集合，于是覆盖它们的规则被判"无流量、可收紧" —— 这
// 一层存在的意义就是挡住那个单向的失败方向。它们成为 UNKNOWN，并带上
// unknownReasonFor 从既有封闭枚举里选出的原因。
//
// 判定与可信度是两个字段，来源也不同：能判的那些可信度由求值引擎给出
// （mesh / CCNP 会降级），判不了的那些只剩这段观测本身可不可信。窗口完整度
// 两条路径都要传导，且只碰 confidence（design doc §4，见 transmitCompleteness）。
func (t traffic) attribute(c flow.Connection) attributed {
	a := attributed{conn: c}
	a.src, a.srcOutcome = t.subjectAt(c.Source)
	a.dst, a.dstOutcome = t.subjectAt(c.Dest)
	// 先拼出那个形状，两端暂时只有地址。求值只在两端都解得开时才发生，
	// 但**每一条**都要留下 flow —— 判不出来的那些也要进观测清单。
	a.flow = replay.Flow{
		Source:    replay.Endpoint{ClusterID: t.clusterID, IP: c.Source.IP},
		Dest:      replay.Endpoint{ClusterID: t.clusterID, IP: c.Dest.IP},
		Protocol:  replay.Protocol(c.Protocol),
		Port:      c.Port,
		Timestamp: t.at,
	}

	if reason := t.unknownReasonFor(a); reason != replay.ReasonNone {
		a.decision = t.unknown(reason, "")
		return a
	}

	srcPod, srcOK := t.podRefOf(a.src, c.Source.IP)
	dstPod, dstOK := t.podRefOf(a.dst, c.Dest.IP)
	if !srcOK || !dstOK {
		// 区间说那一刻这个地址上有 Pod，锚点那次快照里却找不到它。缺的是
		// 标签，而没有标签就比不了 selector。拿一份空标签去比会得出"没有
		// 任何策略选中它"，那是一个关于集群的结论，事实只是快照对不上。
		a.decision = t.unknown(replay.ReasonSnapshotMissing,
			"the interval and the snapshot covering that moment disagree about this endpoint")
		return a
	}
	a.flow.Source.Pod, a.flow.Dest.Pod = srcPod, dstPod

	engine := t.eval.Evaluate(a.flow)
	// **先取身份可信度，再传导完整度。** 顺序反了这个字段就永远是完整度的
	// 影子，而 policygen 会把 mesh 流量当成"只是证据不全"学进候选集。
	a.identityTrusted = engine.Confidence == replay.ConfidenceTrusted
	a.decision = t.transmitCompleteness(engine)
	return a
}

// unknownReasonFor 把两端的解析结果映射成一个封闭枚举里的原因。
//
// 取值全部来自 replay 那份既有的枚举，**一个都不新增**：新增取值要同步改
// 枚举、前端文案与统计口径。窗口完整度**不在这里** —— 它按 design doc §4
// 传导成 confidence，改写 verdict 会把一条算得出的 DENY 变成 UNKNOWN，
// 于是一个 DEGRADED 窗口下的预览报"会拦断 0 条"。
//
// 解不出主体的那一端还要再问一句"这个地址在不在 Fleet 里"，因为两个原因
// 登记的语义正是按这条线分的（replay/decision.go）：EXTERNAL_NO_IDENTITY 是
// "公网流量无可归属主体"，SNAPSHOT_MISSING 是"缺少对应时刻的资产快照"。
// 只看 identity.Outcome 分不开这两件事 —— 一个真正的公网地址一个区间都
// 没有，解析器答的是 NO_DATA；而集群内地址在两段区间之间的空档答的是
// NOT_COVERED。按 Outcome 直接映射会把两者对调，于是运维照着满屏
// SNAPSHOT_MISSING 去查"哪次采集漏了快照"，而根本没有这回事。
//
// 集群内的缺口压过公网：两端一个在集群内解不开、一个在公网时，前者是我们
// 数据里一个真实的洞，后者只是"对端本来就没有主体"。报前者，SNAPSHOT_MISSING
// 的统计口径才仍然只数真正的快照缺口。
func (t traffic) unknownReasonFor(a attributed) replay.UnknownReason {
	inClusterGap := func(outcome identity.Outcome, ip string) bool {
		return outcome != identity.OutcomeResolved && !t.externalAddress(ip)
	}
	switch {
	case a.srcOutcome == identity.OutcomeAmbiguous || a.dstOutcome == identity.OutcomeAmbiguous:
		return replay.ReasonIPAmbiguous
	case inClusterGap(a.srcOutcome, a.conn.Source.IP) || inClusterGap(a.dstOutcome, a.conn.Dest.IP):
		return replay.ReasonSnapshotMissing
	case a.srcOutcome != identity.OutcomeResolved || a.dstOutcome != identity.OutcomeResolved:
		// 剩下的只可能是"解不出主体、且地址不属于任何已登记网段"，
		// 那正是 EXTERNAL_NO_IDENTITY 登记的那件事。
		return replay.ReasonExternalNoIdentity
	default:
		return replay.ReasonNone
	}
}

// transmitCompleteness 把窗口完整度传导到一次**判得出来**的结论上。
//
// design doc §4 要求完整度传导对两条路径同时成立：判不出来的那些由 unknown()
// 传导，判得出来的这些必须在这里补上。少了这一步，一个已知漏了记录的窗口
// 会输出一句 TRUSTED 的"不会拦断任何连接"—— 比不给结论更糟，因为它读起来
// 是可信的。
//
// **只降不升**：求值引擎自己会因为 mesh 或 CCNP 降级（replay.confidenceFor），
// 那是与漏没漏采无关的另外两条理由。一个完整的窗口不得把引擎的 DEGRADED
// 提回 TRUSTED，否则完整度就成了可信度的唯一来源。
//
// verdict 一个字也不改（CLAUDE.md §3）：一次判定可以同时是"会拦断"与
// "不可信"，两者各说各的。
func (t traffic) transmitCompleteness(d replay.Decision) replay.Decision {
	if confidenceOf(t.completeness) == replay.ConfidenceDegraded {
		d.Confidence = replay.ConfidenceDegraded
	}
	return d
}

// unknown 构造一个判不出来的结论。
//
// MatchedRuleIdx 显式填 -1：零值是"命中了第 0 条规则"，而这条连接根本没有
// 进过求值 —— 一个凭空的命中下标会让界面指着一条与结论无关的规则。
func (t traffic) unknown(reason replay.UnknownReason, detail string) replay.Decision {
	return replay.Decision{
		Verdict:       replay.VerdictUnknown,
		Confidence:    confidenceOf(t.completeness),
		UnknownReason: reason,
		Reason:        replay.Reason{MatchedRuleIdx: -1, Detail: detail},
	}
}

// podRefOf 把一个解出来的主体补上快照里的标签，凑成求值引擎要的 Pod。
//
// ok 为 false 表示区间与快照对不上，调用方必须据此答 UNKNOWN，**不得**拿
// 一个只有名字、没有标签的 Pod 去求值。
//
// NamedPorts 留空：采集层没有落容器端口，命名端口于是解析不出来。这不是
// 遗漏而是如实 —— 求值引擎遇到解不开的命名端口会答 NAMED_PORT_UNRESOLVED，
// 那是它自己登记的原因，比猜一个端口号安全。
func (t traffic) podRefOf(id identity.Identity, ip string) (*replay.PodRef, bool) {
	p, ok := t.pods[podKey{namespace: id.Namespace, name: id.PodName}]
	if !ok {
		return nil, false
	}
	return &replay.PodRef{
		ClusterID:   t.clusterID,
		Namespace:   id.Namespace,
		Name:        id.PodName,
		IP:          ip,
		Labels:      p.labels,
		HostNetwork: id.HostNetwork,
		InMesh:      id.InMesh,
	}, true
}

// record 把一条判定过的连接映射成 API 形状。
//
// Flows 与 Flow 共用，安全发现也从它起手：字段列表在几处各写一份，改一处
// 漏改另一处的结果是同一条连接在列表里与下钻页显示成两件事。
func (t traffic) record(index int, a attributed) store.FlowRecord {
	return store.FlowRecord{
		ID: flowIDOf(t.clusterID, t.window, index, a.conn),
		// 时刻取窗口起点，不是"现在"：事实层按窗口存连接，flow.Connection
		// 没有逐条时间戳（见 described.at）。填一个更精确的时刻等于替来源
		// 说了一句它没说过的话。
		Timestamp:     t.at,
		SourceLabel:   t.endpointLabel(a.conn.Source.IP, a.src, a.srcOutcome),
		DestLabel:     t.endpointLabel(a.conn.Dest.IP, a.dst, a.dstOutcome),
		Protocol:      string(a.conn.Protocol),
		Port:          a.conn.Port,
		Verdict:       string(a.decision.Verdict),
		Confidence:    string(a.decision.Confidence),
		UnknownReason: string(a.decision.UnknownReason),
		CrossCluster: crossesCluster(t.fleet, t.clusterID, a.conn.Source.IP) ||
			crossesCluster(t.fleet, t.clusterID, a.conn.Dest.IP),
		Unmanaged: t.unmanaged(a),
	}
}

// unmanaged 报告这条连接上是否存在 NetworkPolicy 管不到的一端。
//
// 判不出结论时求值引擎不会填这个标记，但区间表已经说了这一端是 hostNetwork。
// 照实标出来：一条通往特权组件的连接渲染成普通放行，与"策略确实允许了它"
// 无法区分，而两者的处置手段完全不同。
func (t traffic) unmanaged(a attributed) bool {
	if a.decision.Reason.Unmanaged {
		return true
	}
	if a.srcOutcome == identity.OutcomeResolved && a.src.HostNetwork {
		return true
	}
	return a.dstOutcome == identity.OutcomeResolved && a.dst.HostNetwork
}

// endpointLabel 返回一端的可读标签。
//
// 归属解不出来时只给地址，**不给一个空的 namespace/名字**：一个写着
// "cluster//"的标签在界面上看起来像一个真实主体，而它什么都不是。
func (t traffic) endpointLabel(ip string, id identity.Identity, outcome identity.Outcome) string {
	if outcome != identity.OutcomeResolved {
		return ip
	}
	return fmt.Sprintf("%s/%s/%s", t.clusterID, id.Namespace, id.PodName)
}

// externalAddress 报告一个地址**没有**落进 Fleet 里任何一段已登记网段。
//
// AMBIGUOUS 不算：它命中了两个集群的登记网段，那是一次登记重叠，不是出网。
// POD / SERVICE / NODE 同理，它们明确属于某个已登记集群。
//
// UNKNOWN 算，而它正是本方法最需要解释的一条。internal/cluster 在登记有缺口
// 时拒绝答 EXTERNAL —— 那条规则是对的，它挡住的是"把我们没登记讲成它在集群
// 外"。但这份报告要回答的不是"它一定在公网"，而是"这个目标要不要有人去看"，
// 而两种错法的代价完全不对称：
//
//   - 把一个未登记的内网段当成出网目标，代价是一条误报，地址就摆在那里，
//     看一眼就能排除；
//   - 漏掉一个真的出网目标，代价是一条通往公网的路径从报告里整个消失，
//     而空着的那一节读起来正是"这个集群没有出网敞口"。
//
// 与 FixtureReader.isExternal 是同一条规则（既无主体、也不属于任何集群），
// 两个 Reader 喂同一个前端，口径必须一致。
//
// **当前 registry.Cluster 没有 Service 网段字段**，因此登记缺口恒在、
// Classify 恒答 UNKNOWN，本方法实际认的就是"不在任何已登记 Pod/Node 网段
// 里"。补上 Service 网段登记之后，这里会自动收敛到 EXTERNAL 那一支。
func (t traffic) externalAddress(ip string) bool {
	if ip == "" {
		return false
	}
	c, err := t.fleet.Classify(ip)
	if err != nil {
		return false
	}
	switch c.Scope {
	case cluster.ScopeExternal, cluster.ScopeUnknown:
		return true
	case cluster.ScopePod, cluster.ScopeService, cluster.ScopeNode, cluster.ScopeAmbiguous:
		return false
	default:
		return false
	}
}

// foreignScopesOf 把快照里的覆盖范围翻成求值器认的形状。
//
// 两个类型分别属于快照层与纯求值层，各自独立演进：求值器不该知道这些范围
// 是从 CiliumNetworkPolicy 的 endpointSelector 里抽出来的，快照层也不该
// 依赖求值器的选项类型。
func foreignScopesOf(in []snapshot.ForeignScope) []replay.ForeignScope {
	out := make([]replay.ForeignScope, 0, len(in))
	for _, sc := range in {
		out = append(out, replay.ForeignScope{
			Namespace: sc.Namespace,
			// MatchLabels 为空即"选中该范围内全部主体"，与 LabelSelector
			// 值类型的空语义一致 —— 不必特判。
			Selector: metav1.LabelSelector{MatchLabels: sc.MatchLabels},
		})
	}
	return out
}

// currentImports 读出要并进当前策略集的人工导入（RoleBaselineCurrent）。
//
// **只取导入时刻早于窗口起点的那些。** 一次导入能证明的只是"我在这个时刻
// 告诉你集群里有这条策略"，它没有说过那条策略更早就存在。拿它去解释更早的
// 窗口，等于替人补上一句他没说过的话（CLAUDE.md §4：禁止用当前状态解释
// 历史数据）。代价是刚导入时最近那一个窗口看不到效果，下一个窗口就有了。
//
// 解析失败跳过那一条并继续，与 candidateImports 同一处置：导入在写入时已经
// 过 registry.ParseImport 校验，走到这里还解析不了说明那条记录本身坏了。
func (r *Reader) currentImports(
	ctx context.Context, d described,
) ([]networkingv1.NetworkPolicy, error) {
	stored, err := r.src.PolicyImports(ctx, d.clusterID)
	if err != nil {
		return nil, err
	}
	out := make([]networkingv1.NetworkPolicy, 0, len(stored))
	for _, imp := range stored {
		if imp.Role != registry.RoleBaselineCurrent {
			continue
		}
		if imp.ImportedAt.After(d.at) {
			continue
		}
		parsed, err := registry.ParseImport(imp.YAML)
		if err != nil {
			continue
		}
		out = append(out, parsed.Policy)
	}
	return out, nil
}
