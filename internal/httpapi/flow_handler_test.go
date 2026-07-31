package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/imkerbos/Distill/internal/store"
)

func TestFlowsEndpoint(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/flows?limit=5")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int                `json:"code"`
		Data []store.FlowRecord `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 5 {
		t.Errorf("got %d flows, want 5", len(env.Data))
	}
	for _, f := range env.Data {
		if f.Verdict == "" || f.Confidence == "" {
			t.Errorf("flow %s missing verdict or confidence", f.ID)
		}
	}
}

func TestFlowsFilterByVerdict(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/flows?verdict=DENY&limit=200")

	var env struct {
		Data []store.FlowRecord `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) == 0 {
		t.Fatal("no DENY flows returned")
	}
	for _, f := range env.Data {
		if f.Verdict != "DENY" {
			t.Fatalf("filter leaked a %s flow", f.Verdict)
		}
	}
}

// limit 不是数字时是参数问题，返回 20001。
func TestFlowsRejectsBadLimit(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/flows?limit=many")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad query value is a business-level failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
}

func TestFlowDecisionEndpoint(t *testing.T) {
	h, cookie := newFullRouter(t)

	list := authedGet(t, h, cookie, "/api/v1/flows?limit=1")
	var listEnv struct {
		Data []store.FlowRecord `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listEnv); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listEnv.Data) == 0 {
		t.Fatal("no flows to inspect")
	}

	rec := authedGet(t, h, cookie, "/api/v1/flows/"+listEnv.Data[0].ID+"/decision")
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
	if env.Data.ID != listEnv.Data[0].ID {
		t.Errorf("decision is for flow %q, want %q", env.Data.ID, listEnv.Data[0].ID)
	}
}

func TestFlowDecisionUnknownID(t *testing.T) {
	h, cookie := newFullRouter(t)
	rec := authedGet(t, h, cookie, "/api/v1/flows/flow-999999/decision")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}

func TestFlowsRequiresAuth(t *testing.T) {
	h, _ := newFullRouter(t)
	rec := authedGet(t, h, &http.Cookie{Name: "distill_session", Value: "bogus"}, "/api/v1/flows") //nolint:gosec // G124: request cookie, not a response header

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
