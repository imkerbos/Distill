package settings_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/settings"
)

// mutableSource 是一个能在两次读取之间改变返回值的 settings.Source。
//
// 这正是「按需读取」与「启动快照」唯一分得开的地方：两种实现对**一次**
// 调用给出的答案完全一样，差别只在第二次。
type mutableSource struct {
	current registry.PlatformSetting
	err     error
	// calls 记录 Setting 被调用的次数。
	calls int
}

func (m *mutableSource) Setting(context.Context) (registry.PlatformSetting, error) {
	m.calls++
	if m.err != nil {
		return registry.PlatformSetting{}, m.err
	}
	return m.current, nil
}

// validSetting 返回一份能过 ValidatePlatformSetting 的设置。
func validSetting() registry.PlatformSetting {
	return registry.PlatformSetting{
		SessionTTL:          8 * time.Hour,
		HTTPReadTimeout:     10 * time.Second,
		HTTPWriteTimeout:    20 * time.Second,
		HTTPShutdownTimeout: 15 * time.Second,
		SecretsBackend:      registry.SecretsBackendNone,
		GitVerifyTimeout:    10 * time.Second,
		GitWriteTimeout:     60 * time.Second,
	}
}

// 设置必须按需读取。做成启动快照的后果轮 2 已经演过一次：
// 集群下线后接口仍照旧服务它，直到进程重启。设置更甚 —— 改了 host key
// 却要重启才生效，等于设置页没有存在的意义（design doc §1.1）。
func TestProviderReadsOnEveryCall(t *testing.T) {
	src := &mutableSource{current: validSetting()}
	p := settings.New(src)

	first, err := p.Current(t.Context())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if first.GitVerifyTimeout != 10*time.Second {
		t.Fatalf("GitVerifyTimeout = %v, want the 10s the source currently holds", first.GitVerifyTimeout)
	}

	// 操作者在设置页改了值。
	next := validSetting()
	next.GitVerifyTimeout = 3 * time.Second
	next.SecretsBackend = registry.SecretsBackendDir
	next.SecretsDir = "/run/secrets/distill"
	src.current = next

	second, err := p.Current(t.Context())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if second.GitVerifyTimeout != 3*time.Second {
		t.Errorf("GitVerifyTimeout = %v after the setting changed, want 3s — Current must re-read, not replay a cached first answer",
			second.GitVerifyTimeout)
	}
	if second.SecretsBackend != registry.SecretsBackendDir {
		t.Errorf("SecretsBackend = %q after the setting changed, want DIR", second.SecretsBackend)
	}
	if src.calls != 2 {
		t.Errorf("source was read %d times for 2 calls, want 2 — a cached provider reads once", src.calls)
	}
}

// 读不到设置就是读不到，不得退化成一份编出来的默认值。
//
// 零值超时不是「用默认」而是关掉超时保护，零值 TTL 是会话立即过期。
func TestProviderPropagatesTheSourceError(t *testing.T) {
	want := errors.New("registry is down")
	p := settings.New(&mutableSource{err: want})

	if _, err := p.Current(t.Context()); !errors.Is(err, want) {
		t.Fatalf("Current() error = %v, want it to wrap %v", err, want)
	}
}

// 库里的行未必经过 UpdateSetting 的校验（迁移种子、手工 SQL 都绕得开）。
// 用不了的设置必须以错误的形态到达使用处，而不是安静地生效。
func TestProviderRejectsAnUnusableSetting(t *testing.T) {
	broken := validSetting()
	broken.SessionTTL = 0

	p := settings.New(&mutableSource{current: broken})
	if _, err := p.Current(t.Context()); err == nil {
		t.Fatal("Current() = nil error for a setting with a zero session TTL, want the caller to see it")
	}
}
