package main

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// 没指定集群就没有采集对象。默认采"某一个"集群是这里最坏的落法：
// 它会在操作者以为自己什么都没做的时候连上一个生产集群。
func TestRunRequiresACluster(t *testing.T) {
	if err := run("configs/demo.yaml", "", time.Minute, ingestOptions{}); err == nil {
		t.Fatal("run() = nil error without -cluster, want a refusal")
	}
}

// 显式给出的非正超时是一个写错了的配置，不是"不限时"。
// 静默换成默认值会让一份写错的编排文件看起来生效了（规范 §30）。
func TestRunRejectsANonPositiveTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if err := run("configs/demo.yaml", "prod", d, ingestOptions{}); err == nil {
			t.Fatalf("run() = nil error for -timeout=%v, want a refusal", d)
		}
	}
}

// 后端是 NONE 时返回一个**真正的 nil 接口**：调用方用 `resolver == nil`
// 判断"没有解析器"，包着 nil 指针的接口值会让那个判断为假，随后在空指针
// 上调 Resolve。
func TestNewSecretResolverReturnsNilForTheNoneBackend(t *testing.T) {
	r, err := newSecretResolver(t.Context(), registry.PlatformSetting{
		SecretsBackend: registry.SecretsBackendNone,
	})
	if err != nil {
		t.Fatalf("newSecretResolver() error = %v", err)
	}
	if r != nil {
		t.Errorf("newSecretResolver() = %v, want a nil interface for the NONE backend", r)
	}
}

func TestNewSecretResolverBuildsTheDirBackend(t *testing.T) {
	r, err := newSecretResolver(t.Context(), registry.PlatformSetting{
		SecretsBackend: registry.SecretsBackendDir,
		SecretsDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newSecretResolver() error = %v", err)
	}
	if r == nil {
		t.Fatal("newSecretResolver() = nil for the DIR backend, want a resolver")
	}
}

// 枚举加了取值却没加装配分支时必须失败。静默返回"没有解析器"会把它变成
// 一次悄悄关掉采集的上线。
func TestNewSecretResolverRejectsAnUnknownBackend(t *testing.T) {
	if _, err := newSecretResolver(t.Context(), registry.PlatformSetting{
		SecretsBackend: registry.SecretsBackend("VAULT"),
	}); err == nil {
		t.Fatal("newSecretResolver() = nil error for an unknown backend, want a refusal")
	}
}
