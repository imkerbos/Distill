package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

// realFingerprint 从 fixtureReader 的候选集里取一条真实存在的
// 禁用规则，返回它的 namespace / workload / fingerprint。
//
// 用真实指纹而非常量：handler 会校验指纹确实出现在当前候选集里，
// 写死一个假指纹的测试只会一直走到那条拒绝分支上，永远测不到成功路径。
func realFingerprint(t *testing.T, h http.Handler, cookie *http.Cookie) (string, string, string) {
	t.Helper()
	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-preview")
	if rec.Code != http.StatusOK {
		t.Fatalf("policy-preview status = %d, want 200", rec.Code)
	}
	data := bodyOf(t, rec)["data"].(map[string]any)
	for _, raw := range data["candidates"].([]any) {
		p := raw.(map[string]any)
		for _, rr := range p["rules"].([]any) {
			r := rr.(map[string]any)
			if r["origin"] == "LEARNED" && r["enabled"] == false {
				return p["namespace"].(string), p["workload"].(string), r["fingerprint"].(string)
			}
		}
	}
	t.Fatal("no disabled learned rule in the fixture candidate set")
	return "", "", ""
}

// realBaselineFingerprint 与 realFingerprint 同理，但取一条 BASELINE
// 来源的规则 —— 用来验证「禁用 BASELINE 规则」在写库前就被拒绝，
// 而不是留下一条永远待在「已失效」里、从未生效过的覆盖。
func realBaselineFingerprint(t *testing.T, h http.Handler, cookie *http.Cookie) (string, string, string) {
	t.Helper()
	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-preview")
	if rec.Code != http.StatusOK {
		t.Fatalf("policy-preview status = %d, want 200", rec.Code)
	}
	data := bodyOf(t, rec)["data"].(map[string]any)
	for _, raw := range data["candidates"].([]any) {
		p := raw.(map[string]any)
		for _, rr := range p["rules"].([]any) {
			r := rr.(map[string]any)
			if r["origin"] == "BASELINE" {
				return p["namespace"].(string), p["workload"].(string), r["fingerprint"].(string)
			}
		}
	}
	t.Fatal("no baseline rule in the fixture candidate set")
	return "", "", ""
}

func newRegisteredRegistry() *memRegistry {
	reg := newMemRegistry()
	reg.clusters["prod-asia-1"] = registry.Cluster{
		ID: "prod-asia-1", DisplayName: "Asia", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
	}
	return reg
}

func TestCreateOverrideRequiresSession(t *testing.T) {
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	rec := postJSONNoAuth(t, h, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": "batch", "workload": "worker",
			"fingerprint": strings.Repeat("a", 64), "decision": "ENABLE", "reason": "r"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestCreateOverrideRoundTrips(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	ns, wl, fp := realFingerprint(t, h, cookie)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "对账任务，Q4 迁走"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	stored := reg.overrides["prod-asia-1"]
	if len(stored) != 1 {
		t.Fatalf("stored overrides = %d, want 1", len(stored))
	}
	// 操作者必须来自会话：允许调用方自称身份的审计记录证明不了任何事。
	if stored[0].DecidedBy != "demo" {
		t.Errorf("DecidedBy = %q, want the session user", stored[0].DecidedBy)
	}
}

// 理由为空是业务失败，且必须点名 reason —— 不点名就等于让人猜。
func TestCreateOverrideRejectsEmptyReason(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	ns, wl, fp := realFingerprint(t, h, cookie)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "   "})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad field value is a business failure", rec.Code)
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(20001) {
		t.Fatalf("code = %v, want 20001", body["code"])
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "reason") {
		t.Errorf("msg = %q, want it to name the reason field", msg)
	}
}

// 指纹对不上必须当场拒绝：写进去会得到一条永远待在「已失效」那一节、
// 却从来没生效过的覆盖。
func TestCreateOverrideRejectsUnknownFingerprint(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	ns, wl, _ := realFingerprint(t, h, cookie)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl,
			"fingerprint": strings.Repeat("b", 64), "decision": "ENABLE", "reason": "r"})
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
	if len(reg.overrides["prod-asia-1"]) != 0 {
		t.Error("an override with an unknown fingerprint was stored")
	}
}

// 禁用一条 BASELINE 规则在写库前就必须被拒绝：policygen.Apply 本身
// 只会把它归入「已失效」，静静地永远不生效——EnsureRuleExists 提前
// 用 policygen.ErrBaselineNotDisablable 拦下同一件事，好过让它先落库
// 再无声无息地作废。
func TestCreateOverrideRejectsDisablingBaseline(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	ns, wl, fp := realBaselineFingerprint(t, h, cookie)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "DISABLE", "reason": "试试关掉"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad field value is a business failure", rec.Code)
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(20001) {
		t.Fatalf("code = %v, want 20001 (body %s)", body["code"], rec.Body.String())
	}
	if len(reg.overrides["prod-asia-1"]) != 0 {
		t.Error("a DISABLE override against a BASELINE rule was stored")
	}
}

// 主键是四元组，缺一个就拒绝。按前缀执行的删除会一次撤掉一批人工
// 决定，而调用方以为自己只撤了一条。
func TestDeleteOverrideRequiresAllThreeParams(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	for name, query := range map[string]string{
		"noNamespace":   "?workload=worker&fingerprint=" + strings.Repeat("a", 64),
		"noWorkload":    "?namespace=batch&fingerprint=" + strings.Repeat("a", 64),
		"noFingerprint": "?namespace=batch&workload=worker",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete,
				"/api/v1/clusters/prod-asia-1/rule-overrides"+query, nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := bodyOf(t, rec)["code"]; got != float64(20001) {
				t.Errorf("code = %v, want 20001", got)
			}
		})
	}
}

func TestDeleteOverrideRoundTrips(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	ns, wl, fp := realFingerprint(t, h, cookie)

	if rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "r"}); rec.Code != http.StatusOK {
		t.Fatalf("create status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/clusters/prod-asia-1/rule-overrides?namespace="+ns+
			"&workload="+wl+"&fingerprint="+fp, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("delete code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	if len(reg.overrides["prod-asia-1"]) != 0 {
		t.Error("override still present after delete")
	}
}

// 未注册的集群不接受写入 —— 外键只能证明行存在，而软删除保留行。
func TestOverrideOnUnregisteredClusterIsNotFound(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/zz-unregistered/rule-overrides",
		map[string]any{"namespace": "batch", "workload": "worker",
			"fingerprint": strings.Repeat("a", 64), "decision": "ENABLE", "reason": "r"})
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("POST code = %v, want 20002", got)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/clusters/zz-unregistered/rule-overrides?namespace=batch&workload=worker&fingerprint="+
			strings.Repeat("a", 64), nil)
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if got := bodyOf(t, rec2)["code"]; got != float64(20002) {
		t.Errorf("DELETE code = %v, want 20002", got)
	}
}
