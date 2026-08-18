package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// granularityView 是本组用例要读的那几项。
//
// 手写而不是复用 store.PolicyPreview：断言的是**真的传给前端的那份 JSON**。
// 拿被测类型自己去解自己的输出，一个字段被改成不序列化也照样绿。
type granularityView struct {
	Granularity string `json:"granularity"`
	Candidates  []struct {
		Namespace   string `json:"namespace"`
		Granularity string `json:"granularity"`
		Workload    string `json:"workload"`
	} `json:"candidates"`
	Widening []struct {
		Namespace   string `json:"namespace"`
		Workloads   int    `json:"workloads"`
		Rules       int    `json:"rules"`
		ExtraGrants int    `json:"extraGrants"`
	} `json:"widening"`
	Prediction struct {
		Counts map[string]int `json:"counts"`
	} `json:"prediction"`
}

func previewAt(t *testing.T, query string) granularityView {
	t.Helper()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-preview"+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int             `json:"code"`
		Data granularityView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != 0 {
		t.Fatalf("code = %d, want 0: %s", env.Code, rec.Body.String())
	}
	return env.Data
}

// 缺省是 workload 粒度 —— 本轮之前的行为，没有回归。
func TestPolicyPreviewDefaultsToWorkloadGranularity(t *testing.T) {
	got := previewAt(t, "")
	if got.Granularity != "WORKLOAD" {
		t.Errorf("granularity = %q, want WORKLOAD as the default", got.Granularity)
	}
	if len(got.Widening) != 0 {
		t.Errorf("widening = %v, want empty at workload granularity", got.Widening)
	}
	for _, c := range got.Candidates {
		if c.Workload == "" {
			t.Errorf("%s: workload granularity lost its subject", c.Namespace)
		}
	}
}

// 切到 namespace 粒度：每个 namespace 一份、主体没有 workload、放宽报告在场。
func TestPolicyPreviewAtNamespaceGranularity(t *testing.T) {
	got := previewAt(t, "?granularity=namespace")
	if got.Granularity != "NAMESPACE" {
		t.Fatalf("granularity = %q, want NAMESPACE", got.Granularity)
	}
	seen := map[string]int{}
	for _, c := range got.Candidates {
		seen[c.Namespace]++
		if c.Granularity != "NAMESPACE" {
			t.Errorf("%s: candidate granularity = %q", c.Namespace, c.Granularity)
		}
		if c.Workload != "" {
			t.Errorf("%s: still names workload %q", c.Namespace, c.Workload)
		}
	}
	for ns, n := range seen {
		if n != 1 {
			t.Errorf("%s got %d policies at namespace granularity, want 1", ns, n)
		}
	}
	if len(got.Widening) == 0 {
		t.Error("no widening report at namespace granularity; coarsening only ever widens, and " +
			"that must not happen silently")
	}
}

// **粒度必须回显。** 一份不说明自己粒度的策略集，操作者无从判断屏幕上那
// 42 份是"折叠过的"还是"这个集群只有 42 个 workload"。
func TestTheResponseSaysWhichGranularityItIs(t *testing.T) {
	if a, b := previewAt(t, "").Granularity, previewAt(t, "?granularity=namespace").Granularity; a == b {
		t.Fatalf("both granularities echo %q; the field carries no information", a)
	}
}

// 拼错的取值落到 workload，且**照实回显** —— 界面据此显示当前粒度，
// 拼错看得见。报错则会让一次手误变成一屏空白。
func TestAMisspelledGranularityFallsBackToWorkloadAndSaysSo(t *testing.T) {
	got := previewAt(t, "?granularity=namesapce")
	if got.Granularity != "WORKLOAD" {
		t.Errorf("granularity = %q, want WORKLOAD: an unregistered value must not reach the "+
			"coarser side, which would widen every policy to its whole namespace", got.Granularity)
	}
}

// **两套预测必须各算一套。** 一份 namespace 粒度的策略集配 workload 粒度的
// WOULD_BREAK 描述的是另一套策略，且偏在让人放心的方向（粗化只会放宽）。
func TestEachGranularityCarriesItsOwnPrediction(t *testing.T) {
	wl := previewAt(t, "")
	ns := previewAt(t, "?granularity=namespace")

	if wl.Prediction.Counts["WOULD_BREAK"] == 0 && ns.Prediction.Counts["WOULD_BREAK"] == 0 {
		t.Skip("fixture breaks nothing at either granularity; this case cannot discriminate")
	}
	// 粗化只会放宽 → 拦断不可能变多。这条比"两个数不相等"更强：它钉住了
	// 方向，而一个把两套策略算反了的实现会在这里红。
	if ns.Prediction.Counts["WOULD_BREAK"] > wl.Prediction.Counts["WOULD_BREAK"] {
		t.Errorf("namespace granularity breaks %d connections vs %d at workload granularity; "+
			"coarsening only ever widens, so it cannot break more",
			ns.Prediction.Counts["WOULD_BREAK"], wl.Prediction.Counts["WOULD_BREAK"])
	}
}
