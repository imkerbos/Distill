// Package conntrack 解析 Linux 的 /proc/net/nf_conntrack。
//
// **纯包**：吃 io.Reader、吐 flow.Connection，零文件 I/O、零框架依赖。
// 打开文件与轮询是 agent 的事（cmd/distill-agent）。
//
// 这样分是因为解析的产物会变成推荐给生产的 NetworkPolicy 规则 —— 一条读错的
// 连接会变成一条错的规则。这一层必须能被纯测试逐行钉住，而不是藏在一个要
// 挂载 /proc 才跑得起来的函数里。
package conntrack

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/imkerbos/Distill/internal/flow"
)

// Limit 是一次解析允许携带的去重后连接数上限。
type Limit struct {
	// Max 是上限。非正值按 defaultMax 处理 —— 一次不设上限的解析会在一个
	// 繁忙节点上把内存吃掉，而那不是一个应该靠调用方记得的参数。
	Max int
}

// defaultMax 是没给上限时的取值。
//
// 与 snapshotstore.maxIngestConnections（50000）同阶但更小：那一道是落库的
// 上限，这一道是单个**节点**的。一个节点贡献五万条去重后的服务对，说明这个
// 集群需要分片摄入，而不是说明这道上限该抬高。
const defaultMax = 20000

// Table 是一次解析的产物。
//
// 三个计数都在：**跳过的条数必须能被看见**。一个静默跳过的解析器坏掉时，
// 症状与"这个集群很安静"一模一样，而后者会让下游把"没有流量"读成
// "覆盖这些连接的规则可以收紧"。
type Table struct {
	// Connections 是去重后的连接，键是 (源, 目的, 协议, 目的端口)。
	Connections []flow.Connection
	// Truncated 表示超过上限、有连接没被带上。
	//
	// 与 Dropped 分成两个字段：调用方用它决定**要不要报** dropped，而
	// 报一个 0 等于宣称"一条没漏" —— 轮询 conntrack 永远说不出那句话
	// （design doc 2026-08-19-conntrack-source §5）。
	Truncated bool
	// Dropped 是因为超限而没被带上的去重后连接数。未截断时为 0。
	Dropped uint64
	// SkippedProtocol 是协议不是 TCP/UDP/SCTP 而被跳过的条数。
	//
	// ICMP 落在这里。default-deny 会拦住 ICMP，而 NetworkPolicy v1 表达不了
	// 它 —— 平台既学不到、也推荐不出放行它的规则。这是 NetworkPolicy 本身的
	// 边界，不是本包引入的，但它必须有个数字，否则某天会以 ping 不通的形式
	// 被发现（design doc §6.1）。
	SkippedProtocol uint64
	// SkippedUnreplied 是 TCP 且 [UNREPLIED] 而被跳过的条数。
	SkippedUnreplied uint64
	// SkippedLocal 是回环或源等于目的而被跳过的条数。
	SkippedLocal uint64
	// SkippedMalformed 是读不懂的行数。
	SkippedMalformed uint64
}

// key 是去重键。**临时源端口不进键** —— 那是主要的收敛：一个连接密集的
// 节点上同一个服务对会有成千上万个源端口，不去重会吐出同样多条一模一样的
// 规则（同 ScrapeTargetSnapshots 那次 506 条只有 20 个指纹）。
type key struct {
	src   string
	dst   string
	proto flow.Protocol
	port  int32
}

// Parse 解析一份 nf_conntrack 转储。
//
// 只在读取本身失败时返回错误。**单行读不懂只跳过那一行** —— 一行读不懂
// 不该让整个节点的观测消失，而那正是"不报错也不报数"的实现会造成的后果，
// 因此跳过的条数一并返回。
func Parse(r io.Reader, limit Limit) (Table, error) {
	max := limit.Max
	if max <= 0 {
		max = defaultMax
	}

	var tbl Table
	counts := map[key]int{}
	var order []key

	sc := bufio.NewScanner(r)
	// conntrack 的行可以很长（IPv6 加上一串标记），默认 64KiB 的缓冲够用，
	// 但显式给出来：一行超长时 Scanner 会直接报错，而那会让整个节点的
	// 观测消失。
	sc.Buffer(make([]byte, 0, 64<<10), 512<<10)
	for sc.Scan() {
		c, ok := parseLine(sc.Text(), &tbl)
		if !ok {
			continue
		}
		k := key{src: c.Source.IP, dst: c.Dest.IP, proto: c.Protocol, port: c.Port}
		if _, seen := counts[k]; !seen {
			if len(order) >= max {
				// 截断，不是拒绝：整份拒绝会让这段窗口一行都不落，读起来是
				// "没有流量"（flow-ingest spec §4）。丢掉多少条如实报出来。
				tbl.Truncated = true
				tbl.Dropped++
				continue
			}
			order = append(order, k)
		}
		counts[k]++
	}
	if err := sc.Err(); err != nil {
		return Table{}, fmt.Errorf("conntrack: read the table: %w", err)
	}

	tbl.Connections = make([]flow.Connection, 0, len(order))
	for _, k := range order {
		c := flow.Connection{
			Source:        flow.Endpoint{IP: k.src},
			Dest:          flow.Endpoint{IP: k.dst},
			Protocol:      k.proto,
			Port:          k.port,
			ObservedCount: counts[k],
		}
		// **TCP 报 ALLOWED，其余不报。** 上面已经把 [UNREPLIED] 的 TCP 条目
		// 丢掉了，因此能走到这里的每一条 TCP 握手都完成过——而握手完成意味着
		// 双向都通，任何一条 NetworkPolicy 拦下它都不可能有这个结果。
		//
		// 这是 conntrack 唯一给得出的判定，也是 dry-run 唯一的证伪手段：
		// 平台判 DENY 而这里报 ALLOWED，就是 DISAGREE_UNDER_PERMISSIVE ——
		// 平台以为这条本来就不通，于是不为它生成放行规则，候选集下发后它
		// 从通变成不通，而 dry-run 把它算成 UNCHANGED（internal/reconcile）。
		// 不报的话每一条都落进 SOURCE_SILENT，平台永远答不出自己判得对不对。
		//
		// **UDP / SCTP 不报。** 没有握手，单向条目（syslog、statsd、metrics
		// push）天然 unreplied 且被刻意保留，证明不了对端收到过。报 ALLOWED
		// 是拿"我发过"冒充"它通了"，那个假证据会污染一致率。
		if k.proto == flow.ProtocolTCP {
			c = c.WithVerdict(flow.VerdictAllowed)
		}
		tbl.Connections = append(tbl.Connections, c)
	}
	return tbl, nil
}

// parseLine 解一行。第二个返回值为 false 表示这一行不产出连接，
// 原因已经计入 tbl。
func parseLine(line string, tbl *Table) (flow.Connection, bool) {
	fields := strings.Fields(line)
	// 最短的一条有用行：协议族、族号、协议名、协议号、超时，再加两个元组
	// 各四项。短于此一律读不懂。
	if len(fields) < 13 {
		tbl.SkippedMalformed++
		return flow.Connection{}, false
	}

	proto, ok := protocolOf(fields)
	if !ok {
		// 协议名认不出来分两种：NetworkPolicy 表达不了的（icmp、gre……），
		// 与这一行压根不是 conntrack 行。用第三列在不在已知协议名里区分不了
		// 两者，因此按"表达不了"计 —— 那一栏偏大好过畸形那一栏偏大：
		// 前者是一句关于集群的话，后者会让人去查解析器。
		if len(fields) > 2 && isProtocolName(fields[2]) {
			tbl.SkippedProtocol++
		} else {
			tbl.SkippedMalformed++
		}
		return flow.Connection{}, false
	}

	// 两个元组：先出现的是原始方向，后出现的是回复方向。按出现顺序取
	// 第一个与第二个 src=，而不是按位置索引 —— [UNREPLIED]、[ASSURED]
	// 这些标记会让字段偏移，而标记的数量随内核版本变。
	srcs := valuesOf(fields, "src=")
	sports := valuesOf(fields, "sport=")
	if len(srcs) < 2 || len(sports) < 2 {
		tbl.SkippedMalformed++
		return flow.Connection{}, false
	}

	// **源取原始元组的 src，目的取回复元组的 src，端口取回复元组的 sport。**
	//
	// kube-proxy 对 ClusterIP 做 DNAT：原始元组的 dst 是 ClusterIP、dport 是
	// Service 端口，而真正收包的 Pod 与它监听的 targetPort 只出现在回复元组
	// 的 src/sport 上。对一条没有 NAT 的直连，回复元组的 src/sport 恰好等于
	// 原始元组的 dst/dport —— 因此这条规则两种形态都对，不需要判断有没有
	// NAT。判断错的那一次会把 ClusterIP 写进 ipBlock，而它不是任何 Pod 的
	// 地址，那条规则永远匹配不上，且外观完全正常（design doc §2）。
	//
	// SNAT 改的是回复元组的 dst，因此源仍取原始侧。
	src, dst := srcs[0], srcs[1]
	port, err := strconv.ParseInt(sports[1], 10, 32)
	if err != nil || port <= 0 || port > 65535 {
		tbl.SkippedMalformed++
		return flow.Connection{}, false
	}

	if !plausibleAddress(src) || !plausibleAddress(dst) {
		tbl.SkippedMalformed++
		return flow.Connection{}, false
	}
	if src == dst || isLoopback(src) || isLoopback(dst) {
		// 回环 NetworkPolicy 管不到；源等于目的不是一条服务间访问。
		tbl.SkippedLocal++
		return flow.Connection{}, false
	}

	// **TCP 的 [UNREPLIED] 跳过，UDP 的保留。**
	//
	// TCP unreplied 意味着握手没完成，这条连接从没承载过数据 —— 学它等于
	// 推荐放行一条现在就不通的流量，而那可能正是既有策略在拦的东西。
	// UDP 没有握手，单向 UDP（syslog、statsd、metrics push）天然 unreplied，
	// 跳过它会让平台推荐一份切断这些流量的策略。
	//
	// 两条方向相反，因此不能合成一条统一规则（design doc §3）。
	if proto == flow.ProtocolTCP && strings.Contains(line, "[UNREPLIED]") {
		tbl.SkippedUnreplied++
		return flow.Connection{}, false
	}

	return flow.Connection{
		Source: flow.Endpoint{IP: src}, Dest: flow.Endpoint{IP: dst},
		Protocol: proto, Port: int32(port),
	}, true
}

// protocolOf 从第三列取协议，只认 NetworkPolicy 的 ports 能表达的那三种。
func protocolOf(fields []string) (flow.Protocol, bool) {
	if len(fields) < 3 {
		return "", false
	}
	switch fields[2] {
	case "tcp":
		return flow.ProtocolTCP, true
	case "udp":
		return flow.ProtocolUDP, true
	case "sctp":
		return flow.ProtocolSCTP, true
	default:
		return "", false
	}
}

// isProtocolName 报告这个词看起来是不是一个 L4 协议名。
//
// 用来把「这一行是 conntrack 行、但协议我们表达不了」与「这一行读不懂」
// 分开。清单不求全 —— 认不出的落进畸形那一栏，而那一栏偏大只会让人去查
// 解析器，不会让人对集群下错结论。
func isProtocolName(s string) bool {
	switch s {
	case "icmp", "icmpv6", "gre", "esp", "ah", "udplite", "dccp", "unknown":
		return true
	default:
		return false
	}
}

// valuesOf 按出现顺序取出所有 prefix 开头字段的值。
//
// 按出现顺序而不是按固定位置：[UNREPLIED]、[ASSURED] 这些标记会让字段偏移，
// 而标记的数量随内核版本变 —— 一个按索引取值的实现会在某个内核上安静地
// 读错一列。
func valuesOf(fields []string, prefix string) []string {
	var out []string
	for _, f := range fields {
		if strings.HasPrefix(f, prefix) {
			out = append(out, strings.TrimPrefix(f, prefix))
		}
	}
	return out
}

// plausibleAddress 报告这是不是一个能解析的 IP。
//
// 解不开的地址不进产物：它会一路走到 ipBlock 或身份解析，而两处都会以
// 「这个地址归属不明」的形态出现，看起来像一次关于集群的观测结论。
func plausibleAddress(s string) bool {
	return net.ParseIP(s) != nil
}

// isLoopback 报告这是不是回环地址。
func isLoopback(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.IsLoopback()
}
