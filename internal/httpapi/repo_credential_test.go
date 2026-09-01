package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"testing"

	"github.com/imkerbos/Distill/internal/auth"
)

// memCredentials 是一个内存里的凭据保管处。
//
// 它**只记下被交给它的东西**，不参与任何断言逻辑 —— 用例要证明的是
// "私钥进了保管处、没进响应"，而不是保管处自己怎么加密的
// （那由 dbsecrets 的用例守）。
type memCredentials struct {
	mu   sync.Mutex
	kept map[string][]byte
	err  error
}

func newMemCredentials() *memCredentials {
	return &memCredentials{kept: map[string][]byte{}}
}

func (m *memCredentials) Put(_ context.Context, ref string, plaintext []byte) error {
	if m.err != nil {
		return m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kept[ref] = append([]byte(nil), plaintext...)
	return nil
}

func (m *memCredentials) Delete(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.kept, ref)
	return nil
}

func (m *memCredentials) Has(_ context.Context, ref string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.kept[ref]
	return ok, nil
}

func (m *memCredentials) get(ref string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kept[ref]
}

// newTestPrivateKey 现生成一把真的 SSH 私钥。
//
// 现生成而不是硬编码一段：硬编码的私钥会被密钥扫描器报成泄漏，而且一段
// 手写的 base64 根本解析不了——那会让这些用例测成"拒绝路径"，与它们要测
// 的那条正好相反（第一版就是这么红的）。
func newTestPrivateKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "distill-test")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

func postRepo(t *testing.T, h http.Handler, c *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/git-repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// **私钥只入不出。** 这是整个特性的安全边界：写进去之后，除了平台自己
// 拿去连 Git，没有任何 HTTP 路径能取回它。
//
// 一条会回显私钥的接口，比把私钥明文落库更糟 —— 后者至少还要有人拿到
// 数据库，前者只要有个会话。
func TestPrivateKeyNeverComesBackOutOfTheAPI(t *testing.T) {
	creds := newMemCredentials()
	h, _, cookie := newTestRouterWithCredentials(t, creds)
	key := newTestPrivateKey(t)

	body, err := json.Marshal(map[string]string{
		"repoId":     "uat-policies",
		"repoUrl":    "git@gitlab.example.com:infra/uat.git",
		"branch":     "main",
		"privateKey": key,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := postRepo(t, h, cookie, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("登记失败: %d %s", rec.Code, rec.Body.String())
	}
	// 查密钥**正文**的一段，不查 "PRIVATE KEY" 这几个字 —— 拒绝时的提示
	// 文案里本来就有它们，那样断言会把一句正确的提示当成一次泄漏。
	if body := keyBody(key); strings.Contains(rec.Body.String(), body) {
		t.Errorf("登记的响应里带着私钥:\n%s", rec.Body.String())
	}

	// 列表与详情同样不许带。
	for _, path := range []string{"/api/v1/git-repos"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if body := keyBody(key); strings.Contains(out.Body.String(), body) {
			t.Errorf("%s 的响应里带着私钥:\n%s", path, out.Body.String())
		}
	}

	// 但它确实被保管起来了 —— 否则这个特性什么都没做。
	if got := creds.get("uat-policies"); !bytes.Equal(got, []byte(key)) {
		t.Errorf("私钥没进保管处（拿到 %d 字节）", len(got))
	}
}

// 解析不了的私钥在登记那一刻就被拒，**不落库**。
//
// 存一段用不了的私钥，失败会推迟到写回那一刻，而那时报的是"仓库不可达"——
// 排查方向完全错。
func TestAnUnusablePrivateKeyIsRefusedBeforeItIsStored(t *testing.T) {
	// **这里不放形似真令牌的字面量。** 判定看的只是"这段东西解析不成私钥"，
	// 任何非 PEM 串都验得到同一条路径；而一个长得像真令牌的样例迟早会被
	// 照着换成一个真的（这个用例里就出现过一次，被 GitHub 的推送保护拦下）。
	for _, tc := range []struct{ name, key string }{
		{"公钥而不是私钥", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@example"},
		{"访问令牌", "deploy-token-REDACTED"},
		{"截断的 PEM", "-----BEGIN OPENSSH PRIVATE KEY-----\ndGVzdA==\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := newMemCredentials()
			h, _, cookie := newTestRouterWithCredentials(t, creds)
			body, _ := json.Marshal(map[string]string{
				"repoId": "uat-policies", "repoUrl": "git@gitlab.example.com:infra/uat.git",
				"branch": "main", "privateKey": tc.key,
			})
			rec := postRepo(t, h, cookie, string(body))
			if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "不是一把能用的") {
				t.Errorf("被接受了: %d %s", rec.Code, rec.Body.String())
			}
			if len(creds.kept) != 0 {
				t.Error("一把解析不了的私钥被存了起来")
			}
		})
	}
}

// 错误消息里不许出现私钥内容 —— 它会进日志、进浏览器控制台。
func TestRejectionDoesNotEchoTheKey(t *testing.T) {
	creds := newMemCredentials()
	h, _, cookie := newTestRouterWithCredentials(t, creds)
	const secret = "sk-this-should-never-be-echoed-back"
	body, _ := json.Marshal(map[string]string{
		"repoId": "uat-policies", "repoUrl": "git@gitlab.example.com:infra/uat.git",
		"branch": "main", "privateKey": secret,
	})
	rec := postRepo(t, h, cookie, string(body))
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("拒绝消息把私钥原样回显了:\n%s", rec.Body.String())
	}
}

// 这个部署不由平台保管凭据时，带私钥的请求要被**明确拒绝**，不能静默忽略。
//
// 忽略之后操作者以为钥匙配好了，而失败会推迟到写回那一刻。
func TestAKeyIsRefusedWhenThePlatformDoesNotKeepCredentials(t *testing.T) {
	h, _, cookie := newTestRouterWithCredentials(t, nil)
	key := newTestPrivateKey(t)
	body, _ := json.Marshal(map[string]string{
		"repoId": "uat-policies", "repoUrl": "git@gitlab.example.com:infra/uat.git",
		"branch": "main", "privateKey": key,
	})
	rec := postRepo(t, h, cookie, string(body))
	if !strings.Contains(rec.Body.String(), "没有把凭据交给平台保管") {
		t.Errorf("没有说清为什么不能在这里填私钥: %d %s", rec.Code, rec.Body.String())
	}
}

// keyBody 取私钥正文里的一段，用来判断"这段密钥有没有出现在响应里"。
//
// 取中间而不是取整段：整段里含 PEM 头尾，而头尾在提示文案里也会出现。
func keyBody(pemKey string) string {
	lines := strings.Split(strings.TrimSpace(pemKey), "\n")
	if len(lines) < 3 {
		return pemKey
	}
	return lines[len(lines)/2]
}

// testCredentialStore 是本包用例共用的凭据保管处，默认 nil。
//
// 用包级变量而不是给每个 buildTestRouter* 都加一个参数：那会让十几个与
// 凭据无关的调用点都多带一个 nil，而这件事只有这一组用例关心。
// 每个用例自己设、自己清（见 newTestRouterWithCredentials）。
var testCredentialStore CredentialStoreForTest

// CredentialStoreForTest 与 httpapi.CredentialStore 同形。
type CredentialStoreForTest interface {
	Put(ctx context.Context, ref string, plaintext []byte) error
	Delete(ctx context.Context, ref string) error
	Has(ctx context.Context, ref string) (bool, error)
}

// newTestRouterWithCredentials 装一个带凭据保管处的路由。
//
// creds 传 nil 表示这个部署不由平台保管凭据 —— 那是要被单独守住的一条分支。
func newTestRouterWithCredentials(
	t *testing.T, creds CredentialStoreForTest,
) (http.Handler, *auth.SessionStore, *http.Cookie) {
	t.Helper()
	prev := testCredentialStore
	testCredentialStore = creds
	t.Cleanup(func() { testCredentialStore = prev })
	return newTestRouterWithRegistry(t, fixtureReader(), newRegisteredRegistry())
}
