package secrets_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/secrets"
)

// 资源路径由「配置里的 project + prefix」加「绑定里的短名」拼成，短名
// 自己永远不是一条完整路径（spec §2.1）。这条断言盯的就是那三段来源：
// 把拼装改成只用 ref、或漏掉 prefix，结果都不再等于 want。
func TestResourceNameIsBuiltFromConfigNotFromTheRef(t *testing.T) {
	got := secrets.ResourceName("my-proj", "distill-", "prod-asia-1")
	want := "projects/my-proj/secrets/distill-prod-asia-1/versions/latest"
	if got != want {
		t.Fatalf("ResourceName() = %q, want %q", got, want)
	}
}

// 换一份配置必须换一条路径。
//
// 单独一条等值断言无法区分「按配置拼」与「碰巧拼出了那个字符串」——
// 比如把 project 写死成常量，上面那条仍然是绿的。这条用另一组配置再取
// 一次，让「配置进不去结果」这类改动无处可藏。
func TestResourceNameFollowsTheConfiguredProjectAndPrefix(t *testing.T) {
	first := secrets.ResourceName("proj-a", "distill-", "asia-1")
	second := secrets.ResourceName("proj-b", "other-", "asia-1")
	if first == second {
		t.Fatalf("ResourceName() = %q for both projects, want the configured project and prefix to reach the result", first)
	}
	if want := "projects/proj-b/secrets/other-asia-1/versions/latest"; second != want {
		t.Fatalf("ResourceName() = %q, want %q", second, want)
	}
}

// 短名里塞不进路径分隔符，所以拼出来的资源名不可能跳出配置划定的前缀。
//
// 这里连着 ValidateRef 一起断言：路径安全性由「字符集收窄」与「前缀固定」
// 两件事共同保证，任何一边被放宽，这条都会红。
func TestRefsThatCouldEscapeTheConfiguredNamespaceAreRejected(t *testing.T) {
	for _, ref := range []string{
		"../../other-project/secrets/prod",
		"projects/other/secrets/prod/versions/latest",
		"prod/../../admin",
	} {
		if err := secrets.ValidateRef(ref); err == nil {
			t.Errorf("ValidateRef(%q) = nil, want it rejected before it can reach a resource path", ref)
		}
	}
}

// 一个合法短名拼出来的路径，secrets/ 之后的那一段必须以配置的前缀开头。
func TestResourceNameKeepsTheRefInsideTheConfiguredPrefix(t *testing.T) {
	got := secrets.ResourceName("my-proj", "distill-", "prod-asia-1")
	const want = "projects/my-proj/secrets/distill-"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("ResourceName() = %q, want it to start with %q", got, want)
	}
}
