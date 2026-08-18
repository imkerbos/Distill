package httpapi_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/httpapi"
	"github.com/imkerbos/Distill/internal/response"
)

// 一次流量摄入装不下 1 MiB：UAT 650 个 Pod 按 (src, dst, proto, port) 去重后
// 量级是 1–2 万条连接，JSON 约 1–2 MB。人的子树那个上限是照着几百字节的
// 表单与一份 NetworkPolicy 清单定的，两边差三个数量级。
//
// 因此上限按子树声明。这条用例守住 agent 那一侧真的放宽了 ——
// **且必须让报文大到人的子树会拒**，否则改回一个共用上限它照样绿。
func TestTheAgentSubtreeAcceptsABodyTheHumanSubtreeWouldRefuse(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 一份形状正确的上报，塞进足够多的 Pod 把它撑过 MaxRequestBytes。
	// 合法 JSON 是刻意的：被拒只可能来自尺寸，不可能来自语法 ——
	// 否则上限被摘掉时这条用例仍然绿。
	var pods bytes.Buffer
	for i := 0; pods.Len() <= int(httpapi.MaxRequestBytes); i++ {
		if i > 0 {
			pods.WriteString(",")
		}
		pods.WriteString(`{"namespace":"app","name":"web-`)
		pods.WriteString(strings.Repeat("x", 200))
		pods.WriteString(`","ip":"10.128.0.5","uid":"u","phase":"Running"}`)
	}
	body := `{"schemaVersion":1,"runId":"r-big","status":"OK",
	  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:05Z",
	  "observedAt":"2026-08-18T00:00:05Z",
	  "observation":{"pods":[` + pods.String() + `]}}`

	if int64(len(body)) <= httpapi.MaxRequestBytes {
		t.Fatalf("body is %d bytes, not above the human limit %d — this case proves nothing",
			len(body), httpapi.MaxRequestBytes)
	}
	if int64(len(body)) >= httpapi.MaxAgentRequestBytes {
		t.Fatalf("body is %d bytes, above the agent limit %d too — it would be refused for the "+
			"right reason by accident", len(body), httpapi.MaxAgentRequestBytes)
	}

	rec := postRun(t, h, token, body)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: an asset push above the human body limit was refused, "+
			"so a flow ingest from a real cluster could never get in (%s)",
			got, truncate(rec.Body.String()))
	}
}

// agent 子树仍然有上限 —— 放宽不是取消。
func TestTheAgentSubtreeStillHasACeiling(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	var b bytes.Buffer
	b.WriteString(`{"schemaVersion":1,"runId":"`)
	b.Write(bytes.Repeat([]byte("r"), int(httpapi.MaxAgentRequestBytes)+1))
	b.WriteString(`","status":"OK"}`)

	rec := postRun(t, h, token, b.String())
	if got := bodyOf(t, rec)["code"]; got == float64(response.CodeOK) {
		t.Fatal("the agent subtree accepted an unbounded body")
	}
}

// **根部不再兜底，于是「新增一条子树忘了声明上限」有了落脚点。**
//
// 这条用例逐条走完路由表，把每条带请求体的路由归到它的子树上，并要求
// 每一条子树都在本用例认识的名单里。新增一条子树时，它会红在这里 ——
// 那正是提醒去声明上限的地方。
//
// 只走路由表、不发请求：发请求要为每条路由造合法的认证与路径参数，而
// 认证失败的 401 与尺寸拒绝的 400 长得不一样，一条走不到 handler 的用例
// 会以"被拒了"的面貌给出虚假的安心。
func TestEveryBodyTakingSubtreeIsAccountedFor(t *testing.T) {
	h, _, _ := newTestRouter(t, nil)
	router, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("router is %T, which cannot be walked", h)
	}

	// 已知子树，各自的上限在 router.go 里声明。
	known := map[string]bool{
		"/api/v1/sessions": true, // 登录，MaxRequestBytes
		"/api/v1/agent":    true, // agent，MaxAgentRequestBytes
	}

	seen := map[string]bool{}
	err := chi.Walk(router, func(
		method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler,
	) error {
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			return nil
		}
		route = strings.TrimSuffix(route, "/")
		switch {
		case route == "/api/v1/sessions":
			seen["/api/v1/sessions"] = true
		case strings.HasPrefix(route, "/api/v1/agent/"):
			seen["/api/v1/agent"] = true
		default:
			// 其余全部属于会话保护的那条子树。
			seen["protected"] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	for name := range seen {
		if name == "protected" {
			continue
		}
		if !known[name] {
			t.Errorf("route subtree %q takes a request body but this test does not know it; "+
				"declare a body limit for it in router.go and add it here", name)
		}
	}
	// 三条子树都必须真的出现过 —— 少了任何一条，说明这条用例走空了。
	if !seen["/api/v1/sessions"] || !seen["/api/v1/agent"] || !seen["protected"] {
		t.Errorf("walked routes cover %v, want all three subtrees; this case proved nothing", seen)
	}
}

// truncate 把过长的响应体截短，免得失败输出淹掉终端。
func truncate(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}
