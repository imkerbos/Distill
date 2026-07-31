package response_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		if strings.Contains(msg, leak) {
			t.Errorf("internal message %q leaks %q", msg, leak)
		}
	}
}

// 每个已登记的码都必须有文案，否则前端会收到空提示。
//
// 不能通过 Message() == "" 判断：Message() 对未登记的码会退回
// CodeInternal 文案（见其注释），永远不返回空串，用它做探针只会让
// 这个测试形同虚设。必须直接比对 MessagedCodes()。
func TestEveryCodeHasMessage(t *testing.T) {
	messaged := map[response.Code]bool{}
	for _, c := range response.MessagedCodes() {
		messaged[c] = true
	}
	for _, c := range response.AllCodes() {
		if !messaged[c] {
			t.Errorf("code %d is registered but has no message", c)
		}
	}
}

// 反方向：文案表里不能有未登记的码，否则 AllCodes 会漏掉它，
// 上一个测试就成了摆设。
func TestNoMessageWithoutRegistration(t *testing.T) {
	registered := map[response.Code]bool{}
	for _, c := range response.AllCodes() {
		registered[c] = true
	}
	for _, c := range response.MessagedCodes() {
		if !registered[c] {
			t.Errorf("code %d has a message but is not in AllCodes()", c)
		}
	}
}

// 未登记的码必须退回内部错误文案，而不是空串：空 msg 会让前端渲染
// 一个没有任何说明的错误框，用户和排障的人都无从下手。
func TestMessageFallsBackForUnregisteredCode(t *testing.T) {
	got := response.Code(987654).Message()
	if got == "" {
		t.Fatal("an unregistered code produced an empty message; the UI would render a blank error")
	}
	if got != response.CodeInternal.Message() {
		t.Errorf("Message() = %q, want the internal-error text", got)
	}
}
