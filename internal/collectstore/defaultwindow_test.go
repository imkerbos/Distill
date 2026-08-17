package collectstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// saveIngestWindow 落一次覆盖指定窗口的摄入运行。
//
// 与 saveIngest 分开：那一个钉死在基准场景的窗口上，而这里要的正是"另一段
// 时间"。连接留空 —— 本组用例问的是窗口取自哪一行，不是那一行里有什么。
func saveIngestWindow(
	t *testing.T, s *snapshotstore.Store, clusterID, runID string,
	w flow.Window, status snapshotstore.IngestStatus,
) {
	t.Helper()
	covered := w
	reason := snapshotstore.IngestErrorNone
	if status == snapshotstore.IngestFailed {
		// 失败的运行什么都没盖住，而失败必须带原因（validateIngestRun）。
		covered = flow.Window{}
		reason = snapshotstore.IngestErrorUnreachable
	}
	res, err := flow.NewIngestResult(flow.SourceHubble, w, covered, nil)
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	if err := s.SaveIngest(context.Background(), snapshotstore.IngestRun{
		ClusterID:   clusterID,
		RunID:       runID,
		StartedAt:   w.To,
		FinishedAt:  w.To.Add(time.Second),
		Status:      status,
		ErrorReason: reason,
		Result:      res,
	}); err != nil {
		t.Fatalf("SaveIngest(%s) error = %v", runID, err)
	}
}

// 默认时间窗是这个集群**最近一次摄入的窗口**，从库里的证据推出来。
//
// 这一条是 design doc 2026-08-18 §3.1 在采集侧的落点。此前它是装配时算好的
// 一个常量 —— 合成数据集自己那段约 36 秒 —— 于是一个 COLLECTED 集群的每一屏
// 都被钉在去年那半分钟上：一次小时级摄入完整包住它，完整度判 COMPLETE，
// 降级限定语不亮，而那 36 秒里没被观测到的连接在推荐里就是"没有流量、
// 可以收紧"。
//
// 四件事一起断言，每一件缺了都会让另外几件失去意义：
//
//  1. 一次摄入都没有 → ErrNoCollection，**不是任何兜底窗口**；
//  2. 有摄入 → 就是那一次的窗口，逐字相等；
//  3. 又来一次更晚的 → 跟着走到最近那一次，说明它不是"第一次"或某个常量；
//  4. 最近的那次是 FAILED → 不取它。那次运行根本没拿到数据，就着它的窗口
//     去查必然零条，而零条会被读成"这段时间没有流量"。
func TestDefaultWindowIsTheClusterLatestIngestWindow(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()

	if _, err := r.DefaultWindow(ctx, collectedID); !errors.Is(err, collectstore.ErrNoCollection) {
		t.Fatalf("DefaultWindow(no ingest at all) error = %v, want ErrNoCollection — a cluster "+
			"nobody has ingested for has no window we can answer about, and any fallback would "+
			"be a stretch of time with no evidence behind it", err)
	}

	first := flow.Window{From: windowStart, To: windowEnd}
	saveIngestWindow(t, s, collectedID, "dw-ingest-1", first, snapshotstore.IngestOK)
	assertDefaultWindow(t, r, first, "after a single ingest run")

	// 更晚的一次：默认窗口必须跟过去。恒返回第一次、或返回某个常量的实现
	// 到这里才会红。
	second := flow.Window{From: windowEnd.Add(time.Hour), To: windowEnd.Add(2 * time.Hour)}
	saveIngestWindow(t, s, collectedID, "dw-ingest-2", second, snapshotstore.IngestOK)
	assertDefaultWindow(t, r, second, "after a later ingest run")

	// 最近的一次失败了：窗口留在上一次成功的那段，不跟到它上面。
	failedWindow := flow.Window{From: second.To.Add(time.Hour), To: second.To.Add(2 * time.Hour)}
	saveIngestWindow(t, s, collectedID, "dw-ingest-3", failedWindow, snapshotstore.IngestFailed)
	assertDefaultWindow(t, r, second, "after a later ingest run that failed")

	// 对照组：同一个 Reader、同一个库，另一个从没摄入过的集群仍然答不出。
	// 少了这一条，一个"总是答上一次成功那段"的实现也能让上面全绿。
	if _, err := r.DefaultWindow(ctx, silentID); !errors.Is(err, collectstore.ErrNoCollection) {
		t.Errorf("DefaultWindow(%s, never ingested) error = %v, want ErrNoCollection — the "+
			"window is per cluster, not per database", silentID, err)
	}
}

// assertDefaultWindow 断言默认窗口逐字等于期望的那一段。
func assertDefaultWindow(t *testing.T, r *collectstore.Reader, want flow.Window, stage string) {
	t.Helper()
	got, err := r.DefaultWindow(context.Background(), collectedID)
	if err != nil {
		t.Fatalf("DefaultWindow(%s) error = %v", stage, err)
	}
	// 逐字段比时刻而不是比结构体：DATETIME(6) 读回来的 time.Time 带的是
	// 另一个 Location，== 会在两个描述同一时刻的值上判不相等。
	if !got.From.Equal(want.From) || !got.To.Equal(want.To) {
		t.Errorf("DefaultWindow(%s) = %v–%v, want %v–%v",
			stage, got.From, got.To, want.From, want.To)
	}
	if want := (store.TimeWindow{From: got.From, To: got.To}); !want.Valid() {
		t.Errorf("DefaultWindow(%s) handed back an interval that is not usable for a query: %v–%v",
			stage, got.From, got.To)
	}
}
