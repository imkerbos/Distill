package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/httpapi"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

const flowIngestURL = "/api/v1/clusters/prod-asia-1/flow-ingest"

type stubIngestReader struct {
	summary snapshotstore.IngestSummary
	err     error
}

func (s stubIngestReader) ObservedCoverage(
	_ context.Context, _ string,
) (time.Duration, time.Duration, bool, error) {
	// 替身里放一段"看了很久"：这一组用例测的不是学习期门禁，而
	// 学习期门禁排在写回的最前面，答不出就整条路走不下去。
	//
	// 跨度与覆盖给成同一个值：这里没有断采集要表达，两者相等才是
	// 一条连续观测的样子。
	const year = 365 * 24 * time.Hour
	return year, year, true, nil
}

func (s stubIngestReader) LatestIngest(context.Context, string) (snapshotstore.IngestSummary, error) {
	return s.summary, s.err
}

func ingestRouter(t *testing.T, r httpapi.FlowIngestReader) (http.Handler, *http.Cookie) {
	t.Helper()
	h, _, cookie := newTestRouterWithFlowIngest(t, r)
	return h, cookie
}

// **「从未摄入过」是一个自己的答复**，不是一个空摘要。
//
// 它与"摄入过、这段窗口确实没有连接"在界面上长得一模一样，而处置完全
// 相反：前者要去部署采集器，后者什么都不用做。
func TestNeverIngestedGetsItsOwnBusinessCode(t *testing.T) {
	h, cookie := ingestRouter(t, stubIngestReader{err: snapshotstore.ErrNoIngest})
	rec := authedGet(t, h, cookie, flowIngestURL)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNoIngestRun) {
		t.Fatalf("code = %v, want CodeNoIngestRun: %s", got, rec.Body.String())
	}
}

// 没有装配读取端 → 说"这条读路径没接通"，不说"这个集群没有摄入记录"。
// 答成后者会让操作者去查集群，而问题在平台的装配。
func TestNoReaderIsNotTheSameAsNoIngest(t *testing.T) {
	h, cookie := ingestRouter(t, nil)
	rec := authedGet(t, h, cookie, flowIngestURL)
	got := bodyOf(t, rec)["code"]
	if got == float64(response.CodeNoIngestRun) {
		t.Fatal("a missing reader was reported as the cluster never having ingested")
	}
	if got != float64(response.CodeReadNotWired) {
		t.Errorf("code = %v, want CodeReadNotWired", got)
	}
}

// **失败原因走白名单。** error_reason 在库里只是一列 VARCHAR，一次把底层
// 错误文本写进那一列的笔误，会让 relay 地址与传输细节从这个响应漏出去。
func TestAnUnrecognisedErrorReasonIsNotEchoed(t *testing.T) {
	h, cookie := ingestRouter(t, stubIngestReader{summary: snapshotstore.IngestSummary{
		RunID: "r-1", Status: "FAILED", Source: "HUBBLE",
		ErrorReason: "dial tcp 10.0.0.5:4245: connect: connection refused",
	}})
	rec := authedGet(t, h, cookie, flowIngestURL)
	body := rec.Body.String()
	if contains(body, "10.0.0.5") || contains(body, "connect:") {
		t.Fatalf("the response carries the raw transport error: %s", body)
	}
}

// 对照组：登记过的原因照常透传 —— 上一条不是靠"什么都不回"做到的。
func TestARegisteredErrorReasonIsEchoed(t *testing.T) {
	h, cookie := ingestRouter(t, stubIngestReader{summary: snapshotstore.IngestSummary{
		RunID: "r-1", Status: "FAILED", Source: "HUBBLE",
		ErrorReason: string(snapshotstore.IngestErrorUnreachable),
	}})
	rec := authedGet(t, h, cookie, flowIngestURL)
	if !contains(rec.Body.String(), "UNREACHABLE") {
		t.Errorf("a registered reason was not echoed: %s", rec.Body.String())
	}
}

// 来源同样走白名单：它也是一列 VARCHAR，取值来自别人集群里的进程。
func TestAnUnrecognisedSourceIsNotEchoed(t *testing.T) {
	h, cookie := ingestRouter(t, stubIngestReader{summary: snapshotstore.IngestSummary{
		RunID: "r-1", Status: "OK", Source: "relay at 10.0.0.5:4245",
	}})
	if contains(authedGet(t, h, cookie, flowIngestURL).Body.String(), "10.0.0.5") {
		t.Error("the response carries an unregistered source verbatim")
	}
}

// 读取失败不外传底层错误。
func TestAReadFailureIsNotLeaked(t *testing.T) {
	h, cookie := ingestRouter(t, stubIngestReader{
		err: errors.New("mysql: dial tcp 10.9.9.9:3306: refused")})
	rec := authedGet(t, h, cookie, flowIngestURL)
	if contains(rec.Body.String(), "10.9.9.9") || contains(rec.Body.String(), "mysql") {
		t.Fatalf("the response carries the database error: %s", rec.Body.String())
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
