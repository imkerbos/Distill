package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
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

// fingerprintsUnderWindow 收集在给定窗口下预览到的规则指纹，
// 以 (namespace, workload, fingerprint) 为键。空 from/to 表示不带
// 查询参数，即让 handler 落回它自己的默认窗口。
func fingerprintsUnderWindow(
	t *testing.T, h http.Handler, cookie *http.Cookie, window store.TimeWindow,
) map[[3]string]bool {
	t.Helper()
	path := "/api/v1/clusters/prod-asia-1/policy-preview"
	if !window.From.IsZero() || !window.To.IsZero() {
		path += "?from=" + window.From.UTC().Format(time.RFC3339) +
			"&to=" + window.To.UTC().Format(time.RFC3339)
	}
	rec := authedGet(t, h, cookie, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy-preview status = %d (body %s)", rec.Code, rec.Body.String())
	}
	data := bodyOf(t, rec)["data"].(map[string]any)
	out := map[[3]string]bool{}
	for _, raw := range data["candidates"].([]any) {
		p := raw.(map[string]any)
		ns, wl := p["namespace"].(string), p["workload"].(string)
		for _, rr := range p["rules"].([]any) {
			r := rr.(map[string]any)
			out[[3]string{ns, wl, r["fingerprint"].(string)}] = true
		}
	}
	return out
}

// TestCreateOverrideHonorsPayloadWindow 锁住 correctness point 4：
// EnsureRuleExists 必须用请求体里的 from/to 重算候选集，不能永远用
// d.DefaultWindow——否则调用方在预览页看到的、经由自定义窗口生成的
// 一条规则，提交覆盖时会因为服务端换了一个窗口而被拒绝，错误信息还
// 说"页面可能已过期"，把人指向错误的原因。
//
// fixture 数据集里，任何自定义窗口的候选集在数学上都是全量窗口候选集
// 的子集（BASELINE 规则与窗口无关，LEARNED 规则只会随窗口扩大而增多、
// 不会减少）——用全量窗口当默认值时，永远构造不出"只在自定义窗口里
// 存在、默认窗口里没有"的指纹。所以这里反过来构造：把默认窗口人为收窄
// （模拟真实部署里"最近 N 天"这种默认值本就不是全量的常态），
// 用请求体带上更宽的窗口去核验一条默认窗口看不到的规则——这与
// "自定义窗口比默认窗口更宽或更窄"是同一类问题的两个方向，验证的是
// 同一条修复：请求体里的窗口必须真的被使用，而不是被默默换成默认值。
func TestCreateOverrideHonorsPayloadWindow(t *testing.T) {
	reader := fixtureReader()
	full := reader.(*store.FixtureReader).DataWindow()
	narrow := store.TimeWindow{From: full.From, To: full.From.Add(60 * time.Second)}
	if !narrow.To.Before(full.To) {
		t.Fatal("fixture data window too short for this test's narrow slice")
	}

	reg := newRegisteredRegistry()
	// 默认窗口显式收窄，而不是 newTestRouterWithRegistry 自动推导的
	// 全量窗口——原因见上面的函数注释。
	h, _, cookie := newTestRouterWithWindow(t, reader, reg, narrow)

	wideOnly := fingerprintsUnderWindow(t, h, cookie, full)
	narrowSet := fingerprintsUnderWindow(t, h, cookie, store.TimeWindow{})
	var ns, wl, fp string
	for key := range wideOnly {
		if !narrowSet[key] {
			ns, wl, fp = key[0], key[1], key[2]
			break
		}
	}
	if fp == "" {
		t.Fatal("no fingerprint found that exists under the full window but not the narrow default")
	}

	// 请求体带上完整窗口（与预览页用的同一个），指纹在这个窗口下存在，
	// 必须被接受——即便它在 handler 的默认窗口（更窄）下根本看不到。
	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{
			"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "自定义窗口下确认",
			"from": full.From.UTC().Format(time.RFC3339),
			"to":   full.To.UTC().Format(time.RFC3339),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 — the payload's window should have made this fingerprint visible (body %s)",
			got, rec.Body.String())
	}
	if len(reg.overrides["prod-asia-1"]) != 1 {
		t.Fatalf("stored overrides = %d, want 1", len(reg.overrides["prod-asia-1"]))
	}

	// 同一个指纹，这次不带 from/to：落回默认窗口（更窄），默认窗口下
	// 看不到这条规则，必须拒绝——证明"两者都不填时退回默认窗口"这条
	// 兜底没有被上面的窗口穿透逻辑破坏，也顺带证明上面的成功不是因为
	// 指纹在默认窗口下也凑巧存在。
	rec2 := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{
			"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "不带窗口，应当回落到更窄的默认窗口并被拒绝",
		})
	if got := bodyOf(t, rec2)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001 (body %s)", got, rec2.Body.String())
	}
	if len(reg.overrides["prod-asia-1"]) != 1 {
		t.Error("the second, window-less request must not have overwritten the stored override")
	}
}

// TestCreateOverrideRejectsHalfSpecifiedWindow 锁住"只填一个"的语义：
// 与 parseWindow 在 /flows、/policy-preview 上的既有判据一致，
// 半成品窗口是调用方的输入错误，不能被当成"没填"而回落到默认值。
func TestCreateOverrideRejectsHalfSpecifiedWindow(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	ns, wl, fp := realFingerprint(t, h, cookie)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{
			"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "r",
			"from": "2026-07-31T10:00:00Z",
		})
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
	if len(reg.overrides["prod-asia-1"]) != 0 {
		t.Error("a half-specified window must not have been accepted as a fallback to the default")
	}
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
	// 必须点名 BASELINE：只看 code 分不清"因为它是 BASELINE 被拦下"
	// 和"凑巧指纹没对上"这两种拒绝，两者背后的原因、操作者该怎么改，
	// 完全不同。
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "BASELINE") {
		t.Errorf("msg = %q, want it to name BASELINE", msg)
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

// TestCreateOverrideReaderFailureIsInternalErrorAndLeaksNothing 覆盖
// fleet_handler_test.go 里 TestReaderFailureIsInternalErrorAndLeaksNothing
// 没有覆盖到的一条路径：POST /rule-overrides 走的是
// d.Reader.EnsureRuleExists，不是那条测试枚举的任何一个 GET 端点。
//
// 这条路径专门要测的不是"writeRegistryError 会不会泄露"——那件事已经
// 在别处证明过。要测的是新加的
// errors.Is(err, policygen.ErrBaselineNotDisablable) 分支排在
// writeRegistryError 之前这件事本身是安全的：这个分支只在命中特定哨兵
// 时短路，其余错误（包括 Reader 的内部故障）必须原样落到
// writeRegistryError，而不是被这个新分支意外吞掉或误判成业务失败。
//
// 集群必须是真实注册的：如果连 d.Registry.Cluster 都失败，请求会在
// 走到 EnsureRuleExists 之前就已经返回，这条测试就测不到目标分支。
func TestCreateOverrideReaderFailureIsInternalErrorAndLeaksNothing(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, brokenReader{}, reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": "batch", "workload": "worker",
			"fingerprint": strings.Repeat("a", 64), "decision": "ENABLE", "reason": "r"})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(50001) {
		t.Errorf("code = %v, want 50001", body["code"])
	}
	if body["data"] != nil {
		t.Errorf("data = %v, want null", body["data"])
	}
	for _, secret := range []string{"bigquery", "connection refused", "10.0.0.5", "9050"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("response leaked %q: %s", secret, rec.Body.String())
		}
	}
	for _, values := range rec.Header() {
		for _, v := range values {
			for _, secret := range []string{"bigquery", "connection refused", "10.0.0.5", "9050"} {
				if strings.Contains(v, secret) {
					t.Errorf("response header leaked %q", secret)
				}
			}
		}
	}
}
