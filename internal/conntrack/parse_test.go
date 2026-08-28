package conntrack_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/conntrack"
	"github.com/imkerbos/Distill/internal/flow"
)

// parse 解一段 conntrack 文本，要求不报错。
func parse(t *testing.T, text string) conntrack.Table {
	t.Helper()
	tbl, err := conntrack.Parse(strings.NewReader(text), conntrack.Limit{Max: 1000})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return tbl
}

// only 取出恰好一条连接。
func only(t *testing.T, tbl conntrack.Table) flow.Connection {
	t.Helper()
	if len(tbl.Connections) != 1 {
		t.Fatalf("parsed %d connections, want exactly 1: %+v", len(tbl.Connections), tbl.Connections)
	}
	return tbl.Connections[0]
}

// 一条没有 NAT 的直连：源、目的、端口都取得对。
func TestADirectConnectionIsReadAsItself(t *testing.T) {
	c := only(t, parse(t, `ipv4     2 tcp      6 431982 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=45678 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=45678 [ASSURED] mark=0 use=2
`))
	if c.Source.IP != "10.244.1.5" {
		t.Errorf("source = %q, want 10.244.1.5", c.Source.IP)
	}
	if c.Dest.IP != "10.244.2.7" {
		t.Errorf("destination = %q, want 10.244.2.7", c.Dest.IP)
	}
	if c.Port != 8080 {
		t.Errorf("port = %d, want 8080", c.Port)
	}
	if c.Protocol != flow.ProtocolTCP {
		t.Errorf("protocol = %q, want TCP", c.Protocol)
	}
}

// **目的取回复元组，不取原始元组。**
//
// kube-proxy 对 ClusterIP 做 DNAT：原始元组的 dst 是 ClusterIP（10.96.0.10）、
// dport 是 Service 端口（53），而真正收包的 Pod（10.244.0.3）与它监听的
// targetPort（5353）只出现在回复元组上。
//
// **一个只看原始元组的实现会在这里红** —— 它会把 ClusterIP 写进规则，而
// ClusterIP 不是任何 Pod 的地址，那条规则永远匹配不上，且外观完全正常。
func TestADNATedConnectionResolvesToTheBackendPodNotTheClusterIP(t *testing.T) {
	c := only(t, parse(t, `ipv4     2 udp      17 29 src=10.244.1.5 dst=10.96.0.10 sport=51234 dport=53 src=10.244.0.3 dst=10.244.1.5 sport=5353 dport=51234 mark=0 use=2
`))
	if c.Dest.IP == "10.96.0.10" {
		t.Error("destination is the ClusterIP; no pod has that address, so the rule would never match")
	}
	if c.Dest.IP != "10.244.0.3" {
		t.Errorf("destination = %q, want the backend pod 10.244.0.3", c.Dest.IP)
	}
	if c.Port == 53 {
		t.Error("port is the Service port; the backend listens on its targetPort")
	}
	if c.Port != 5353 {
		t.Errorf("port = %d, want the targetPort 5353", c.Port)
	}
	if c.Source.IP != "10.244.1.5" {
		t.Errorf("source = %q, want 10.244.1.5", c.Source.IP)
	}
}

// SNAT 改的是回复元组的 dst；原始元组的 src 始终是真正的发起方。
func TestASNATedConnectionKeepsTheRealInitiator(t *testing.T) {
	c := only(t, parse(t, `ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=172.20.0.9 sport=45678 dport=443 src=172.20.0.9 dst=192.168.1.1 sport=443 dport=33333 [ASSURED] mark=0 use=1
`))
	if c.Source.IP != "10.244.1.5" {
		t.Errorf("source = %q, want the real initiator 10.244.1.5, not the SNAT address", c.Source.IP)
	}
	if c.Dest.IP != "172.20.0.9" {
		t.Errorf("destination = %q, want 172.20.0.9", c.Dest.IP)
	}
}

// TCP 的 [UNREPLIED] 跳过：握手没完成，这条连接从没承载过数据。
// 学它等于推荐放行一条现在就不通的流量 —— 而那可能正是既有策略在拦的东西。
func TestAnUnrepliedTCPConnectionIsNotLearnedFrom(t *testing.T) {
	tbl := parse(t, `ipv4     2 tcp      6 118 SYN_SENT src=10.244.1.5 dst=10.244.2.7 sport=44444 dport=9999 [UNREPLIED] src=10.244.2.7 dst=10.244.1.5 sport=9999 dport=44444 mark=0 use=1
`)
	if len(tbl.Connections) != 0 {
		t.Errorf("kept %d connections from an unreplied TCP handshake: %+v",
			len(tbl.Connections), tbl.Connections)
	}
}

// **UDP 的 [UNREPLIED] 必须保留。** UDP 没有握手，单向 UDP（syslog、statsd、
// metrics push）天然 unreplied —— 跳过它会让平台推荐一份切断这些流量的策略。
//
// 这条与上一条方向相反，因此**把两者合成一条统一规则的实现会在这里红**。
func TestAnUnrepliedUDPConnectionIsKept(t *testing.T) {
	c := only(t, parse(t, `ipv4     2 udp      17 29 src=10.244.1.5 dst=10.244.3.9 sport=51234 dport=514 [UNREPLIED] src=10.244.3.9 dst=10.244.1.5 sport=514 dport=51234 mark=0 use=2
`))
	if c.Protocol != flow.ProtocolUDP || c.Port != 514 {
		t.Errorf("connection = %s/%d, want UDP/514", c.Protocol, c.Port)
	}
}

// 三类跳过：回环、源等于目的、NetworkPolicy 表达不了的协议。
func TestTheThingsNetworkPolicyCannotExpressAreSkipped(t *testing.T) {
	tbl := parse(t, `ipv4     2 tcp      6 100 ESTABLISHED src=127.0.0.1 dst=127.0.0.1 sport=1 dport=2 src=127.0.0.1 dst=127.0.0.1 sport=2 dport=1 mark=0 use=1
ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.1.5 sport=3 dport=4 src=10.244.1.5 dst=10.244.1.5 sport=4 dport=3 mark=0 use=1
ipv4     2 icmp     1 29 src=10.244.1.5 dst=10.244.2.7 type=8 code=0 id=1 src=10.244.2.7 dst=10.244.1.5 type=0 code=0 id=1 mark=0 use=1
`)
	if len(tbl.Connections) != 0 {
		t.Errorf("kept %d connections that NetworkPolicy cannot act on: %+v",
			len(tbl.Connections), tbl.Connections)
	}
	if tbl.SkippedProtocol == 0 {
		t.Error("the ICMP entry was skipped without being counted; a default-deny does block ICMP " +
			"and NetworkPolicy cannot express it, so the number has to be visible somewhere")
	}
}

// 同一个 (src, dst, proto, port) 出现多次 → 一条，ObservedCount 累加。
//
// 不去重会让一个连接密集的节点吐出成千上万条一模一样的规则（同
// ScrapeTargetSnapshots 那次 506 条只有 20 个指纹）。临时源端口不进键，
// 那是主要的收敛。
func TestTheSameServicePairCollapsesWithACount(t *testing.T) {
	tbl := parse(t, `ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=1001 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=1001 mark=0 use=1
ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=1002 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=1002 mark=0 use=1
ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=1003 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=1003 mark=0 use=1
`)
	c := only(t, tbl)
	if c.ObservedCount != 3 {
		t.Errorf("ObservedCount = %d, want 3: three ephemeral ports, one service pair", c.ObservedCount)
	}
}

// 畸形行跳过该行，不让整次解析失败：一行读不懂不该让整个节点的观测消失。
func TestAMalformedLineDoesNotLoseTheWholeNode(t *testing.T) {
	tbl := parse(t, `this is not a conntrack line at all
ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=1 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=1 mark=0 use=1
ipv4     2 tcp      6 100 ESTABLISHED src=nonsense dst=also-nonsense sport=x dport=y
`)
	if len(tbl.Connections) != 1 {
		t.Fatalf("parsed %d connections, want the one good line to survive", len(tbl.Connections))
	}
	if tbl.SkippedMalformed < 2 {
		t.Errorf("SkippedMalformed = %d, want at least 2: a silent skip makes a parser bug look "+
			"like a quiet cluster", tbl.SkippedMalformed)
	}
}

// IPv6 解得出来。
func TestIPv6EntriesAreParsed(t *testing.T) {
	c := only(t, parse(t, `ipv6     10 tcp      6 100 ESTABLISHED src=fd00::1 dst=fd00::2 sport=1 dport=8080 src=fd00::2 dst=fd00::1 sport=8080 dport=1 mark=0 use=1
`))
	if c.Source.IP != "fd00::1" || c.Dest.IP != "fd00::2" {
		t.Errorf("addresses = %s -> %s, want fd00::1 -> fd00::2", c.Source.IP, c.Dest.IP)
	}
}

// 超过上限：截断，且把丢掉的条数报出来。
//
// dropped 是"知道漏了 N 条"，比 UNKNOWN 那种"不知道漏没漏"更强的证据 ——
// 静默截断会让一个读取上限伪装成关于集群的观测结论。
func TestExceedingTheLimitTruncatesAndSaysHowMuchItLost(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2." +
			itoa(i) + " sport=1 dport=8080 src=10.244.2." + itoa(i) +
			" dst=10.244.1.5 sport=8080 dport=1 mark=0 use=1\n")
	}
	tbl, err := conntrack.Parse(strings.NewReader(b.String()), conntrack.Limit{Max: 4})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(tbl.Connections) != 4 {
		t.Errorf("kept %d connections, want the cap of 4", len(tbl.Connections))
	}
	if tbl.Dropped != 6 {
		t.Errorf("Dropped = %d, want 6: ten distinct pairs, four carried", tbl.Dropped)
	}
}

// 未超限时**不报** dropped：报 0 等于宣称"一条没漏"，而轮询 conntrack
// 永远说不出那句话。
func TestNotTruncatingReportsNoDroppedCount(t *testing.T) {
	tbl := parse(t, `ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=1 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=1 mark=0 use=1
`)
	if tbl.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0 when nothing was truncated", tbl.Dropped)
	}
	if tbl.Truncated {
		t.Error("Truncated is set although the table fit; the caller uses it to decide whether to " +
			"report a dropped count at all, and reporting 0 would claim nothing was missed")
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

// TCP 条目要带上 ALLOWED：解析层已经丢掉了 [UNREPLIED]，留下的每一条握手
// 都完成过，而握手完成意味着执行平面放行过它。
//
// 这是 conntrack 唯一能给出的判定，也是 dry-run 唯一的证伪手段——没有它，
// 对账里每一条都落进 SOURCE_SILENT，平台永远答不出"我判得对不对"。
// UAT 实测：接上之前 17331 条观测，一致率分子分母全为 0。
func TestTCPEntriesCarryTheAllowedVerdict(t *testing.T) {
	const table = `ipv4 2 tcp 6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=1 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=1 mark=0 use=1
`
	tbl, err := conntrack.Parse(strings.NewReader(table), conntrack.Limit{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(tbl.Connections) != 1 {
		t.Fatalf("got %d connections, want 1", len(tbl.Connections))
	}
	v, reported := tbl.Connections[0].Verdict()
	if !reported {
		t.Fatal("TCP 条目没带判定 —— 对账会把它算成来源没报，一致率永远是空的")
	}
	if v != flow.VerdictAllowed {
		t.Errorf("verdict = %q, want %q", v, flow.VerdictAllowed)
	}
}

// **UDP 不报。** 没有握手，单向条目（syslog、statsd、metrics push）天然
// unreplied 且被刻意保留——一条 UDP 条目证明不了对端收到过。报 ALLOWED
// 等于拿"我发过"冒充"它通了"，而那个假证据会污染一致率。
func TestUDPEntriesReportNoVerdict(t *testing.T) {
	const table = `ipv4 2 udp 17 29 src=10.244.1.5 dst=10.96.0.10 sport=2 dport=53 src=10.244.0.3 dst=10.244.1.5 sport=5353 dport=2 mark=0 use=1
`
	tbl, err := conntrack.Parse(strings.NewReader(table), conntrack.Limit{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(tbl.Connections) != 1 {
		t.Fatalf("got %d connections, want 1", len(tbl.Connections))
	}
	if _, reported := tbl.Connections[0].Verdict(); reported {
		t.Error("UDP 条目报了判定 —— 单向 UDP 证明不了对端收到过")
	}
}
