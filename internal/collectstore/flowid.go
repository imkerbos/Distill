package collectstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
)

// 事实层里的连接没有主键：它按 (窗口, 摄入运行, 序号) 存，而那三样都不在
// store.Reader 的下钻接口上 —— Flow 只收到一个 ID。于是这个 ID 必须自己
// 说清"哪个集群、哪段窗口、哪一条"，否则下钻只能在"最近一次窗口"里找，
// 而列表页看的是调用方选定的窗口，两者不是同一批连接。
//
// 编码而不是新建一张映射表：一张表要跟着连接一起过期，过期口径与保留期
// 一旦对不上，链接会指向一条已经不在库里的连接，而它答得出、只是答错。

// flowIDFields 是编码进 ID 的字段数。
const flowIDFields = 5

// flowIDSep 分隔编码字段。用不可见的单元分隔符：集群 ID 没有字符集约束，
// 任何可打印分隔符都可能出现在集群 ID 里，于是解码会在错误的位置切开。
const flowIDSep = "\x1f"

// flowFingerprintLen 是连接指纹的十六进制长度（8 字节）。
//
// 指纹不是为了防伪造 —— ID 本来就由调用方带回来，伪造一个只能看到自己
// 有权看的那个集群那段窗口里的连接。它防的是**错位**：窗口被重新摄入
// 之后序号会变，一个旧链接照着序号取，会取出另一条连接并把它当成用户
// 点的那条。指纹对不上就答"没有这条"，而不是答一条别的。
const flowFingerprintLen = 16

// flowRef 是一个流量 ID 解出来的东西。
type flowRef struct {
	clusterID   string
	window      flow.Window
	index       int
	fingerprint string
}

// flowIDOf 给窗口内第 index 条连接算出下钻用的 ID。
//
// base64url 而不是原样拼接：ID 会进 URL 路径（/api/v1/flows/{id}/decision），
// 而集群 ID 的字符集没有约束，一个带斜杠的集群 ID 会让路由在错误的位置断开。
func flowIDOf(clusterID string, w flow.Window, index int, c flow.Connection) string {
	payload := strings.Join([]string{
		clusterID,
		strconv.FormatInt(w.From.UTC().UnixMicro(), 10),
		strconv.FormatInt(w.To.UTC().UnixMicro(), 10),
		strconv.Itoa(index),
		flowFingerprint(c),
	}, flowIDSep)
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// flowFingerprint 是一条连接除计数之外的全部内容的摘要。
//
// 不含 ObservedCount：同一条连接被再摄入一次时计数会变，而它仍然是用户
// 点开的那一条。含协议与端口：同一对地址上的两个端口是两条不同的连接，
// 而它们的下钻结论可以完全相反。
func flowFingerprint(c flow.Connection) string {
	h := sha256.New()
	// 每段前面写长度，字段之间就不会因为内容里恰好有分隔符而串位 ——
	// "a|b" 与 "a" + "|b" 必须是两个不同的指纹。
	for _, part := range []string{c.Source.IP, c.Dest.IP, string(c.Protocol), strconv.Itoa(int(c.Port))} {
		_, _ = h.Write([]byte(strconv.Itoa(len(part))))
		_, _ = h.Write([]byte(":"))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:flowFingerprintLen]
}

// ClusterOfFlowID 取出一个采集侧流量 ID 自带的集群；ID 不是本包发出的
// 形状时第二个返回值为 false。
//
// 装配层按 ID 分派下钻时需要且只需要这一个事实（design doc 2026-08-18 §3.1）。
// **只回一个字符串，不导出 parseFlowID 或 flowRef**：窗口、序号与指纹是这条
// 下钻链路上的三道自检 —— 窗口决定重新读哪一段流量、指纹用来发现"这段窗口
// 被重新摄入过、序号已经不指向当初那一条"。把它们一并交出去，等于给了包外
// 一个绕开这三道自检自行取数的位置，而它取错时答得出、也不报错。
//
// 也不做成"返回一个 Reader"之类更大的形状：选 Reader 是装配层的决定，
// 本包不该知道有别的 Reader 存在。
func ClusterOfFlowID(id string) (string, bool) {
	ref, ok := parseFlowID(id)
	if !ok {
		return "", false
	}
	return ref.clusterID, true
}

// parseFlowID 解出一个流量 ID 指向的集群、窗口与位置。
//
// 解不开一律 ok=false，由调用方答成"没有这条流量"：一个解不开的 ID 与一条
// 不存在的流量对调用方是同一件事，分开报会让人能靠响应差异探出 ID 的结构。
func parseFlowID(id string) (flowRef, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return flowRef{}, false
	}
	parts := strings.Split(string(raw), flowIDSep)
	if len(parts) != flowIDFields {
		return flowRef{}, false
	}
	from, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return flowRef{}, false
	}
	to, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return flowRef{}, false
	}
	index, err := strconv.Atoi(parts[3])
	if err != nil || index < 0 {
		return flowRef{}, false
	}
	if len(parts[4]) != flowFingerprintLen {
		return flowRef{}, false
	}
	ref := flowRef{
		clusterID: parts[0],
		window: flow.Window{
			From: time.UnixMicro(from).UTC(),
			To:   time.UnixMicro(to).UTC(),
		},
		index:       index,
		fingerprint: parts[4],
	}
	if ref.clusterID == "" || !ref.window.Valid() {
		return flowRef{}, false
	}
	return ref, true
}
