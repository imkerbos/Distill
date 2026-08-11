package httpapi_test

import (
	"net/http"
	"net/http/httptest"
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
	reg := newMemRegistry()
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
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
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
}

func TestImportRequiresSession(t *testing.T) {
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	rec := postJSONNoAuth(t, h, "/api/v1/clusters/prod-asia-1/policy-imports",
		map[string]any{"role": "BASELINE_CURRENT", "source": "PASTE", "yaml": importYAML})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestListImportsRoundTrips(t *testing.T) {
	reg := newMemRegistry()
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
	reg := newMemRegistry()
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
