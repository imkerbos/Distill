package httpapi_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/agentauth"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// issueAgent 走真实端点签发一把 token，返回 (agentID, token)。
//
// 不直接往 memRegistry 里塞：这些用例要验的正是端点这一段，绕过它等于
// 让测试自己回答自己的问题。
func issueAgent(t *testing.T, h http.Handler, cookie *http.Cookie, clusterID string) (string, string) {
	t.Helper()
	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/"+clusterID+"/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("issue status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data, ok := bodyOf(t, rec)["data"].(map[string]any)
	if !ok {
		t.Fatalf("issue response has no data object: %s", rec.Body.String())
	}
	agentID, _ := data["agentId"].(string)
	token, _ := data["token"].(string)
	if agentID == "" || token == "" {
		t.Fatalf("issue response is missing agentId or token: %s", rec.Body.String())
	}
	return agentID, token
}

func TestIssueAgentReturnsAUsableToken(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	agentID, token := issueAgent(t, h, cookie, "prod-asia-1")

	if !strings.HasPrefix(token, agentauth.Prefix) {
		t.Errorf("token = %q, want the %q prefix", token, agentauth.Prefix)
	}
	// 签发出来的 token 必须能被解析回同一个公开段，否则认证会去查一条
	// 不存在的记录 —— 而那时端点看起来完全正常。
	if got, ok := agentauth.Parse(token); !ok || got != agentID {
		t.Errorf("Parse(issued token) = (%q, %v), want (%q, true)", got, ok, agentID)
	}
	// 库里存的必须是这把 token 的摘要，不是别的什么。
	stored, found, err := reg.ClusterAgentByID(t.Context(), agentID)
	if err != nil || !found {
		t.Fatalf("ClusterAgentByID() = (_, %v, %v), want found", found, err)
	}
	if !agentauth.Matches(token, stored.TokenHash) {
		t.Error("the stored digest does not match the token that was handed out — " +
			"这把 token 永远认不过，而签发那一刻看起来是成功的")
	}
	if stored.ClusterID != "prod-asia-1" {
		t.Errorf("stored ClusterID = %q, want prod-asia-1", stored.ClusterID)
	}
}

func TestAgentTokenIsShownExactlyOnce(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, token := issueAgent(t, h, cookie, "prod-asia-1")

	list := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/agents")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	// 列表确实回了这条记录 —— 否则下面两条断言只是在空列表上成立。
	if !strings.Contains(body, agentID) {
		t.Fatalf("the agent list does not contain %s: %s", agentID, body)
	}
	// 明文只在签发那一次出现（规范 §19、§20）。列表回显等于把一次泄漏
	// 变成永久可读。
	if strings.Contains(body, token) {
		t.Error("the agent list echoed the plaintext token")
	}
	// 摘要同样不出边界：它是离线爆破的输入。
	// 三种形态都查：Go 对 []byte 默认序列化成 base64，而一条只查 hex 的
	// 断言会在最可能发生的那种泄漏上静默通过。
	stored, _, _ := reg.ClusterAgentByID(t.Context(), agentID)
	for _, form := range []string{
		string(stored.TokenHash),
		hexOf(stored.TokenHash),
		base64.StdEncoding.EncodeToString(stored.TokenHash),
	} {
		if form != "" && strings.Contains(body, form) {
			t.Errorf("the agent list leaked the token digest in the form %q", form)
		}
	}
	if strings.Contains(strings.ToLower(body), "hash") {
		t.Errorf("the agent list has a field that looks like a digest: %s", body)
	}
}

// hexOf 把摘要渲染成十六进制，用来搜"换了个编码就漏出去"的情况。
func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

func TestIssueAgentRequiresAdmin(t *testing.T) {
	reg := fixtureSource()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	reg.withAccount(t, "alice", registry.RoleViewer, viewerPassword)
	viewer := sessionCookie(t, sessions, reg, "alice", registry.RoleViewer)

	// 签发一把能往平台写数据的凭据是权限变更（规范 §28）。
	issue := authedPostJSON(t, h, viewer, "/api/v1/clusters/prod-asia-1/agents", nil)
	if issue.Code != http.StatusForbidden {
		t.Errorf("viewer issue status = %d, want 403", issue.Code)
	}
	// 列表也要管理员：它回答的是「这个集群有几把能往平台写数据的钥匙、
	// 上次什么时候用的」，那是凭据台账，不是只读业务数据。
	list := authedGet(t, h, viewer, "/api/v1/clusters/prod-asia-1/agents")
	if list.Code != http.StatusForbidden {
		t.Errorf("viewer list status = %d, want 403", list.Code)
	}
	del := authedDelete(t, h, viewer, "/api/v1/clusters/prod-asia-1/agents/0011223344556677")
	if del.Code != http.StatusForbidden {
		t.Errorf("viewer revoke status = %d, want 403", del.Code)
	}
}

func TestIssueAgentForAnUnknownClusterIsNotFound(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	rec := authedPostJSON(t, h, cookie, "/api/v1/clusters/no-such-cluster/agents", nil)
	// 业务失败走 HTTP 200 + 业务码（response.WriteBusiness 的约定）：
	// 查一个不存在的 ID 不是服务出了问题，不该计进服务错误率。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNotFound) {
		t.Errorf("code = %v, want %d — 给一个不存在的集群签发凭据，那把 token "+
			"之后会认到一个查不到集群的归属上", got, response.CodeNotFound)
	}
}

func TestRevokeAgentThenItIsListedAsRevoked(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, _ := issueAgent(t, h, cookie, "prod-asia-1")

	del := authedDelete(t, h, cookie, "/api/v1/clusters/prod-asia-1/agents/"+agentID)
	if del.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200: %s", del.Code, del.Body.String())
	}

	// 吊销不删行：列表要看得见这个集群历史上签过几把。
	list := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/agents")
	body := list.Body.String()
	if !strings.Contains(body, agentID) {
		t.Errorf("the revoked agent vanished from the list: %s", body)
	}
	if !strings.Contains(body, string(registry.AgentRevoked)) {
		t.Errorf("the list does not say the agent is revoked: %s", body)
	}
}

func TestRevokeAnUnknownAgentIsNotFound(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	rec := authedDelete(t, h, cookie, "/api/v1/clusters/prod-asia-1/agents/ffffffffffffffff")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNotFound) {
		t.Errorf("code = %v, want %d", got, response.CodeNotFound)
	}
}

func TestRevokeWillNotReachAcrossClusters(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, _ := issueAgent(t, h, cookie, "prod-asia-1")

	// 路径上的集群与这把 token 绑定的集群不一致时必须落空。少了这一条，
	// 一个管理员能吊销别的集群的 agent，而界面上看起来他只在操作自己那个。
	rec := authedDelete(t, h, cookie, "/api/v1/clusters/prod-eu-1/agents/"+agentID)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNotFound) {
		t.Errorf("cross-cluster revoke code = %v, want %d", got, response.CodeNotFound)
	}
	stored, _, _ := reg.ClusterAgentByID(t.Context(), agentID)
	if stored.State != registry.AgentActive {
		t.Errorf("State = %q after a cross-cluster revoke, want it untouched", stored.State)
	}
}

func TestAgentListIsScopedToItsCluster(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	agentID, _ := issueAgent(t, h, cookie, "prod-asia-1")

	other := authedGet(t, h, cookie, "/api/v1/clusters/prod-eu-1/agents")
	if strings.Contains(other.Body.String(), agentID) {
		t.Errorf("prod-eu-1 listed prod-asia-1's agent: %s", other.Body.String())
	}
}
