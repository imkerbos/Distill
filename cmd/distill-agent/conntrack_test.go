package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	applog "github.com/imkerbos/Distill/internal/log"
)

func quietLogger(t *testing.T) *slog.Logger {
	t.Helper()
	l, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return l
}

// recordingFlowSink 记下 agent 交上来的那份摄入报文。
type recordingFlowSink struct {
	pushes []flowIngestPayload
	err    error
}

func (s *recordingFlowSink) SaveFlowIngest(_ context.Context, p flowIngestPayload) error {
	s.pushes = append(s.pushes, p)
	return s.err
}

func (s *recordingFlowSink) only(t *testing.T) flowIngestPayload {
	t.Helper()
	if len(s.pushes) != 1 {
		t.Fatalf("sink holds %d pushes, want exactly 1", len(s.pushes))
	}
	return s.pushes[0]
}

const twoConnections = `ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.2.7 sport=1 dport=8080 src=10.244.2.7 dst=10.244.1.5 sport=8080 dport=1 mark=0 use=1
ipv4     2 udp      17 29 src=10.244.1.5 dst=10.96.0.10 sport=2 dport=53 src=10.244.0.3 dst=10.244.1.5 sport=5353 dport=2 mark=0 use=1
`

func tableFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nf_conntrack")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write table: %v", err)
	}
	return path
}

// 一次成功的采集交上来的连接与解析结果一致。
func TestConntrackRunPushesWhatItRead(t *testing.T) {
	sink := &recordingFlowSink{}
	err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 1, interval: time.Millisecond,
	}, sink, quietLogger(t))
	if err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}

	got := sink.only(t)
	if got.Status != "OK" || got.ErrorReason != "" {
		t.Errorf("status = %q reason = %q, want OK with no reason", got.Status, got.ErrorReason)
	}
	if len(got.Connections) != 2 {
		t.Fatalf("pushed %d connections, want 2: %+v", len(got.Connections), got.Connections)
	}
	// 目的取回复元组：DNAT 过的那条必须是后端 Pod 与 targetPort。
	var found bool
	for _, c := range got.Connections {
		if c.DstIP == "10.244.0.3" && c.Port == 5353 {
			found = true
		}
		if c.DstIP == "10.96.0.10" {
			t.Error("pushed a ClusterIP as the destination; no pod has that address")
		}
	}
	if !found {
		t.Error("the DNATed connection did not resolve to its backend pod")
	}
}

// **完整度证据必须是"说不出"，不是"满的"。**
//
// conntrack 是当前被跟踪连接的快照，不是日志：轮询之间起止的连接从来不出现
// 在任何一次快照里。因此覆盖窗口缺席、采样率缺席 —— 填任何数都是编的，而
// 编出来的那个数会让下游不降级，于是一批没被看见的连接被当成不存在。
func TestConntrackNeverClaimsItCoveredTheWindow(t *testing.T) {
	sink := &recordingFlowSink{}
	if err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 1, interval: time.Millisecond,
	}, sink, quietLogger(t)); err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}
	got := sink.only(t)

	if got.CoveredWindow != nil {
		t.Error("reported a covered window; polling a conntrack table cannot prove it covered " +
			"the whole period, and claiming so lets downstream stop degrading")
	}
	if got.SampleRate != nil {
		t.Error("reported a sample rate; conntrack does not sample, it just may not have looked " +
			"at the moment a connection existed")
	}
	if got.RequestedWindow.From.IsZero() || !got.RequestedWindow.To.After(got.RequestedWindow.From) {
		t.Errorf("requested window = %+v, want the period it actually tried to observe",
			got.RequestedWindow)
	}
}

// 没截断就**不报** dropped：报 0 等于宣称"一条没漏"，而轮询 conntrack
// 永远说不出那句话。
func TestAnUntruncatedRunReportsNoDroppedCount(t *testing.T) {
	sink := &recordingFlowSink{}
	if err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 1, interval: time.Millisecond,
	}, sink, quietLogger(t)); err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}
	if got := sink.only(t); got.Dropped != nil {
		t.Errorf("Dropped = %d, want absent: reporting zero claims nothing was missed",
			*got.Dropped)
	}
}

// 截断了就报出来：dropped=N 是"知道漏了 N 条"，比"不知道漏没漏"更强的证据。
func TestATruncatedRunReportsItsDroppedCount(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString("ipv4     2 tcp      6 100 ESTABLISHED src=10.244.1.5 dst=10.244.9.")
		b.WriteString(string(rune('1' + i)))
		b.WriteString(" sport=1 dport=8080 src=10.244.9.")
		b.WriteString(string(rune('1' + i)))
		b.WriteString(" dst=10.244.1.5 sport=8080 dport=1 mark=0 use=1\n")
	}
	sink := &recordingFlowSink{}
	if err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, b.String()),
		polls: 1, interval: time.Millisecond, maxConnections: 2,
	}, sink, quietLogger(t)); err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}
	got := sink.only(t)
	if len(got.Connections) != 2 {
		t.Errorf("pushed %d connections, want the cap of 2", len(got.Connections))
	}
	if got.Dropped == nil || *got.Dropped == 0 {
		t.Fatal("truncated without saying how much it lost; a read cap would masquerade as a " +
			"conclusion about the cluster")
	}
}

// **读不到 conntrack 要报 FAILED，不静默成功、也不假装零流量。**
//
// 这同时是 UAT 上那个未知数的答案：数据面若绕开 netfilter，运维要看到
// 一条明确的失败，而不是一个空窗口。
func TestAnUnreadableTableIsReportedAsFailed(t *testing.T) {
	sink := &recordingFlowSink{}
	err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: filepath.Join(t.TempDir(), "absent"),
		polls: 1, interval: time.Millisecond,
	}, sink, quietLogger(t))
	if err == nil {
		t.Error("a run that could not read the table returned success")
	}

	got := sink.only(t)
	if got.Status != "FAILED" {
		t.Errorf("status = %q, want FAILED", got.Status)
	}
	if got.ErrorReason != "UNREACHABLE" {
		t.Errorf("errorReason = %q, want UNREACHABLE", got.ErrorReason)
	}
	if len(got.Connections) != 0 {
		t.Error("a failed ingest carried connections; the platform refuses that, and rightly so")
	}
}

// 表存在但一条也读不出来 → 成功、空、完整度仍然由平台读作 UNKNOWN。
//
// **这与读不到表是两件事**：前者是"看过了，这一刻没有连接"，后者是
// "没看到"。塌成一个会让 calico 绕开 netfilter 这件事看起来像集群很安静。
func TestAnEmptyTableIsSuccessNotFailure(t *testing.T) {
	sink := &recordingFlowSink{}
	if err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, ""),
		polls: 1, interval: time.Millisecond,
	}, sink, quietLogger(t)); err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}
	got := sink.only(t)
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK: the table was readable and simply held nothing", got.Status)
	}
	if len(got.Connections) != 0 {
		t.Errorf("pushed %d connections from an empty table", len(got.Connections))
	}
	if got.CoveredWindow != nil {
		t.Error("an empty read still must not claim it covered the window")
	}
}

// 多次轮询取并集，ObservedCount 累加 —— 单次快照只看得见那一瞬间还活着的
// 连接，短连接密集的集群里那是很小的一部分。
func TestPollingSeveralTimesAccumulates(t *testing.T) {
	sink := &recordingFlowSink{}
	if err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 3, interval: time.Millisecond,
	}, sink, quietLogger(t)); err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}
	got := sink.only(t)
	if len(got.Connections) != 2 {
		t.Fatalf("pushed %d connections, want 2 after deduping three identical polls",
			len(got.Connections))
	}
	for _, c := range got.Connections {
		if c.ObservedCount != 3 {
			t.Errorf("%s->%s observedCount = %d, want 3", c.SrcIP, c.DstIP, c.ObservedCount)
		}
	}
}

// 推送失败要把错误交出去，不能吞掉：agent 跑在 CronJob / DaemonSet 里，
// 一次吞掉的失败会让这一轮观测悄悄消失。
func TestAFailedPushIsNotSwallowed(t *testing.T) {
	sink := &recordingFlowSink{err: errors.New("the platform could not be reached")}
	err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 1, interval: time.Millisecond,
	}, sink, quietLogger(t))
	if err == nil {
		t.Error("a push failure was swallowed")
	}
}

// 每次运行一个新的 run_id：幂等键重复会让第二个节点的观测被当成重推丢掉。
func TestEachRunMintsItsOwnID(t *testing.T) {
	seen := map[string]bool{}
	for range 3 {
		sink := &recordingFlowSink{}
		if err := conntrackOnce(context.Background(), conntrackOptions{
			clusterID: "uat", tablePath: tableFile(t, twoConnections),
			polls: 1, interval: time.Millisecond,
		}, sink, quietLogger(t)); err != nil {
			t.Fatalf("conntrackOnce() error = %v", err)
		}
		id := sink.only(t).RunID
		if id == "" {
			t.Fatal("pushed a run with no id; a retry would become a second history record")
		}
		if seen[id] {
			t.Fatalf("run id %q was reused; one node's observations would be dropped as a "+
				"duplicate of another's", id)
		}
		seen[id] = true
	}
}

// **来源必须是 NODE_CONNTRACK，不能借用 HUBBLE。**
//
// 来源落进 observed_connection.source_kind 长期留存，而"这条连接是谁看见的"
// 决定了它该按哪一份完整度元数据解释。借名会让一批轮询来的连接在事后被当成
// Hubble 的实时流量读 —— 而后者的漏采成因完全不同。
func TestTheSourceIsNamedForWhatActuallySawIt(t *testing.T) {
	sink := &recordingFlowSink{}
	if err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 1, interval: time.Millisecond,
	}, sink, quietLogger(t)); err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}
	if got := sink.only(t).Source; got != "NODE_CONNTRACK" {
		t.Errorf("source = %q, want NODE_CONNTRACK", got)
	}
}
