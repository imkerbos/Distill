package httpapi_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const importYAML = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-gateway
  namespace: payment
spec:
  podSelector: {}
`

func TestImportRoundTrips(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": importYAML})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0", got)
	}
	if len(reg.imports["prod-asia-1"]) != 1 {
		t.Fatalf("stored imports = %d, want 1", len(reg.imports["prod-asia-1"]))
	}
	got := reg.imports["prod-asia-1"][0]
	if got.Namespace != "payment" || got.Name != "allow-gateway" {
		t.Errorf("parsed metadata = %s/%s, want payment/allow-gateway", got.Namespace, got.Name)
	}
	if got.ImportedBy == "" {
		t.Error("ImportedBy is empty; the actor must come from the session")
	}
}

// 写坏的 ipBlock 必须在导入时被拦住，而不是等到求值层产出
// POLICY_MALFORMED 之后被读成平台的缺陷。
func TestImportRejectsMalformedIPBlock(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: x, namespace: y}
spec:
  podSelector: {}
  ingress:
    - from: [{ipBlock: {cidr: "10.0.0/8"}}]
`})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
	// msg 必须点名是 ipBlock 出的问题：这段文案来自 registry.ParseImport
	// 自己的校验逻辑，不是第三方错误文本，回传它不构成内部信息泄露，
	// 而不点名的话使用者只知道"导入失败"，找不到是哪一行 YAML 写错了。
	if msg, _ := bodyOf(t, rec)["msg"].(string); !strings.Contains(msg, "ipBlock") {
		t.Errorf("msg = %q, want it to name ipBlock", msg)
	}
}

// podSelector 是字符串而不是对象，这份 YAML 结构合法但类型不对：
// sigs.k8s.io/yaml 底层走 encoding/json，产出的错误文本里直接带着
// Go 结构体类型名 —— "json: cannot unmarshal string into Go struct
// field NetworkPolicySpec.spec.podSelector of type v1.LabelSelector"。
// 这条错误确实是 registry.ErrInvalid 家族（errors.Is 会为真），但
// err.Error() 里的这段文字是 sigs.k8s.io/yaml 写的，不是我们写的 ——
// 回传字面值就是把内部实现类型交给了调用方。writeRegistryError 必须
// 只回传 InvalidError.Detail，不能回传 err.Error()。
func TestImportRejectsTypeMismatchWithoutLeakingGoTypeNames(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: x
  namespace: y
spec:
  podSelector: "not-an-object"
`})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a malformed YAML value is a business failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
	msg, _ := bodyOf(t, rec)["msg"].(string)
	if msg == "" {
		t.Fatal("msg is empty; the caller needs something actionable")
	}
	for _, leaked := range []string{"LabelSelector", "NetworkPolicySpec", "json:", "yaml:"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("msg = %q, leaked internal type/library text %q", msg, leaked)
		}
	}
}

// 请求体不是合法 JSON 是协议层问题，走真实的 400 —— 与 cluster_handler.go
// 的畸形请求体处理一致。此前这条路径没有测试覆盖。
func TestImportRejectsMalformedJSON(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters/prod-asia-1/policy-imports",
		bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

func TestImportRequiresSession(t *testing.T) {
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	rec := postJSONNoAuth(t, h, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": importYAML})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestListImportsRoundTrips(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": importYAML})

	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	data, ok := bodyOf(t, rec)["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want one import", bodyOf(t, rec)["data"])
	}
}

func TestDeleteImportRoundTrips(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	create := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": importYAML})
	importID, ok := bodyOf(t, create)["data"].(map[string]any)["importId"].(string)
	if !ok || importID == "" {
		t.Fatalf("create response missing importId: %v", bodyOf(t, create))
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/clusters/prod-asia-1/policy-imports/"+importID, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(reg.imports["prod-asia-1"]) != 0 {
		t.Errorf("imports = %d, want 0 after delete", len(reg.imports["prod-asia-1"]))
	}
}

// 三个导入端点都必须先解析集群的注册状态。
//
// 外键挡不住这件事：软删除保留 cluster 那一行，外键照样满足。审阅时的
// 实证是 —— 下线 zz-probe 之后 POST /clusters/zz-probe/policy-imports
// 仍然返回 code 0，并写下一条时间戳晚于下线操作的审计记录（spec §4.5）。
func TestImportEndpointsRejectOffboardedCluster(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	// 先导入一条，确认下线前这三个端点是通的，否则「下线后返回 20002」
	// 可能只是因为集群从来就没被接受过。
	create := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": importYAML})
	if got := bodyOf(t, create)["code"]; got != float64(0) {
		t.Fatalf("pre-offboard import code = %v, want 0 (body %s)", got, create.Body.String())
	}
	importID, _ := bodyOf(t, create)["data"].(map[string]any)["importId"].(string)

	del := httptest.NewRequest(http.MethodDelete, "/api/v1/clusters/prod-asia-1", nil)
	del.AddCookie(cookie)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, del)
	if got := bodyOf(t, delRec)["code"]; got != float64(0) {
		t.Fatalf("offboard code = %v, want 0", got)
	}

	stored := len(reg.imports["prod-asia-1"])

	cases := map[string]func() *httptest.ResponseRecorder{
		"list": func() *httptest.ResponseRecorder {
			return authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports")
		},
		"create": func() *httptest.ResponseRecorder {
			return authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
				map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": importYAML})
		},
		"delete": func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodDelete,
				"/api/v1/clusters/prod-asia-1/policy-imports/"+importID, nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call()
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 — an offboarded cluster is a business failure", rec.Code)
			}
			if got := bodyOf(t, rec)["code"]; got != float64(20002) {
				t.Errorf("code = %v, want 20002 (body %s)", got, rec.Body.String())
			}
		})
	}

	if len(reg.imports["prod-asia-1"]) != stored {
		t.Errorf("imports changed from %d to %d — a write landed on an offboarded cluster",
			stored, len(reg.imports["prod-asia-1"]))
	}
}

// 来源为 GIT 却没有 commit，必须在入库前被拒绝。
//
// 这条记录会被界面标成「现状」，而轮 3 的漂移检测拿 commit 做基准 ——
// 没有 commit 的 GIT 记录是一句无法核验的溯源声明（spec §4）。
func TestImportRejectsGitSourceWithoutCommitSHA(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "GIT", "gitCommitSha": "", "yaml": importYAML})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad field combination is a business failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001 (body %s)", got, rec.Body.String())
	}
	if msg, _ := bodyOf(t, rec)["msg"].(string); !strings.Contains(msg, "gitCommitSha") {
		t.Errorf("msg = %q, want it to name gitCommitSha", msg)
	}
	if len(reg.imports["prod-asia-1"]) != 0 {
		t.Error("the import was stored despite failing validation")
	}
}

// 反向：非 GIT 来源却带着 commit，同样要拒绝 —— 一个不指向任何同步动作
// 的 commit 是一句凭空的溯源声明，而界面会照着它显示「已用 Git 核对」。
func TestImportRejectsCommitSHAOnNonGitSource(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{
			"role": "BASELINE_CURRENT", "source": "PASTE",
			"gitCommitSha": "0123456789abcdef0123456789abcdef01234567", "yaml": importYAML,
		})
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001 (body %s)", got, rec.Body.String())
	}
	if len(reg.imports["prod-asia-1"]) != 0 {
		t.Error("the import was stored despite failing validation")
	}
}
