package snapshotstore_test

import (
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// 一次都没摄入过 → 说"从未"，不说"没有流量"。
//
// 两者在今天的界面上长得一模一样，而处置完全相反：一个要去装采集器，
// 一个什么都不用做。
func TestNeverIngestedIsItsOwnAnswer(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.LatestIngest(t.Context(), clusterA)
	if !errors.Is(err, snapshotstore.ErrNoIngest) {
		t.Fatalf("LatestIngest() error = %v, want ErrNoIngest", err)
	}
}

// 摄入过、这段窗口确实没有连接 —— 这是一句关于集群的话，不是"从未"。
func TestAnIngestWithNoConnectionsIsNotTheSameAsNeverIngesting(t *testing.T) {
	s, _ := newTestStore(t)
	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-empty", snapshotstore.IngestOK,
		completeIngest(t)))

	got, err := s.LatestIngest(t.Context(), clusterA)
	if err != nil {
		t.Fatalf("LatestIngest() error = %v", err)
	}
	if got.Status != string(snapshotstore.IngestOK) {
		t.Errorf("Status = %q, want OK", got.Status)
	}
	if got.Connections != 0 {
		t.Errorf("Connections = %d, want 0", got.Connections)
	}
	if got.RunID != "ingest-empty" {
		t.Errorf("RunID = %q", got.RunID)
	}
}

// 最近一次要真的是最近一次。
func TestLatestIngestIsTheMostRecentRun(t *testing.T) {
	s, _ := newTestStore(t)
	mustSaveIngest(t, s, ingestRun(clusterA, "old", snapshotstore.IngestOK,
		completeIngest(t, connection("10.4.0.9", "10.4.0.21", 8080))))
	later := ingestRun(clusterA, "new", snapshotstore.IngestOK,
		completeIngest(t, connection("10.4.0.9", "10.4.0.22", 9090)))
	later.StartedAt = later.StartedAt.Add(time.Hour)
	later.FinishedAt = later.FinishedAt.Add(time.Hour)
	mustSaveIngest(t, s, later)

	got, err := s.LatestIngest(t.Context(), clusterA)
	if err != nil {
		t.Fatalf("LatestIngest() error = %v", err)
	}
	if got.RunID != "new" {
		t.Errorf("RunID = %q, want the later run", got.RunID)
	}
}

// 失败的那一次要带着封闭枚举的原因交出来 —— 没有原因的失败，与一次
// "摄入成功、这段时间确实没有流量"在界面上长得一模一样。
func TestAFailedIngestCarriesItsReason(t *testing.T) {
	s, _ := newTestStore(t)
	run := ingestRun(clusterA, "ingest-bad", snapshotstore.IngestFailed, completeIngest(t))
	run.ErrorReason = snapshotstore.IngestErrorUnreachable
	mustSaveIngest(t, s, run)

	got, err := s.LatestIngest(t.Context(), clusterA)
	if err != nil {
		t.Fatalf("LatestIngest() error = %v", err)
	}
	if got.Status != string(snapshotstore.IngestFailed) {
		t.Errorf("Status = %q, want FAILED", got.Status)
	}
	if got.ErrorReason != string(snapshotstore.IngestErrorUnreachable) {
		t.Errorf("ErrorReason = %q, want UNREACHABLE", got.ErrorReason)
	}
}

// **完整度由证据算出，不落库、不由这一层编。**
//
// 同时要说得出缺了哪几项证据：不说的话，操作者会以为 UNKNOWN 是平台的
// 毛病，而它其实是来源的性质。
func TestLatestIngestSaysWhyItIsNotComplete(t *testing.T) {
	s, _ := newTestStore(t)
	// 覆盖窗口给满、丢弃数给 0，只差采样率。
	res, err := flow.NewIngestResult(flow.SourceHubble, window, window, nil)
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-partial", snapshotstore.IngestOK,
		res.WithDropped(0)))

	got, err := s.LatestIngest(t.Context(), clusterA)
	if err != nil {
		t.Fatalf("LatestIngest() error = %v", err)
	}
	if got.Completeness == string(flow.CompletenessComplete) {
		t.Fatal("completeness is COMPLETE while the source never reported a sample rate")
	}
	if got.SampleRateKnown {
		t.Error("SampleRateKnown is true although no sample rate was ever reported")
	}
	if !got.DroppedReported {
		t.Error("DroppedReported is false although the source reported zero dropped")
	}
}

// 来源照实回显：它是摄入时写下的事实，读出来就是，不猜。
func TestLatestIngestEchoesItsSource(t *testing.T) {
	s, _ := newTestStore(t)
	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-src", snapshotstore.IngestOK,
		completeIngest(t)))
	got, _ := s.LatestIngest(t.Context(), clusterA)
	if got.Source != string(flow.SourceHubble) {
		t.Errorf("Source = %q, want HUBBLE", got.Source)
	}
}
