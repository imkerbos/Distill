package flow

import (
	"fmt"

	"github.com/imkerbos/Distill/internal/identity"
)

// Protocol 是连接的传输层协议，封闭枚举。
//
// 取值与 replay.Protocol 逐字相同但不 import：本包是摄入侧的契约，把求值
// 引擎的类型拉进来等于让两层共享内部状态（CLAUDE.md §6），映射由边缘层做。
type Protocol string

const (
	// ProtocolTCP 是 TCP。
	ProtocolTCP Protocol = "TCP"
	// ProtocolUDP 是 UDP。
	ProtocolUDP Protocol = "UDP"
	// ProtocolSCTP 是 SCTP。
	ProtocolSCTP Protocol = "SCTP"
)

// Valid 判断该协议是否已登记。零值不合法。
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolTCP, ProtocolUDP, ProtocolSCTP:
		return true
	default:
		return false
	}
}

// Verdict 是来源报告的实际放行/拒绝，封闭枚举。
//
// 没有"未报告"这个取值：未报告就是零值，而零值不合法，读取一律走
// Connection.Verdict 的第二个返回值。Hubble 报判定，VPC flow logs 不报 ——
// 把不报当放行，会让一批实际被网络拒掉的连接变成正常业务流量的证据，
// 进而生成一条"允许"规则，把现网的拒绝变成允许（spec §4）。
type Verdict string

const (
	// VerdictAllowed 表示来源明确报告这条连接被放行。
	VerdictAllowed Verdict = "ALLOWED"
	// VerdictDenied 表示来源明确报告这条连接被拒绝。
	VerdictDenied Verdict = "DENIED"
)

// Valid 判断该判定是否已登记。零值不合法 —— 它表示来源根本没报。
func (v Verdict) Valid() bool {
	switch v {
	case VerdictAllowed, VerdictDenied:
		return true
	default:
		return false
	}
}

// Endpoint 是一条连接的一端。
//
// 地址是唯一必有的东西，身份可以整个缺席：Hubble 的流量自带 Pod 标签，
// VPC flow logs 只有 IP，必须走 C 轮的解析器还原（spec §3）。身份字段
// 不导出，是为了让"拿主体却不看可信度"写不出来。
type Endpoint struct {
	// IP 是这一端的地址。
	IP string

	subject    identity.Identity
	confidence identity.Outcome
}

// WithIdentity 附上一次身份解析的结果。
//
// outcome 不是 RESOLVED 时主体一律丢弃：AMBIGUOUS 时任选一个仍然查得出
// 结果、仍然不报错，而错的那次没有任何症状（与 identity.Resolve 同一条
// 规则）。未登记的 outcome 同样按"没解析过"处理 —— 一个平台不认识的
// 可信度不得成为使用这个主体的依据。
func (e Endpoint) WithIdentity(subject identity.Identity, outcome identity.Outcome) Endpoint {
	out := Endpoint{IP: e.IP}
	if !outcome.Valid() {
		return out
	}
	out.confidence = outcome
	if outcome == identity.OutcomeResolved {
		out.subject = subject
	}
	return out
}

// Identity 返回这一端的主体与这次解析的可信度（spec §4 的
// identity_confidence，逐条而非一个全局值）。
//
// 两个值一起返回：只取主体、不看可信度的调用写不出来 —— 未解析的端点
// 主体是零值，把它当成"某个空 namespace 里的负载"会静默对错账。
//
// 可信度不合法（含零值：从没解析过）时一律报 NO_DATA。说成 NOT_COVERED
// 才是那个让人放心的方向：后者是"那时这个 IP 确实没有 Pod"，是下游会
// 当作事实使用的结论，而我们这里只是没解析过。
func (e Endpoint) Identity() (identity.Identity, identity.Outcome) {
	if !e.confidence.Valid() {
		return identity.Identity{}, identity.OutcomeNoData
	}
	return e.subject, e.confidence
}

// Connection 是一个时间窗内观测到的一条连接 —— 不是一个报文。
//
// 按连接而非按报文，是因为判定引擎的输入单位就是连接，而按报文存，体量
// 是连接数的几个数量级倍，多出来的信息判定层一条也用不上（spec §4）。
type Connection struct {
	// Source 是发起端。
	Source Endpoint
	// Dest 是目的端。
	Dest Endpoint
	// Protocol 是传输层协议。
	Protocol Protocol
	// Port 是目的端口。
	Port int32
	// ObservedCount 是该窗口内看到这条连接多少次。
	//
	// 它不是完整度：看见 3 次只说明至少发生过 3 次，采样下的 0 与真正
	// 没有发生过的 0 长得一模一样，那件事由 IngestResult 的完整度回答。
	ObservedCount int

	verdict Verdict
}

// WithVerdict 附上来源报告的判定。
//
// 未登记的取值（含零值）一律按"来源没报"处理：用一个平台不认识的字符串
// 决定放行与否，比承认不知道更危险。
func (c Connection) WithVerdict(v Verdict) Connection {
	if !v.Valid() {
		c.verdict = ""
		return c
	}
	c.verdict = v
	return c
}

// Verdict 返回来源报告的判定，reported 为 false 表示来源根本没报。
//
// 两个值一起返回，而不是把"没报"编进枚举：调用方必须先接住 reported,
// 才拿得到判定，于是 `if v != VerdictDenied { 当成放行 }` 这种写法在类型
// 层面就少了一个可以被忽略的位置。空与放行是两件事（spec §4）。
func (c Connection) Verdict() (v Verdict, reported bool) {
	if !c.verdict.Valid() {
		return "", false
	}
	return c.verdict, true
}

// Completeness 是一次摄入的完整度，封闭枚举。
type Completeness string

const (
	// CompletenessComplete 表示来源明确证明了这个窗口没有漏：实际覆盖到了
	// 请求的整段时间、采样率为 1、且报告丢弃为 0。三项缺一不可。
	CompletenessComplete Completeness = "COMPLETE"
	// CompletenessDegraded 表示已知漏了：窗口没覆盖满、有采样、或有丢弃。
	CompletenessDegraded Completeness = "DEGRADED"
	// CompletenessUnknown 表示完整度无从判断 —— 来源没给出足以证明"没漏"
	// 的元数据（Hubble 的丢弃计数在 relay 层不总是可得，spec §5）。
	//
	// 与 DEGRADED 分开的理由与 identity 的 NOT_COVERED / NO_DATA 同源：
	// "知道漏了"与"不知道漏没漏"是两条不同的排查线索，合成一个会让前者
	// 的排查方向糊掉。但对下游判定而言两者一视同仁 —— 都不是完整，都必须
	// 降级。零值 IngestResult 落在这一档。
	CompletenessUnknown Completeness = "UNKNOWN"
)

// Valid 判断该完整度是否已登记。零值不合法：一份没被赋过值的完整度不得
// 被当成任何一种结论，尤其不得被当成 COMPLETE。
func (c Completeness) Valid() bool {
	switch c {
	case CompletenessComplete, CompletenessDegraded, CompletenessUnknown:
		return true
	default:
		return false
	}
}

// IngestResult 是一次摄入的结果：看见了哪些连接，以及这段观测有多可信。
//
// 字段全部不导出，且没有任何设置完整度的途径：完整度不是一个可以被填写
// 的字段，而是证据的函数（见 completeness）。能被填写就能被填成 COMPLETE
// 而没人给过依据，下游据此不降级，于是一批没被看见的连接被当成不存在，
// 覆盖它们的规则被判"可收紧"（spec §2）。
//
// 零值是一次"什么都没说"的摄入：没有连接、完整度 UNKNOWN、采样率未知、
// 丢弃数未知。它绝不读作"这段时间没有流量且一条没漏"。
type IngestResult struct {
	source SourceKind
	// requested 是调用方要的窗口，covered 是来源实际覆盖到的窗口。两者
	// 分开，是因为"我要一小时"与"它只给了我 40 分钟"必须能区分。
	requested Window
	covered   Window

	conns []Connection

	sampleRate      float64
	sampleRateKnown bool

	dropped         uint64
	droppedReported bool
}

// NewIngestResult 构造一次摄入的结果。
//
// covered 传零值表示来源说不出自己实际覆盖了多少 —— 那是未知，不是全
// 覆盖，完整度据此落到 UNKNOWN。
//
// source 与 requested 不合法直接报错而不是静默降级：这两项是调用方自己
// 就该知道的事，错了说明摄入器写坏了，而不是来源给不出元数据。
func NewIngestResult(source SourceKind, requested, covered Window, conns []Connection) (IngestResult, error) {
	if !source.Valid() {
		return IngestResult{}, fmt.Errorf("flow: unregistered source kind %q", string(source))
	}
	if !requested.Valid() {
		return IngestResult{}, fmt.Errorf("flow: invalid requested window")
	}
	if covered != (Window{}) && !covered.Valid() {
		return IngestResult{}, fmt.Errorf("flow: invalid covered window")
	}
	return IngestResult{
		source:    source,
		requested: requested,
		covered:   covered,
		conns:     conns,
	}, nil
}

// WithSampleRate 记下来源报告的采样率。
//
// 只接受 (0,1] 内的值，越界一律保持未知：一个算错的采样率会让完整度变成
// 一句没有依据的话。取不到采样率时就别调用它 —— **不得填 1.0**，那等于
// 宣称"一条没漏"，而那是一句没人说过的话（spec §5）。
func (r IngestResult) WithSampleRate(rate float64) IngestResult {
	if rate <= 0 || rate > 1 {
		return r
	}
	r.sampleRate = rate
	r.sampleRateKnown = true
	return r
}

// WithDropped 记下来源报告的丢弃条数。来源不报时就别调用它：0 与"不报"
// 必须分开，前者是证据，后者是没有证据。
func (r IngestResult) WithDropped(n uint64) IngestResult {
	r.dropped = n
	r.droppedReported = true
	return r
}

// Source 返回这批连接的来源。
func (r IngestResult) Source() SourceKind { return r.source }

// Windows 返回请求的窗口与来源实际覆盖到的窗口。covered 不合法表示来源
// 说不出自己覆盖了多少。
func (r IngestResult) Windows() (requested, covered Window) { return r.requested, r.covered }

// SampleRate 返回来源报告的采样率，known 为 false 表示取不到。
//
// known 为 false 时第一个返回值没有任何含义 —— 尤其不是 1.0。调用方必须
// 改看完整度。
func (r IngestResult) SampleRate() (rate float64, known bool) {
	if !r.sampleRateKnown {
		return 0, false
	}
	return r.sampleRate, true
}

// Dropped 返回来源报告的丢弃条数，reported 为 false 表示来源不报。
func (r IngestResult) Dropped() (n uint64, reported bool) {
	if !r.droppedReported {
		return 0, false
	}
	return r.dropped, true
}

// Connections 返回窗口内观测到的连接，以及这批观测的完整度。
//
// 两个值一起返回而不是拆成两个方法：一个"只取连接、不看完整度"的调用方
// 就是把一份可能缺了连接的观测当成完整事实在用，而那正是 spec §2 里那个
// 单向错误的入口。取不到完整度，就写不出那种调用。
func (r IngestResult) Connections() ([]Connection, Completeness) {
	return r.conns, r.completeness()
}

// completeness 由证据算出。
//
// 判定顺序是刻意的：已知的缺口压过未知 —— 覆盖不全是我们自己就能看出来
// 的漏，不必等来源交采样率。而"没有任何一项证据"永远落在 UNKNOWN，
// 只有三项证据齐备且都指向没漏时才是 COMPLETE。
func (r IngestResult) completeness() Completeness {
	if !r.source.Valid() || !r.requested.Valid() || !r.covered.Valid() {
		// 连自己覆盖了哪段时间都说不出来，"没漏"就无从谈起。零值走这一条。
		return CompletenessUnknown
	}
	if !r.covered.Covers(r.requested) {
		return CompletenessDegraded
	}
	rate, rateKnown := r.SampleRate()
	if rateKnown && rate < 1 {
		return CompletenessDegraded
	}
	dropped, droppedReported := r.Dropped()
	if droppedReported && dropped > 0 {
		return CompletenessDegraded
	}
	if !rateKnown || !droppedReported {
		return CompletenessUnknown
	}
	return CompletenessComplete
}
