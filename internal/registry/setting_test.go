package registry_test

import (
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// validSetting 返回一份全部字段合法的设置，供用例在此基础上改单个字段。
func validSetting() registry.PlatformSetting {
	return registry.PlatformSetting{
		SessionTTL:          8 * time.Hour,
		HTTPReadTimeout:     10 * time.Second,
		HTTPWriteTimeout:    20 * time.Second,
		HTTPShutdownTimeout: 15 * time.Second,
		SecretsBackend:      registry.SecretsBackendNone,
		GitVerifyTimeout:    10 * time.Second,
		GitVerifyHostKeys:   "example.com ssh-ed25519 AAAA...",
	}
}

// settingWith 构造一份除凭据后端与其字段外全部合法的设置，
// 让互斥性用例只改变这四个与本用例相关的值。
func settingWith(backend registry.SecretsBackend, project, prefix, dir string) registry.PlatformSetting {
	s := validSetting()
	s.SecretsBackend = backend
	s.SecretsProject = project
	s.SecretsPrefix = prefix
	s.SecretsDir = dir
	return s
}

// 后端与其字段必须互斥：选 DIR 却填了 project，说明操作者以为两套都生效。
// 这种配置最坏的落法是生产进程从本地目录读私钥——进程正常起来、校验正常
// 出结论，只是身份来源错了，没有任何症状会暴露它。
func TestSecretsBackendAndItsFieldsMustAgree(t *testing.T) {
	cases := []struct {
		name string
		s    registry.PlatformSetting
		ok   bool
	}{
		{"dir backend with dir", settingWith(registry.SecretsBackendDir, "", "", "/var/secrets"), true},
		{"dir backend with project", settingWith(registry.SecretsBackendDir, "p", "", "/var/secrets"), false},
		{"sm backend with both", settingWith(registry.SecretsBackendSecretManager, "p", "distill-", ""), true},
		{"sm backend without prefix", settingWith(registry.SecretsBackendSecretManager, "p", "", ""), false},
		{"sm backend with dir", settingWith(registry.SecretsBackendSecretManager, "p", "distill-", "/var/x"), false},
		{"none backend with nothing", settingWith(registry.SecretsBackendNone, "", "", ""), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := registry.ValidatePlatformSetting(c.s)
			if c.ok && err != nil {
				t.Errorf("ValidatePlatformSetting() = %v, want nil", err)
			}
			if !c.ok && !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("ValidatePlatformSetting() = %v, want ErrInvalid", err)
			}
		})
	}
}

// 未登记的后端取值必须被拒绝——这条独立于字段互斥性之外，
// 校验的是枚举本身封闭。
func TestValidatePlatformSettingRejectsUnregisteredBackend(t *testing.T) {
	s := settingWith("bogus", "", "", "")
	if err := registry.ValidatePlatformSetting(s); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("ValidatePlatformSetting() = %v, want ErrInvalid", err)
	}
}

// 超时为零或负值必须拒绝：gitverify.New 本来就拒绝非正数，
// 让它在保存设置时就失败，而不是等到下一次校验才炸。
func TestNonPositiveTimeoutsAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*registry.PlatformSetting)
	}{
		{"sessionTtl zero", func(s *registry.PlatformSetting) { s.SessionTTL = 0 }},
		{"sessionTtl negative", func(s *registry.PlatformSetting) { s.SessionTTL = -time.Second }},
		{"httpReadTimeout zero", func(s *registry.PlatformSetting) { s.HTTPReadTimeout = 0 }},
		{"httpWriteTimeout zero", func(s *registry.PlatformSetting) { s.HTTPWriteTimeout = 0 }},
		{"httpShutdownTimeout zero", func(s *registry.PlatformSetting) { s.HTTPShutdownTimeout = 0 }},
		{"gitVerifyTimeout zero", func(s *registry.PlatformSetting) { s.GitVerifyTimeout = 0 }},
		{"gitVerifyTimeout negative", func(s *registry.PlatformSetting) { s.GitVerifyTimeout = -time.Second }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSetting()
			c.mutate(&s)
			if err := registry.ValidatePlatformSetting(s); !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("ValidatePlatformSetting() = %v, want ErrInvalid", err)
			}
		})
	}
}

// 全部字段合法时必须放行——防止上面两组用例全靠误报凑出「always fail」
// 就能通过的空校验。
func TestValidatePlatformSettingAcceptsAWellFormedSetting(t *testing.T) {
	if err := registry.ValidatePlatformSetting(validSetting()); err != nil {
		t.Fatalf("ValidatePlatformSetting() = %v, want nil", err)
	}
}
