package snapshotstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
)

// ErrNoIngest 表示这个集群还没有过任何一次流量摄入。
//
// **与"摄入过、这段窗口确实没有连接"必须分开。** 两者在界面上长得一模一样
// （都是"没有流量"），而处置完全相反：前者要去部署采集器或开流量日志，
// 后者什么都不用做 —— 那是一句关于集群的话。塌成一个，操作者会照着错的
// 那一半行动（design doc 2026-08-19-flow-ingest-visibility §3）。
var ErrNoIngest = errors.New("snapshotstore: this cluster has never had a flow ingest")

// IngestSummary 是最近一次流量摄入的摘要。
type IngestSummary struct {
	ClusterID string `json:"clusterId"`
	RunID     string `json:"runId"`
	// Source 是这批连接的来源，摄入时写下的事实，照实回显、不猜。
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// Status 取值 OK / PARTIAL / FAILED。
	Status string `json:"status"`
	// ErrorReason 仅在这一次失败时非空，取值是封闭枚举。
	ErrorReason string `json:"errorReason,omitempty"`
	// Window 是这次要观测的那段时间。
	Window flow.Window `json:"window"`
	// Covered 是来源说自己实际覆盖到的那段；不合法表示来源说不出。
	Covered flow.Window `json:"covered"`
	// Connections 是这次摄入落库的连接条数。
	Connections int `json:"connections"`

	// 下面三项是**完整度的证据**，逐项报出来。
	//
	// 只报一个 Completeness 不够：操作者会以为 UNKNOWN 是平台的毛病，
	// 而它其实是来源的性质 —— Hubble 与 conntrack 都永远到不了 COMPLETE，
	// 各有各的原因。缺了哪一项要说得出（design doc §4）。
	SampleRate      float64 `json:"sampleRate,omitempty"`
	SampleRateKnown bool    `json:"sampleRateKnown"`
	Dropped         uint64  `json:"dropped,omitempty"`
	DroppedReported bool    `json:"droppedReported"`
	// CoveredKnown 表示来源说得出自己覆盖了哪一段。
	CoveredKnown bool `json:"coveredKnown"`

	// Completeness 由上面那几项算出，**不落库**：完整度不是一个可以被
	// 填写的字段，而是证据的函数（flow.IngestResult 连 setter 都没有）。
	// 存一份算出来的结论只会多一个会与证据对不上的位置。
	Completeness string `json:"completeness"`
}

// LatestIngest 返回最近一次流量摄入的摘要。
//
// 从未摄入过时返回 ErrNoIngest —— 那与"摄入过、没有连接"是两句不同的话。
func (s *Store) LatestIngest(ctx context.Context, clusterID string) (IngestSummary, error) {
	out := IngestSummary{ClusterID: clusterID}
	var (
		coveredStart, coveredEnd sql.NullTime
		sampleRate               sql.NullFloat64
		dropped                  sql.NullInt64
	)
	// 按 finished_at 取最近一次，不按 run_id：run_id 是随机的，排序无意义。
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, source_kind, window_start, window_end,
		        covered_start, covered_end, started_at, finished_at,
		        status, error_reason, sample_rate, dropped
		   FROM flow_ingest_run
		  WHERE cluster_id = ?
		  ORDER BY finished_at DESC, run_id DESC
		  LIMIT 1`, clusterID).Scan(
		&out.RunID, &out.Source, &out.Window.From, &out.Window.To,
		&coveredStart, &coveredEnd, &out.StartedAt, &out.FinishedAt,
		&out.Status, &out.ErrorReason, &sampleRate, &dropped)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestSummary{}, fmt.Errorf("%w: cluster %s", ErrNoIngest, clusterID)
	}
	if err != nil {
		return IngestSummary{}, fmt.Errorf("snapshotstore: read latest ingest: %w", err)
	}

	// NULL 是"来源说不出"，不是零：填一个零值覆盖窗口等于替来源宣称它
	// 一秒都没覆盖到，而它其实是没说过这件事。
	if coveredStart.Valid && coveredEnd.Valid {
		out.Covered = flow.Window{From: coveredStart.Time, To: coveredEnd.Time}
		out.CoveredKnown = out.Covered.Valid()
	}
	if sampleRate.Valid {
		out.SampleRate, out.SampleRateKnown = sampleRate.Float64, true
	}
	if dropped.Valid {
		out.Dropped, out.DroppedReported = uint64(dropped.Int64), true //nolint:gosec // 列非负
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM observed_connection WHERE cluster_id = ? AND ingest_run_id = ?`,
		clusterID, out.RunID).Scan(&out.Connections); err != nil {
		return IngestSummary{}, fmt.Errorf("snapshotstore: count ingested connections: %w", err)
	}

	out.Completeness = completenessOf(out)
	return out, nil
}

// completenessOf 把库里那几项证据交回 flow 包算一次完整度。
//
// **不在这里另写一份判据**：库里存的是证据，结论由同一个函数算出来。两份
// 判据迟早会给出互相矛盾的答案，而那种矛盾不会报错。
//
// 来源或窗口在库里就是坏的时候答 UNKNOWN，**而不是让整次读取失败**：
// 一条说不出自己从哪来的记录仍然值得显示（它证明了"这个集群摄入过"），
// 只是它说不出自己有多完整。这与"从未摄入过"仍然是两句不同的话。
func completenessOf(s IngestSummary) string {
	res, err := flow.NewIngestResult(flow.SourceKind(s.Source), s.Window, s.Covered, nil)
	if err != nil {
		return string(flow.CompletenessUnknown)
	}
	if s.SampleRateKnown {
		res = res.WithSampleRate(s.SampleRate)
	}
	if s.DroppedReported {
		res = res.WithDropped(s.Dropped)
	}
	_, completeness := res.Connections()
	return string(completeness)
}
