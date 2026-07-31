package response_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/imkerbos/Distill/internal/response"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	return got
}

func TestWriteOK(t *testing.T) {
	rec := httptest.NewRecorder()
	response.WriteOK(rec, map[string]string{"hello": "world"})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	got := decode(t, rec)
	if got["code"] != float64(0) {
		t.Errorf("code = %v, want 0", got["code"])
	}
	data, ok := got["data"].(map[string]any)
	if !ok || data["hello"] != "world" {
		t.Errorf("data = %v, want the payload", got["data"])
	}
}

// 业务错误走 HTTP 200：它不该计入服务错误率。
func TestWriteBusinessKeeps200(t *testing.T) {
	rec := httptest.NewRecorder()
	response.WriteBusiness(rec, response.CodeNotFound)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a business-level failure", rec.Code)
	}
	got := decode(t, rec)
	if got["code"] != float64(20002) {
		t.Errorf("code = %v, want 20002", got["code"])
	}
	if got["data"] != nil {
		t.Errorf("data = %v, want null on failure", got["data"])
	}
}

// 系统性问题保留 HTTP 状态码：网关与监控要能统计到。
func TestWriteSystemKeepsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	response.WriteSystem(rec, http.StatusUnauthorized, response.CodeUnauthenticated)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	got := decode(t, rec)
	if got["code"] != float64(10003) {
		t.Errorf("code = %v, want 10003", got["code"])
	}
}

// 内部错误文案必须是固定的：泄出堆栈或文件路径等于把内部结构送给攻击者。
func TestInternalMessageLeaksNothing(t *testing.T) {
	msg := response.CodeInternal.Message()
	if msg == "" {
		t.Fatal("internal code has an empty message")
	}
	for _, leak := range []string{"panic", "goroutine", ".go:", "/Users/", "sql"} {
		if contains(msg, leak) {
			t.Errorf("internal message %q leaks %q", msg, leak)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) &&
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()
}

// 每个已登记的码都必须有文案，否则前端会收到空提示。
func TestEveryCodeHasMessage(t *testing.T) {
	for _, c := range response.AllCodes() {
		if c.Message() == "" {
			t.Errorf("code %d has no message", c)
		}
	}
}
