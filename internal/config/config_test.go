package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/config"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadReadsYAML(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
  read_timeout: 5s
  write_timeout: 10s
  shutdown_timeout: 15s
auth:
  session_ttl: 8h
  users:
    - username: demo
      password_hash: "$2a$10$abcdefghijklmnopqrstuv"
log:
  level: INFO
`)

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":10100" {
		t.Errorf("Addr = %q, want :10100", cfg.Server.Addr)
	}
	if cfg.Auth.SessionTTL != 8*time.Hour {
		t.Errorf("SessionTTL = %v, want 8h", cfg.Auth.SessionTTL)
	}
	if len(cfg.Auth.Users) != 1 || cfg.Auth.Users[0].Username != "demo" {
		t.Fatalf("Users = %+v, want one user named demo", cfg.Auth.Users)
	}
}

// 配置错误必须在启动时暴露，而不是等到第一个请求打进来才发现。
func TestLoadRejectsMissingAddr(t *testing.T) {
	p := writeYAML(t, "auth:\n  session_ttl: 8h\n")
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error for missing server.addr")
	}
}

func TestLoadRejectsNoUsers(t *testing.T) {
	p := writeYAML(t, "server:\n  addr: \":10100\"\nauth:\n  session_ttl: 8h\n")
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error when no users are configured")
	}
}

// 环境变量优先于文件，容器里才能不重建镜像改配置。
func TestEnvOverridesFile(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  session_ttl: 8h
  users:
    - username: demo
      password_hash: "x"
`)
	t.Setenv("DISTILL_SERVER__ADDR", ":19999")

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":19999" {
		t.Errorf("Addr = %q, want env override :19999", cfg.Server.Addr)
	}
}
