package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/httpapi"
	"github.com/imkerbos/Distill/internal/response"
)

// decodingHandler 模仿五个真实调用点的形状：把请求体解成 JSON，
// 失败即 400 + 20001。
func decodingHandler(t *testing.T, decoded *bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
			return
		}
		*decoded = true
		response.WriteOK(w, nil)
	})
}

// oversizedLoginBody 返回一个合法 JSON、但超过上限的登录请求体。
//
// 刻意是**合法** JSON：这样「被拒绝」只可能来自尺寸限制，不可能来自
// 语法错误 —— 否则限制被摘掉时测试仍然会绿。
func oversizedLoginBody() []byte {
	var b bytes.Buffer
	b.WriteString(`{"username":"demo","password":"`)
	b.Write(bytes.Repeat([]byte("a"), int(httpapi.MaxRequestBytes)+1))
	b.WriteString(`"}`)
	return b.Bytes()
}

// 方向一：守卫本身有效 —— 超过上限的请求体到不了 Decode 的成功分支。
func TestLimitRequestBodyRejectsOversizedBody(t *testing.T) {
	decoded := false
	h := httpapi.LimitRequestBody(1024)(decodingHandler(t, &decoded))

	body := append([]byte(`{"x":"`), append(bytes.Repeat([]byte("a"), 4096), []byte(`"}`)...)...)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))

	if decoded {
		t.Fatal("handler decoded a body larger than the limit")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if got["code"] != float64(20001) {
		t.Errorf("code = %v, want 20001", got["code"])
	}
	// §22：Go 的原始错误文本不得抵达客户端。
	if strings.Contains(rec.Body.String(), "too large") ||
		strings.Contains(rec.Body.String(), "http:") {
		t.Errorf("a raw Go error reached the client: %s", rec.Body.String())
	}
}

// 上限之内的请求体必须照常通过 —— 否则「全都拒绝」也能让上一条测试变绿。
func TestLimitRequestBodyPassesNormalBody(t *testing.T) {
	decoded := false
	h := httpapi.LimitRequestBody(1024)(decodingHandler(t, &decoded))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"x":"y"}`)))

	if !decoded {
		t.Fatal("handler never decoded a body well under the limit")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// 方向二：装配点仍然生效。
//
// 请求体是**合法** JSON 且带正确凭证 —— 一旦 router.go 里的
// LimitRequestBody 被摘掉，这个请求会一路解析成功并返回 200 + code 0，
// 这条测试立刻变红。它验证的不是中间件的行为，而是「它还挂在那里」。
func TestRouterAppliesBodyLimitToUnauthenticatedEndpoint(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewReader(oversizedLoginBody()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — an oversized body reached the decoder", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
}

// 同一条装配的第二个证据：限制同样覆盖会话之后的写端点。
//
// 请求体是一份合法的集群登记 JSON，只是 displayName 超长；没有限制时
// 它会被 memRegistry 收下并返回 200 + code 0。
func TestRouterAppliesBodyLimitToProtectedEndpoint(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, nil, newMemRegistry())

	var b bytes.Buffer
	b.WriteString(`{"id":"c1","podCidr":"10.0.0.0/16","nodeCidr":"10.1.0.0/16","displayName":"`)
	b.Write(bytes.Repeat([]byte("n"), int(httpapi.MaxRequestBytes)+1))
	b.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters", bytes.NewReader(b.Bytes()))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — an oversized body reached the decoder", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
}

// 正常尺寸的登录请求必须仍然成功 —— 证明上面两条不是靠「什么都拒绝」变绿的。
func TestRouterStillAcceptsNormalSizedBody(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)

	rec := postJSON(t, h, "/api/v1/sessions", map[string]string{
		"username": "demo", "password": testPassword,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Errorf("code = %v, want 0 — the limit is rejecting legitimate payloads", got)
	}
}
