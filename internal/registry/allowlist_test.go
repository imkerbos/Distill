package registry_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

// 内网网段可以放行 —— 这是这一栏存在的理由。
func TestAllowedDestinationsAcceptsPrivateRanges(t *testing.T) {
	got, err := registry.ParseAllowedDestinations(
		"10.170.0.0/16\n# UAT 的 GitLab\n192.168.1.0/24\n\nfd00::/8\n")
	if err != nil {
		t.Fatalf("合法的内网网段被拒: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("解析出 %d 条，want 3（空行与注释要跳过）: %v", len(got), got)
	}
}

// **等于关掉检查的填法必须被拒。**
//
// 这一栏挡的是"平台被诱导去连任意地址"。能放行整个互联网的清单，
// 与没有这道检查是同一件事，而它看起来还像配置正确。
func TestAllowedDestinationsRefusesAnythingThatDisablesTheCheck(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"整个 IPv4", "0.0.0.0/0"},
		{"整个 IPv6", "::/0"},
		{"公网段", "8.8.8.0/24"},
		{"Azure 元数据", "168.63.129.16/32"},
		{"阿里云元数据所在段", "100.64.0.0/10"},
		{"链路本地（含云元数据 169.254.169.254）", "169.254.0.0/16"},
		{"回环", "127.0.0.0/8"},
		{"跨出私有段的写法", "10.0.0.0/7"},
		{"不是 CIDR", "10.170.1.11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := registry.ParseAllowedDestinations(tc.raw); err == nil {
				t.Errorf("%q 被接受了 —— 这一条等于关掉这道检查", tc.raw)
			}
		})
	}
}

// 空清单合法，表示"只放行公网"，也就是引入这一栏之前的行为。
func TestAnEmptyAllowlistIsValid(t *testing.T) {
	got, err := registry.ParseAllowedDestinations("")
	if err != nil || len(got) != 0 {
		t.Errorf("空清单应当合法且为空: %v %v", got, err)
	}
}

// 非法网段在**保存设置**时就被拒，不留到拨号那一刻。
//
// 留到拨号的症状是 REPO_UNREACHABLE —— 一句说不出真正原因的话。
func TestSettingValidationRejectsABadAllowlist(t *testing.T) {
	s := registry.PlatformSetting{
		SessionTTL: 1, HTTPReadTimeout: 1, HTTPWriteTimeout: 1,
		HTTPShutdownTimeout: 1, GitVerifyTimeout: 1,
		SecretsBackend:         registry.SecretsBackendNone,
		GitAllowedDestinations: "0.0.0.0/0",
	}
	err := registry.ValidatePlatformSetting(s)
	if err == nil {
		t.Fatal("保存时没有拦下 0.0.0.0/0")
	}
	if !strings.Contains(err.Error(), "私有地址空间") {
		t.Errorf("拒绝理由说不清为什么: %v", err)
	}
}
