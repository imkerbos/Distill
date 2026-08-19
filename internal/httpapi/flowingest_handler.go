package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// FlowIngestReader 读最近一次流量摄入的摘要。
//
// 与 CollectionReader 分成两个接口而不是并成一个：资产采集与流量摄入是
// 两条独立的链路，各自会失败、各自重试，而一个部署可以只有其中一条。
// 合并会让"没有流量来源的部署"必须提供一个假的读取端。
//
// 允许为 nil：nil 表示"本部署没有流量读取端"，**不是**"这个集群没有摄入
// 记录" —— 后者是 ErrNoIngest，两者的处置完全不同。
type FlowIngestReader interface {
	LatestIngest(ctx context.Context, clusterID string) (snapshotstore.IngestSummary, error)
}

// ingestErrorReasons 是允许出现在响应里的摄入失败原因，封闭枚举。
//
// 白名单而不是原样透传，理由同 collectionFailureReasons：
// flow_ingest_run.error_reason 在库里只是一列 VARCHAR，封闭性只由写入侧的
// Go 常量保证。一次把底层错误文本写进那一列的笔误，会让 relay 地址、
// 主机名与传输细节从这个响应漏出去（安全规范 §19、§22）。
var ingestErrorReasons = map[string]bool{
	string(snapshotstore.IngestErrorUnreachable):    true,
	string(snapshotstore.IngestErrorUnauthorized):   true,
	string(snapshotstore.IngestErrorQuotaExhausted): true,
	string(snapshotstore.IngestErrorTimeout):        true,
	string(snapshotstore.IngestErrorOther):          true,
}

// ingestSources 是允许回显的来源，封闭枚举。理由同上：source_kind 也是
// 一列 VARCHAR，而这个取值来自别人集群里的进程。
var ingestSources = map[string]bool{"HUBBLE": true, "NODE_CONNTRACK": true}

// handleFlowIngest 交出最近一次流量摄入的摘要。
//
// **「从未摄入过」是一个业务码，不是一个空摘要。** 它与"摄入过、这段窗口
// 确实没有连接"在界面上长得一模一样，而处置完全相反：前者要去部署采集器，
// 后者什么都不用做（design doc 2026-08-19-flow-ingest-visibility §3）。
func handleFlowIngest(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		if d.FlowIngest == nil {
			// 没有读取端的部署答"这条读路径没接通"，**不是**"这个集群没有
			// 摄入记录"。答成后者会让操作者去查集群，而问题在平台的装配。
			response.WriteBusiness(w, response.CodeReadNotWired)
			return
		}
		got, err := d.FlowIngest.LatestIngest(r.Context(), clusterID)
		if errors.Is(err, snapshotstore.ErrNoIngest) {
			response.WriteBusiness(w, response.CodeNoIngestRun)
			return
		}
		if err != nil {
			d.Logger.Error("cannot read the latest flow ingest",
				"err", err, "cluster", clusterID, "request_id", RequestIDFrom(r.Context()))
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}

		if got.ErrorReason != "" && !ingestErrorReasons[got.ErrorReason] {
			d.Logger.Warn("an ingest run carries an unrecognised error reason",
				"cluster", clusterID, "runId", got.RunID, "reason", got.ErrorReason,
				"request_id", RequestIDFrom(r.Context()))
			got.ErrorReason = reasonUnrecognized
		}
		if got.Source != "" && !ingestSources[got.Source] {
			d.Logger.Warn("an ingest run carries an unrecognised source",
				"cluster", clusterID, "runId", got.RunID, "source", got.Source,
				"request_id", RequestIDFrom(r.Context()))
			got.Source = reasonUnrecognized
		}
		response.WriteOK(w, got)
	}
}
