package agentauth_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/agentauth"
)

func TestIssueProducesAParsableSelfMatchingToken(t *testing.T) {
	token, agentID, hash, err := agentauth.Issue()
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if !strings.HasPrefix(token, agentauth.Prefix) {
		t.Errorf("Issue() token = %q, want the %q prefix — 前缀是一把落进日志、"+
			"仓库或工单的 token 能被扫描器认出来的唯一线索", token, agentauth.Prefix)
	}
	got, ok := agentauth.Parse(token)
	if !ok {
		t.Fatalf("Parse(%q) = not ok, want ok", token)
	}
	if got != agentID {
		t.Errorf("Parse() agent id = %q, want %q — 签发与解析必须对同一个公开段"+
			"达成一致，否则认证会去查一条不存在的记录", got, agentID)
	}
	if len(hash) != sha256.Size {
		t.Errorf("Issue() hash length = %d, want %d", len(hash), sha256.Size)
	}
	if !agentauth.Matches(token, hash) {
		t.Error("Matches(issued token, its own hash) = false")
	}
}

func TestIssuedAgentIDIsAcceptedByTheRegistry(t *testing.T) {
	// 签发侧与校验侧对"什么算合法 agent id"必须是同一个约定。各写一份，
	// 漂的那天症状是签发出来的 token 存不进库，而两边都"看起来没错"。
	_, agentID, _, err := agentauth.Issue()
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if len(agentID) != 16 {
		t.Errorf("agent id %q has length %d, want 16 (registry.AgentIDLen)", agentID, len(agentID))
	}
	for _, c := range agentID {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("agent id %q is not lowercase hex", agentID)
		}
	}
}

func TestIssueNeverRepeats(t *testing.T) {
	seenToken, seenID := make(map[string]bool), make(map[string]bool)
	for i := 0; i < 512; i++ {
		token, agentID, _, err := agentauth.Issue()
		if err != nil {
			t.Fatalf("Issue() error = %v", err)
		}
		if seenToken[token] || seenID[agentID] {
			t.Fatal("Issue() repeated a token or an agent id — 重复的公开段意味着" +
				"两个集群的 agent 可能认到同一条记录上")
		}
		seenToken[token], seenID[agentID] = true, true
	}
}

// secretOf 取出一把 token 的秘密段。
func secretOf(t *testing.T, token string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(token, agentauth.Prefix)
	if !ok {
		t.Fatalf("token %q has no prefix", token)
	}
	_, secret, ok := strings.Cut(rest, "_")
	if !ok {
		t.Fatalf("token %q has no secret segment", token)
	}
	return secret
}

func TestIssuedSecretsCarryTheirOwnEntropy(t *testing.T) {
	// **公开段随机不等于 token 不可预测。** 只断言"两把 token 不相同"是
	// 抓不住这件事的：公开段本来就不同，于是即使秘密段恒为全零、每一把
	// token 都能由它的 agent id 直接推出来，那条断言依然是绿的。
	//
	// 这里单独盯秘密段：它才是这把 token 唯一的秘密，而 Matches 的强度
	// 完全建立在它之上。
	seen := make(map[string]bool)
	zero := strings.Repeat("A", len(secretOf(t, mustIssue(t)))) // base64 全零的形状
	for i := 0; i < 128; i++ {
		secret := secretOf(t, mustIssue(t))
		if secret == zero {
			t.Fatal("issued a token whose secret segment is all zeroes — 秘密段没有熵，" +
				"每把 token 都能由它的 agent id 推出来")
		}
		if seen[secret] {
			t.Fatal("Issue() repeated a secret segment")
		}
		seen[secret] = true
	}
}

func mustIssue(t *testing.T) string {
	t.Helper()
	token, _, _, err := agentauth.Issue()
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	return token
}

func TestMatchesRejectsAnythingButTheRightToken(t *testing.T) {
	token, _, hash, _ := agentauth.Issue()
	other, _, _, _ := agentauth.Issue()

	if agentauth.Matches(other, hash) {
		t.Error("Matches(another token, hash) = true")
	}
	if agentauth.Matches("", hash) {
		t.Error(`Matches("", hash) = true — 一个忘了带 Authorization 头的请求` +
			`不得撞上任何一条哈希`)
	}
	if agentauth.Matches(token, nil) {
		t.Error("Matches(token, nil) = true — 哈希缺失时必须拒绝，不是放行（规范 §2 Fail Secure）")
	}
	if agentauth.Matches(token, make([]byte, 16)) {
		t.Error("Matches(token, 16-byte hash) = true")
	}
}

func TestMatchesRejectsTheEmptyTokenEvenAgainstItsOwnDigest(t *testing.T) {
	// 空 token 的摘要是一个固定值。写入侧只要出过一次把它落库的 bug
	// （比如某条路径拼出了空 token 再算摘要），一个**根本没带
	// Authorization 头**的请求就会认证通过。
	//
	// 这一条是 Matches 里那句 `token == ""` 的唯一绑定点：去掉它，
	// 下面这行就会返回 true。
	empty := sha256.Sum256([]byte(""))
	if agentauth.Matches("", empty[:]) {
		t.Error(`Matches("", sha256("")) = true — 一个不带凭据的请求认证通过了`)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, tok := range []string{
		"",
		"dstl_",
		"dstl_abc",
		"dstl_0011223344556677",            // 缺秘密段
		"dstl_0011223344556677_",           // 秘密段为空
		"dstl_0011223344556g77_c2VjcmV0",   // 公开段非 hex
		"dstl_00112233445566_c2VjcmV0",     // 公开段偏短
		"dstl_0011223344556677aa_c2VjcmV0", // 公开段偏长
		"nodstl_0011223344556677_c2VjcmV0", // 前缀不对
		"0011223344556677_c2VjcmV0",        // 没有前缀
	} {
		if id, ok := agentauth.Parse(tok); ok {
			t.Errorf("Parse(%q) = (%q, true), want not ok", tok, id)
		}
	}
}

func TestParseDoesNotAuthenticate(t *testing.T) {
	// Parse 只做形状检查。一个形状正确但根本不存在的 token 必须能被解析出
	// 公开段 —— 认证要靠查库 + Matches 两步，而"查不到记录"与"token 不对"
	// 在调用方那里走同一个响应、在日志里分得开。把两者合进 Parse，
	// 就再也分不开了。
	if _, ok := agentauth.Parse("dstl_ffffffffffffffff_bm90YXJlYWx0b2tlbg"); !ok {
		t.Error("Parse(well-formed but unknown token) = not ok — 形状正确就该解析得出，" +
			"存不存在是下一步的事")
	}
}
