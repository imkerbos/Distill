package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const previewPath = "/api/v1/clusters/prod-asia-1/policy-preview"

func TestPolicyPreviewRequiresSession(t *testing.T) {
	h, _, _ := newTestRouter(t, fixtureReader())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, previewPath, nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — the frontend redirects to login on this", rec.Code)
	}
}

// 四块必须同时返回。少任何一块，界面都会把一份残缺的推荐
// 显示成完整方案。
func TestPolicyPreviewReturnsFourBlocks(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, previewPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(0) {
		t.Fatalf("code = %v, want 0", body["code"])
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is not an object: %v", body["data"])
	}
	for _, key := range []string{
		"candidates", "missingBaselines", "ungeneratable", "prediction", "baselineKinds",
	} {
		if _, present := data[key]; !present {
			t.Errorf("data missing key %q", key)
		}
	}
	if got, _ := data["candidates"].([]any); len(got) == 0 {
		t.Error("candidates empty; the fixture has learnable traffic")
	}
}

// 四类计数即使为零也必须出现。省略一个零会让界面把"没有会被打断的连接"
// 显示成"这一项没算"，两者的处置完全不同。
func TestPolicyPreviewCountsIncludeEveryChangeKind(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	data := bodyOf(t, authedGet(t, h, cookie, previewPath))["data"].(map[string]any)
	pred, ok := data["prediction"].(map[string]any)
	if !ok {
		t.Fatalf("prediction is not an object: %v", data["prediction"])
	}
	counts, ok := pred["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts is not an object: %v", pred["counts"])
	}
	for _, k := range []string{"WOULD_BREAK", "WOULD_OPEN", "UNCHANGED", "UNKNOWN"} {
		if _, present := counts[k]; !present {
			t.Errorf("counts missing %q; a zero must be shown, not omitted", k)
		}
	}
}

// 时间窗必须回显：一个按时间筛过的结果若不说明筛的是哪一段，
// 在界面上与全量结果无法区分。
func TestPolicyPreviewEchoesWindow(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	const from, to = "2026-07-31T10:00:00Z", "2026-07-31T10:00:10Z"
	rec := authedGet(t, h, cookie, previewPath+"?from="+from+"&to="+to)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	data := bodyOf(t, rec)["data"].(map[string]any)
	window, ok := data["window"].(map[string]any)
	if !ok {
		t.Fatalf("window is not an object: %v", data["window"])
	}
	if window["from"] != from || window["to"] != to {
		t.Errorf("window = %v, want from=%s to=%s", window, from, to)
	}
}

// 只给半个窗口是参数错误，属于业务失败：不该计入服务错误率。
func TestPolicyPreviewRejectsHalfWindow(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, previewPath+"?from=2026-07-31T10:00:00Z")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad query value is a business-level failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
}

func TestPolicyPreviewUnknownClusterIsBusinessFailure(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/clusters/nope/policy-preview")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a missing resource is not a server fault", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}

// 不存在的 namespace 与不存在的集群同码。
//
// 这里若放行成一份 code=0 的空报告，界面会显示「没有会被这条推荐拦断的
// 连接」「没有 namespace 缺失 baseline」—— 一次拼写错误得到一份体检报告。
func TestPolicyPreviewUnknownNamespaceIsBusinessFailure(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, previewPath+"?namespace=no-such-ns")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a missing resource is not a server fault", rec.Code)
	}
	body := bodyOf(t, rec)
	if got := body["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002 (body = %v)", got, body)
	}
}

// 存在的 namespace 必须照常返回，否则上面那条断言用一个恒错的实现也能通过。
func TestPolicyPreviewKnownNamespaceSucceeds(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, previewPath+"?namespace=payment")

	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Errorf("code = %v, want 0 for an existing namespace", got)
	}
}

// 内部错误只回固定文案，真实原因带 request_id 进日志。
func TestPolicyPreviewHidesInternalError(t *testing.T) {
	h, _, cookie := newTestRouter(t, brokenReader{})
	rec := authedGet(t, h, cookie, previewPath)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	for _, secret := range []string{"bigquery", "10.0.0.5", "9050"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("response leaked %q: %s", secret, rec.Body.String())
		}
	}
}
