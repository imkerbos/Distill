package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/imkerbos/Distill/internal/store"
)

// flowListEnvelope 是流量列表的响应形状：分页信息与列表本身同级，
// 调用方据此知道自己拿到的是不是全部。
type flowListEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Items    []store.FlowRecord `json:"items"`
		Total    int                `json:"total"`
		Returned int                `json:"returned"`
		Limit    int                `json:"limit"`
	} `json:"data"`
}

func decodeFlowList(t *testing.T, body []byte) flowListEnvelope {
	t.Helper()
	var env flowListEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v (%q)", err, string(body))
	}
	return env
}

func TestFlowsEndpoint(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?limit=5")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	env := decodeFlowList(t, rec.Body.Bytes())
	if len(env.Data.Items) != 5 {
		t.Errorf("got %d flows, want 5", len(env.Data.Items))
	}
	for _, f := range env.Data.Items {
		if f.Verdict == "" || f.Confidence == "" {
			t.Errorf("flow %s missing verdict or confidence", f.ID)
		}
	}
}

// 被截断的列表必须自报总数，否则界面会把"只给你看了 5 条"说成"一共就这些"。
func TestFlowsResponseReportsTruncation(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?limit=5").Body.Bytes())

	if env.Data.Total <= env.Data.Returned {
		t.Errorf("total = %d, returned = %d — truncation is invisible to the frontend",
			env.Data.Total, env.Data.Returned)
	}
	if env.Data.Returned != len(env.Data.Items) {
		t.Errorf("returned = %d but %d items present", env.Data.Returned, len(env.Data.Items))
	}
	if env.Data.Limit != 5 {
		t.Errorf("limit = %d, want 5", env.Data.Limit)
	}
}

// 默认视图（不带 limit）必须带上刻意制造的敞口，否则 demo 一开屏就是全绿。
func TestFlowsDefaultViewIncludesTheGaps(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows").Body.Bytes())

	if env.Data.Returned != env.Data.Total {
		t.Errorf("default view returned %d of %d flows; the dataset fits in one page",
			env.Data.Returned, env.Data.Total)
	}
	var unknown, cross int
	for _, f := range env.Data.Items {
		if f.Verdict == "UNKNOWN" {
			unknown++
		}
		if f.CrossCluster {
			cross++
		}
	}
	if unknown == 0 || cross == 0 {
		t.Errorf("default view has %d UNKNOWN and %d cross-cluster flows; both must be visible",
			unknown, cross)
	}
}

func TestFlowsFilterByVerdict(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?verdict=DENY&limit=200").Body.Bytes())

	if len(env.Data.Items) == 0 {
		t.Fatal("no DENY flows returned")
	}
	for _, f := range env.Data.Items {
		if f.Verdict != "DENY" {
			t.Fatalf("filter leaked a %s flow", f.Verdict)
		}
	}
}

// limit 不是数字时是参数问题，返回 20001。
func TestFlowsRejectsBadLimit(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?limit=many")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad query value is a business-level failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
}

// 拼错的取值必须报 20001，不能返回空列表：一次输入错误不该被界面读成
// "这个集群没有这类流量"。不存在的集群则与拓扑、质量端点一样报 20002。
func TestFlowsRejectsUnknownFilterValues(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())

	for _, tc := range []struct {
		query string
		code  float64
	}{
		{"verdict=ALOW", 20001},
		{"verdict=allow", 20001},
		{"confidence=TRUSTD", 20001},
		{"confidence=trusted", 20001},
		{"cluster=no-such", 20002},
	} {
		rec := authedGet(t, h, cookie, "/api/v1/flows?"+tc.query)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", tc.query, rec.Code)
		}
		body := bodyOf(t, rec)
		if body["code"] != tc.code {
			t.Errorf("%s code = %v, want %v", tc.query, body["code"], tc.code)
		}
		if body["data"] != nil {
			t.Errorf("%s returned data on a failure", tc.query)
		}
	}
}

// 空取值仍然表示"不筛选"，不能被当成非法输入。
func TestFlowsEmptyFilterValuesMeanNoFilter(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?verdict=&confidence=&cluster=")

	env := decodeFlowList(t, rec.Body.Bytes())
	if env.Code != 0 {
		t.Fatalf("code = %d, want 0", env.Code)
	}
	if len(env.Data.Items) == 0 {
		t.Error("empty filter values returned no flows")
	}
}

// NetworkPolicy 管不到的流量必须自己说出来，不能混在普通 ALLOW 里。
func TestFlowsMarkUnmanagedTraffic(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?limit=1000").Body.Bytes())

	for _, f := range env.Data.Items {
		if f.Unmanaged {
			return
		}
	}
	t.Error("no flow is marked unmanaged; traffic to the hostNetwork pod renders as a plain ALLOW")
}

func TestFlowDecisionEndpoint(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())

	list := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?limit=1").Body.Bytes())
	if len(list.Data.Items) == 0 {
		t.Fatal("no flows to inspect")
	}

	rec := authedGet(t, h, cookie, "/api/v1/flows/"+list.Data.Items[0].ID+"/decision")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int            `json:"code"`
		Data store.Decision `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.ID != list.Data.Items[0].ID {
		t.Errorf("decision is for flow %q, want %q", env.Data.ID, list.Data.Items[0].ID)
	}
}

func TestFlowDecisionUnknownID(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows/flow-999999/decision")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}

func TestFlowsRequiresAuth(t *testing.T) {
	h, _, _ := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, &http.Cookie{Name: "distill_session", Value: "bogus"}, "/api/v1/flows") //nolint:gosec // G124: request cookie, not a response header

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
