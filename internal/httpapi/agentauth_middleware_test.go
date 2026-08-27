package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/agentauth"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// bearerGet 用 agent token 发一次请求，不带任何 Cookie。
func bearerGet(t *testing.T, h http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// bearerPost 用 agent token 发一次写请求。
//
// 与 bearerGet 分开而不是加一个 method 参数：读路径与写路径各自挂中间件，
// 用例要能分别打到它们 —— 「读被挡住」证明不了「写被挡住」。
func bearerPost(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// cookieGet 用会话 Cookie 发一次请求，不带 Authorization。
func cookieGet(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentTokenAuthenticatesOnTheAgentLane(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := bearerGet(t, h, token, "/api/v1/agent/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// 这一条 token 绑定的集群必须出现在回答里：它就是摄入时的归属来源。
	if !strings.Contains(rec.Body.String(), "prod-asia-1") {
		t.Errorf("config did not name the agent's own cluster: %s", rec.Body.String())
	}
}

// --- 两条钉子：两条认证链互不相通（design doc 2026-08-18 §3.3）---

func TestAgentLaneRejectsASessionCookie(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	// 一个管理员的浏览器会话不得成为一次摄入的身份：摄入是往身份表里
	// **写**，而会话代表的是一个人在看页面。
	rec := cookieGet(t, h, cookie, "/api/v1/agent/config")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("session cookie on the agent lane status = %d, want 401: %s",
			rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeAgentUnauthenticated) {
		t.Errorf("code = %v, want %d", got, response.CodeAgentUnauthenticated)
	}
}

func TestHumanLaneRejectsAnAgentToken(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 一把泄漏的节点 agent token 不得成为一把能读全平台的钥匙。
	for _, path := range []string{
		"/api/v1/clusters",
		"/api/v1/clusters/prod-asia-1/topology",
		"/api/v1/clusters/prod-asia-1/agents",
		"/api/v1/accounts",
		"/api/v1/settings",
	} {
		rec := bearerGet(t, h, token, path)
		if rec.Code == http.StatusOK {
			t.Errorf("agent token opened %s (status 200): %s", path, rec.Body.String())
		}
	}
}

// agentLanePrefix 是 agent 子树的挂载点。
const agentLanePrefix = "/api/v1/agent/"

// walkRoutes 列出路由器上注册的每一条路由（方法 + 已填好参数的路径）。
//
// 从**真实路由表**遍历而不是维护一张清单：新增端点会自动进入下面两条
// 断言，而清单只会在有人记得改它的时候才跟上 —— 忘记改的那一条，正是
// 会被挑中的那一条。
func walkRoutes(t *testing.T, h http.Handler) [][2]string {
	t.Helper()
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("router is %T, which cannot be walked — this test can no longer see the route table", h)
	}
	var out [][2]string
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := route
		for name, value := range routeParams {
			path = strings.ReplaceAll(path, name, value)
		}
		if strings.Contains(path, "{") {
			t.Errorf("route %s has a path parameter this test does not know how to fill", route)
			return nil
		}
		out = append(out, [2]string{method, path})
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return out
}

func TestEveryAgentRouteRefusesASession(t *testing.T) {
	// 这条与 TestAgentLaneRejectsASessionCookie 不重复：那条盯的是一个
	// 具体端点，这条从路由表遍历 —— 明天新加的 agent 端点自动被覆盖，
	// 而不是等有人记得补一条用例。
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	seen := 0
	for _, mp := range walkRoutes(t, h) {
		method, path := mp[0], mp[1]
		if !strings.HasPrefix(path, agentLanePrefix) {
			continue
		}
		seen++
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with an admin session status = %d, want 401 — 这条 agent "+
				"端点接受了人的会话", method, path, rec.Code)
		}
	}
	// 一条都没走到说明遍历没找到 agent 子树，上面的循环等于没跑。
	if seen == 0 {
		t.Fatal("no agent route was walked; this test proved nothing")
	}
}

func TestNoHumanRouteAcceptsAnAgentToken(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	seen := 0
	for _, mp := range walkRoutes(t, h) {
		method, path := mp[0], mp[1]
		if strings.HasPrefix(path, agentLanePrefix) {
			continue
		}
		if method == http.MethodPost && path == "/api/v1/sessions" {
			// 登录本来就不需要身份，它拒绝的是错的口令，不是错的 token。
			continue
		}
		seen++
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s accepted an agent token — 一把泄漏的节点 agent token "+
				"成了一把能操作平台的钥匙", method, path)
		}
	}
	if seen == 0 {
		t.Fatal("no human route was walked; this test proved nothing")
	}
}

// --- 认证判定 ---

func TestAgentLaneRejectsRevokedUnknownAndMalformedAlike(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, token := issueAgent(t, h, cookie, "prod-asia-1")

	if rec := bearerGet(t, h, token, "/api/v1/agent/config"); rec.Code != http.StatusOK {
		t.Fatalf("fresh token status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if rec := authedDelete(t, h, cookie,
		"/api/v1/clusters/prod-asia-1/agents/"+agentID); rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", rec.Code)
	}

	revoked := bearerGet(t, h, token, "/api/v1/agent/config")
	if revoked.Code != http.StatusUnauthorized {
		t.Errorf("revoked token status = %d, want 401: %s", revoked.Code, revoked.Body.String())
	}
	unknown := bearerGet(t, h, "dstl_ffffffffffffffff_bm90YXJlYWx0b2tlbg", "/api/v1/agent/config")
	if unknown.Code != http.StatusUnauthorized {
		t.Errorf("unknown token status = %d, want 401", unknown.Code)
	}
	malformed := bearerGet(t, h, "not-a-token", "/api/v1/agent/config")
	if malformed.Code != http.StatusUnauthorized {
		t.Errorf("malformed token status = %d, want 401", malformed.Code)
	}
	missing := bearerGet(t, h, "", "/api/v1/agent/config")
	if missing.Code != http.StatusUnauthorized {
		t.Errorf("missing token status = %d, want 401", missing.Code)
	}

	// 四种失败对调用方是同一句话：分开回答等于帮试探者确认哪个 agent_id
	// 是存在的、哪一把只是被吊销了（规范 §22）。差别只进日志。
	bodies := map[string]string{
		"revoked":   revoked.Body.String(),
		"unknown":   unknown.Body.String(),
		"malformed": malformed.Body.String(),
		"missing":   missing.Body.String(),
	}
	for name, body := range bodies {
		if body != bodies["unknown"] {
			t.Errorf("the %s response body differs from the unknown one:\n %s\n %s",
				name, body, bodies["unknown"])
		}
	}
}

func TestAgentLaneRejectsAForgedSecretOnAKnownAgentID(t *testing.T) {
	// **公开段存在不等于这把 token 是真的。** 只用"不存在的 agent_id"去测
	// 拒绝，是抓不住这件事的：那条路径在查库那一步就返回了，摘要比对
	// 根本没被执行到 —— 于是把比对整段删掉，测试依然全绿。
	//
	// 这里保留真实的公开段、只换掉秘密段：唯一能拒绝它的就是摘要比对。
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, token := issueAgent(t, h, cookie, "prod-asia-1")

	forged := agentauth.Prefix + agentID + "_" + "Zm9yZ2VkLXNlY3JldC12YWx1ZQ"
	if got, ok := agentauth.Parse(forged); !ok || got != agentID {
		t.Fatalf("the forged token does not carry the real agent id; this test would prove nothing")
	}
	if forged == token {
		t.Fatal("the forged token equals the real one; this test would prove nothing")
	}

	rec := bearerGet(t, h, forged, "/api/v1/agent/config")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("forged secret status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentLaneRecordsLastSeen(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, token := issueAgent(t, h, cookie, "prod-asia-1")

	before, _, _ := reg.ClusterAgentByID(t.Context(), agentID)
	if !before.LastSeenAt.IsZero() {
		t.Fatalf("LastSeenAt = %v before any use, want zero", before.LastSeenAt)
	}
	if rec := bearerGet(t, h, token, "/api/v1/agent/config"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	after, _, _ := reg.ClusterAgentByID(t.Context(), agentID)
	if after.LastSeenAt.IsZero() {
		t.Error("LastSeenAt is still zero after a successful call — 操作者没法回答" +
			"「这个 agent 还活着吗」")
	}
}

func TestAgentConfigLeaksNothingAboutTheFleet(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	body := bearerGet(t, h, token, "/api/v1/agent/config").Body.String()
	// 网段判定挪到了平台侧（design doc §3.4）：agent 不需要知道别的集群
	// 存在，也就不该知道。把 fleet 网段下发给每一个被管集群，等于把整个
	// fleet 的拓扑发出去。
	for _, leak := range []string{"prod-eu-1", "podCidr", "podCIDR", "10.4.0.0", "nodeCidr", "10.128.0.0"} {
		if strings.Contains(body, leak) {
			t.Errorf("the agent config leaked %q: %s", leak, body)
		}
	}
}

func TestAgentLaneAcceptsOnlyTheBearerScheme(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	for _, header := range []string{token, "Basic " + token, "bearer " + token, "Bearer"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/config", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q status = %d, want 401", header, rec.Code)
		}
	}
}

var _ = registry.AgentActive

// 集群下线之后，它的 agent 凭据必须立刻失效。
//
// 来自一次真实排查：建集群 → 签 token → 下线 → 用同一把 token，
// /agent/config 照旧返回成功，一次完整的采集推送也照旧落库。而集群已经从
// 每一屏消失，那些凭据没有任何界面可以看见或吊销 —— 影响是凭据、账单
// （CLAUDE.md §5）与审计三条：库里会留下一行时间戳晚于下线操作的观测。
//
// 这一条走完整链路。**两道防线各自还有一条单独的用例**：它们互为兜底，
// 只测这一条的话，杀掉其中任何一道都不会变红（实测如此）。
func TestAgentLaneRejectsATokenWhoseClusterWasRetired(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	if rec := bearerGet(t, h, token, "/api/v1/agent/config"); rec.Code != http.StatusOK {
		t.Fatalf("fresh token status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := authedDelete(t, h, cookie, "/api/v1/clusters/prod-asia-1"); rec.Code != http.StatusOK {
		t.Fatalf("retire status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if rec := bearerGet(t, h, token, "/api/v1/agent/config"); rec.Code != http.StatusUnauthorized {
		t.Errorf("config after retiring the cluster = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	// 写路径单独打一次：两条路由各自挂中间件，漏挂一条不会有任何症状。
	run := bearerPost(t, h, token, "/api/v1/agent/collection-runs", `{
		"schemaVersion":1,"runId":"retired-run-1","status":"OK",
		"startedAt":"2026-08-27T02:00:00Z","finishedAt":"2026-08-27T02:00:01Z",
		"observedAt":"2026-08-27T02:00:00Z","observation":{"namespaces":[{"name":"default"}]}}`)
	if run.Code != http.StatusUnauthorized {
		t.Errorf("push after retiring the cluster = %d, want 401: %s", run.Code, run.Body.String())
	}
}

// 防线一：认证层据集群状态拒绝，与这把 token 自己的状态无关。
//
// 这一道才是承重的那条：它对**所有**已签发的 token 生效，包括下线动作
// 发生之前就发出去的、以及本次改动之前那些从未被吊销过的。这里刻意让
// token 保持 ACTIVE —— 只有这样才测得到"拒绝来自集群，不来自吊销"。
func TestAgentLaneRefusesAnActiveTokenOfAVanishedCluster(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 只让集群消失，不碰 token：模拟一把下线之前就签出去的凭据。
	delete(reg.clusters, "prod-asia-1")
	if got := reg.agents[agentID].State; got != registry.AgentActive {
		t.Fatalf("fixture 前提不成立：token 状态是 %q，这条用例要的是 ACTIVE", got)
	}

	if rec := bearerGet(t, h, token, "/api/v1/agent/config"); rec.Code != http.StatusUnauthorized {
		t.Errorf("an ACTIVE token of a vanished cluster got %d, want 401: %s",
			rec.Code, rec.Body.String())
	}
}

// 防线二：下线顺带吊销这个集群的全部 token。
//
// 单独断言在注册表上，不经 HTTP：经 HTTP 的话防线一会先把请求挡掉，
// 这一半永远不会被测到。它要保证的是库里不存着一句自相矛盾的话 ——
// 一个不存在的集群有可用凭据 —— 因为那些凭据再也没有界面可以看见。
func TestRetiringAClusterRevokesItsAgentTokens(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, _ := issueAgent(t, h, cookie, "prod-asia-1")

	if rec := authedDelete(t, h, cookie, "/api/v1/clusters/prod-asia-1"); rec.Code != http.StatusOK {
		t.Fatalf("retire status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := reg.agents[agentID].State; got != registry.AgentRevoked {
		t.Errorf("token state after retiring the cluster = %q, want %q",
			got, registry.AgentRevoked)
	}
	if reg.agents[agentID].RevokedAt.IsZero() {
		t.Error("token was marked revoked without a revocation time")
	}
}
