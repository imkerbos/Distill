package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/config"
)

// sampleHostKey 是一条 known_hosts 记录。公钥不是机密，配置里的 host key
// 本就与凭据分开存放（spec §2.2）。
const sampleHostKey = "gitlab.example.com ssh-ed25519 " +
	"AAAAC3NzaC1lZDI1NTE5AAAAIKyjWioKIYrbPTzY9F8JKIElwSThZ4xuqtqQPGo9tDIg"

// 未配置 secrets 时必须返回一个**真正的 nil 接口**。
//
// 返回 (*gitverify.Verifier)(nil) 一样能通过编译，但 httpapi 用
// `d.GitVerifier == nil` 判断"没有校验这回事"，包着 nil 指针的接口值会让
// 那个判断为假 —— 随后每次校验都在空指针上调方法，而这条路径挂在操作者
// 的保存动作上。这条断言就是那个区分。
func TestNewGitVerifierIsNilWhenSecretsAreNotConfigured(t *testing.T) {
	v, err := newGitVerifier(&config.Config{})
	if err != nil {
		t.Fatalf("newGitVerifier() error = %v", err)
	}
	if v != nil {
		t.Errorf("verifier = %#v, want an untyped nil interface", v)
	}
}

func TestNewGitVerifierBuildsAVerifierWhenConfigured(t *testing.T) {
	cfg := &config.Config{
		Secrets:   config.SecretsConfig{Dir: t.TempDir()},
		GitVerify: config.GitVerifyConfig{Timeout: 10 * time.Second, HostKeys: sampleHostKey},
	}
	v, err := newGitVerifier(cfg)
	if err != nil {
		t.Fatalf("newGitVerifier() error = %v", err)
	}
	if v == nil {
		t.Error("verifier = nil, want a configured verifier")
	}
}

// 超时必须由装配方显式传下去。
//
// gitverify.New 拒绝非正超时而不是取一个默认值；如果这里漏传（或传了
// 零值），启动必须失败。少了这条，一次装配疏忽的表现是"每次校验都用一个
// 谁也说不出来的超时"，而那正是本项目最难在事后复盘的一类问题。
func TestNewGitVerifierRefusesAMissingTimeout(t *testing.T) {
	cfg := &config.Config{
		Secrets:   config.SecretsConfig{Dir: t.TempDir()},
		GitVerify: config.GitVerifyConfig{HostKeys: sampleHostKey},
	}
	if _, err := newGitVerifier(cfg); err == nil {
		t.Fatal("newGitVerifier() = nil error, want a startup failure for a missing timeout")
	}
}

// 没有 host key 就没有 SSH 校验，而回退到不校验等于接受任意中间人。
func TestNewGitVerifierRefusesMissingHostKeys(t *testing.T) {
	cfg := &config.Config{
		Secrets:   config.SecretsConfig{Dir: t.TempDir()},
		GitVerify: config.GitVerifyConfig{Timeout: 10 * time.Second},
	}
	if _, err := newGitVerifier(cfg); err == nil {
		t.Fatal("newGitVerifier() = nil error, want a startup failure for missing host keys")
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	newHealthHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got["code"] != float64(0) {
		t.Errorf("code = %v, want 0", got["code"])
	}
}
