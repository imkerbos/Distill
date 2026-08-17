package hubble

import (
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	"github.com/cilium/cilium/api/v1/observer"
	relaypb "github.com/cilium/cilium/api/v1/relay"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
)

// connKey 是一条连接的身份：两端地址、协议、目的端口，外加来源报告的判定。
//
// **判定进 key，而不是事后把一个五元组的多条 flow 合并成一个判定。** 同一个
// 五元组在一个窗口里既可能被放行也可能被拒（策略中途生效、只有一部分报文
// 撞上规则）。合并就必须选一个赢家，而两个方向都有害：选 DENIED 会让一条
// 其实还在跑的连接被读成"已经断了、切掉不心疼"；选 ALLOWED 会把现网的拒绝
// 写成允许，进而生成一条放行规则（spec §4）。分成两条连接，两件事都不丢，
// 也就不必选。
type connKey struct {
	srcIP    string
	dstIP    string
	protocol flow.Protocol
	port     int32
	// verdict 为空表示这一批 flow 来源根本没报判定 —— 与"放行"是两件事。
	verdict flow.Verdict
}

// pending 是一条连接累计到的观测。
type pending struct {
	count int
	// src / dst 的身份只在 Hubble 真的带了 Pod 标签时才落上。known 为 false
	// 的那一端**不调用 WithIdentity**，于是 Endpoint.Identity() 报 NO_DATA。
	src      identity.Identity
	srcKnown bool
	dst      identity.Identity
	dstKnown bool
}

// accumulator 把一条 flow 流累成一次摄入的结果。
//
// 只累加证据，不下结论：完整度由 internal/flow 从证据算出，本类型没有任何
// 途径直接宣称"完整"。
type accumulator struct {
	order []connKey
	conns map[connKey]*pending

	dropped         uint64
	droppedReported bool

	// nodeGap 记的是 relay 明确说了某个节点不在线。它不是一个时间上的缺口，
	// 因此表达成"覆盖窗口说不出来"（UNKNOWN），而不是硬把窗口截短。
	nodeGap bool

	truncated  bool
	lastFlowAt time.Time
}

// newAccumulator 构造一个空的累加器。
func newAccumulator() *accumulator {
	return &accumulator{conns: map[connKey]*pending{}}
}

// observe 吃掉一条 relay 消息，返回是否已达到读取上限。
func (a *accumulator) observe(resp *observer.GetFlowsResponse) (full bool) {
	switch r := resp.GetResponseTypes().(type) {
	case *observer.GetFlowsResponse_Flow:
		a.observeFlow(r.Flow)
	case *observer.GetFlowsResponse_LostEvents:
		// 只有 relay 真的报了才记。**不报时绝不记 0** —— 0 是"确实没丢"的
		// 证据，而没报是没有证据，两者混起来就成了一句没人说过的"没漏"。
		a.dropped += r.LostEvents.GetNumEventsLost()
		a.droppedReported = true
	case *observer.GetFlowsResponse_NodeStatus:
		// 只有 NODE_CONNECTED 说明这个节点的流量能看见。其余取值（不可用、
		// 已移除、报错、以及 relay 自己都说不清的 UNKNOWN）一律当作缺口 ——
		// 这一层认不出的形态必须往"漏了"的方向倒。
		if r.NodeStatus.GetStateChange() != relaypb.NodeState_NODE_CONNECTED {
			a.nodeGap = true
		}
	}
	if len(a.order) >= maxFlowsPerIngest {
		a.truncated = true
		return true
	}
	return false
}

// observeFlow 把一条 flow 折进对应的连接。
//
// **`is_reply` 为 true 的一律丢弃。** 回程报文的五元组是反着的，照收会凭空
// 造出一条 `B:8080 → A:54321` 的"连接"。多出来的行数只是便宜的那一半：贵的
// 那一半是策略生成器把事实层读作"存在的连接"，于是给 B 生成一条指向 A 临时
// 端口的 egress 规则 —— 那不是噪声，是错的输出，而且错得让运维没有任何办法
// 认出它是摄入产生的假象。
//
// **`is_reply` 缺失（BoolValue 为 nil）的一律保留。** 缺失是"不知道方向"，
// 不是"不是回程"。把不知道当成 true 会丢掉真实存在的连接，而漏一条连接 →
// 平台认为它不存在 → 覆盖它的规则被判可收紧 → 推荐一份切断它的策略，正是
// 本轮存在的理由（spec §2）。多报活得下去，少报会推荐一次切断。
//
// 这两句话必须一起读：下一个看到行数的人会想把缺失的那批也一并丢掉。
func (a *accumulator) observeFlow(f *flowpb.Flow) {
	if f.GetIsReply().GetValue() {
		return
	}

	proto, port, ok := protocolOf(f.GetL4())
	if !ok {
		// 没有 L4 端口的 flow（ICMP、VRRP、IGMP 等）不构成本平台意义上的
		// 连接：判定引擎的输入单位是 (协议, 端口)。
		return
	}
	srcIP, dstIP := f.GetIP().GetSource(), f.GetIP().GetDestination()
	if srcIP == "" || dstIP == "" {
		return
	}

	verdict, _ := verdictOf(f.GetVerdict())
	key := connKey{srcIP: srcIP, dstIP: dstIP, protocol: proto, port: port, verdict: verdict}

	p, seen := a.conns[key]
	if !seen {
		p = &pending{}
		a.conns[key] = p
		a.order = append(a.order, key)
	}
	p.count++

	// 身份只补一次，且只在还没补上时补：同一条连接的多条 flow 里，可能只有
	// 一部分带全了标签。补上过就不再动，避免后一条标签缺失的 flow 把已经
	// 拿到的主体抹掉。
	if !p.srcKnown {
		p.src, p.srcKnown = identityOf(f.GetSource())
	}
	if !p.dstKnown {
		p.dst, p.dstKnown = identityOf(f.GetDestination())
	}

	if t := f.GetTime(); t != nil {
		if at := t.AsTime(); at.After(a.lastFlowAt) {
			a.lastFlowAt = at
		}
	}
}

// result 把累计到的证据组装成一次摄入的结果。
//
// **从头到尾没有调用 WithSampleRate。** Hubble 不报采样率，因此这里没有任何
// 依据可以填 —— 填 1.0 等于宣称"一条没漏"，而那正是 spec §5 点名禁止的那句
// 没人说过的话。于是本来源永远到不了 COMPLETE，这是对的：它确实证明不了。
func (a *accumulator) result(window flow.Window) (flow.IngestResult, error) {
	conns := make([]flow.Connection, 0, len(a.order))
	for _, key := range a.order {
		p := a.conns[key]
		c := flow.Connection{
			Source:        endpointOf(key.srcIP, p.src, p.srcKnown),
			Dest:          endpointOf(key.dstIP, p.dst, p.dstKnown),
			Protocol:      key.protocol,
			Port:          key.port,
			ObservedCount: p.count,
		}
		// verdict 为空时 WithVerdict 会保持"来源没报"，不会变成放行。
		conns = append(conns, c.WithVerdict(key.verdict))
	}

	res, err := flow.NewIngestResult(flow.SourceHubble, window, a.covered(window), conns)
	if err != nil {
		return flow.IngestResult{}, err
	}
	if a.droppedReported {
		res = res.WithDropped(a.dropped)
	}
	return res, nil
}

// covered 报告来源实际覆盖到的窗口。
//
// 三种情形，刻意分开：
//
//   - 被读取上限截断：我们知道自己在某个时刻停下了，覆盖窗口据此缩到那一刻，
//     盖不住请求窗口 → DEGRADED。**停下的时刻没被量到时退回"说不出覆盖"
//     （UNKNOWN），这是裁定，不是遗漏**：本层能表达"已知漏了"的通道只有覆盖
//     窗口，而说得出 DEGRADED 就得说出一个具体的截止时刻；没量到还硬填一个，
//     就是编一个我们没有的事实 —— 与下面 nodeGap 那一支拒绝硬把时间窗截短
//     同源。两档对下游一视同仁，差别只在排查线索，而一个编出来的截止时刻会把
//     那条线索指向一段从没被测量过的时间。见 spec §5 的 2026-08-17 补记。
//   - relay 报了节点缺口：漏掉的是某几个节点，不是某一段时间，硬把时间窗
//     截短是在编一个我们没有的事实 → 覆盖窗口留空，落 UNKNOWN。
//   - 其余：relay 接受了 since/until 并把这段范围流完，覆盖窗口即请求窗口。
//     这**不是**一句"没漏" —— 完整度还要求采样率已知且丢弃已报告，而 Hubble
//     两样都给不齐，所以常态仍然是 UNKNOWN。
func (a *accumulator) covered(window flow.Window) flow.Window {
	if a.truncated {
		if a.lastFlowAt.After(window.From) && a.lastFlowAt.Before(window.To) {
			return flow.Window{From: window.From, To: a.lastFlowAt}
		}
		return flow.Window{}
	}
	if a.nodeGap {
		return flow.Window{}
	}
	return window
}

// endpointOf 组装一端，身份缺失时**不附身份**。
//
// 不附身份的那一端，Endpoint.Identity() 会报 NO_DATA 而不是 NOT_COVERED：
// 后者的意思是"那一刻这个 IP 确实没有 Pod"，是下游会当事实用的结论；而这里
// 的实情只是 Hubble 这一条 flow 上没有标签 —— 对端可能真在集群外，也可能是
// 标签没带上，我们分不出来，就不得替它选一个（spec §3）。
func endpointOf(ip string, id identity.Identity, known bool) flow.Endpoint {
	ep := flow.Endpoint{IP: ip}
	if !known {
		return ep
	}
	return ep.WithIdentity(id, identity.OutcomeResolved)
}

// identityOf 从 Hubble 的 endpoint 还原主体，还原不出时返回 false。
//
// Hubble 的 flow 自带 Pod 标签，这是它相对 VPC flow logs 的便利（spec §3）；
// 但便利不等于总是有。namespace 与 pod 名缺一不可 —— 只有其中一个的时候，
// 拼出来的主体在对账里会静默匹配到别的东西上。
//
// **已知缺陷，不是可以忽略的细节：`HostNetwork` 在这里是一个没人观测过的
// `false`。** Hubble 的 Endpoint 消息里没有这一项，而 identity.Identity 把它
// 声明成普通 bool —— 类型上根本没有"未观测"这个取值，要有就得改
// internal/identity，而那是一个必须保持纯净、不在本任务范围内的包。于是这里
// 只能留下一个读起来像观测结果的默认值：一个 hostNetwork Pod 若被 Hubble 带上
// 了 pod 名，这条连接会说它受 NetworkPolicy 管辖，而实际上不受。
// **凡是要用这一位的下游，权威来源一律是资产侧（internal/snapshot →
// internal/identity），不得用流量侧的这一位。**
//
// PodUID 与 InMesh 同样留零值，但性质不同、可以就这么放着：Hubble 不带它们，
// 这两个字段是给别的来源用的，且今天没有任何下游从流量侧读它们 ——
// 全部读取点都在资产侧（internal/store、internal/baseline、internal/snapshotstore）。
func identityOf(ep *flowpb.Endpoint) (identity.Identity, bool) {
	ns, pod := ep.GetNamespace(), ep.GetPodName()
	if ns == "" || pod == "" {
		return identity.Identity{}, false
	}
	id := identity.Identity{Namespace: ns, PodName: pod}
	// 只取第一个 workload：Hubble 给的是一条 ownerRef 链，第一个是顶层控制器。
	// 对账一律用 workload 而非 pod（CLAUDE.md §4）。
	if w := ep.GetWorkloads(); len(w) > 0 {
		id.WorkloadKind, id.WorkloadName = w[0].GetKind(), w[0].GetName()
	}
	return id, true
}

// verdictOf 把 Hubble 的判定映射到平台的封闭枚举，映射不出时返回 false。
//
// 只有 FORWARDED 与 DROPPED 是对这条连接下的结论。其余取值一律**留空**，
// 而不是当作放行：
//
//   - TRACED / TRANSLATED：proto 自己写着"还没有得出判定"。
//   - REDIRECTED：转给了代理，不是终局。
//   - ERROR：处理过程出错，什么都没证明。
//   - AUDIT：审计模式下"本来会被拒"。报文确实过去了，但把它记成 ALLOWED
//     会让一条只因审计模式才通的连接变成"现网允许"的证据，而审计模式一关
//     它就断 —— 那正是空与放行必须分开的理由（spec §4）。
//   - VERDICT_UNKNOWN：来源明说自己不知道。
func verdictOf(v flowpb.Verdict) (flow.Verdict, bool) {
	switch v {
	case flowpb.Verdict_FORWARDED:
		return flow.VerdictAllowed, true
	case flowpb.Verdict_DROPPED:
		return flow.VerdictDenied, true
	default:
		return "", false
	}
}

// protocolOf 取出传输层协议与目的端口，认不出时返回 false。
//
// 端口越界一律认不出：一个 0 端口的"连接"在判定层会匹配到别的东西上。
func protocolOf(l4 *flowpb.Layer4) (flow.Protocol, int32, bool) {
	var proto flow.Protocol
	var port uint32

	switch p := l4.GetProtocol().(type) {
	case *flowpb.Layer4_TCP:
		proto, port = flow.ProtocolTCP, p.TCP.GetDestinationPort()
	case *flowpb.Layer4_UDP:
		proto, port = flow.ProtocolUDP, p.UDP.GetDestinationPort()
	case *flowpb.Layer4_SCTP:
		proto, port = flow.ProtocolSCTP, p.SCTP.GetDestinationPort()
	default:
		return "", 0, false
	}
	if port == 0 || port > 65535 {
		return "", 0, false
	}
	return proto, int32(port), true
}
