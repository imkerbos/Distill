package gitverify_test

import (
	"errors"
	"testing"

	"github.com/imkerbos/Distill/internal/gitverify"
	"github.com/imkerbos/Distill/internal/registry"
)

// 公钥被拒要报 AUTH_FAILED，不是 REPO_UNREACHABLE。
//
// go-git 的 SSH 传输不把它翻成 transport.ErrAuthorizationFailed，
// 于是它一直落进 default 兜底。后果是运维照着"仓库不可达"去查网络，
// 而实际要做的是把公钥装到仓库上——这两件事该找的人不是同一个。
//
// UAT 实测：deploy key 没装，git ls-remote 明确报
// "Permission denied (publickey)"，而平台报的是仓库不可达。
func TestPublicKeyRejectionIsAnAuthFailure(t *testing.T) {
	for _, msg := range []string{
		"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain",
		"unable to authenticate, attempted methods [none publickey]",
		"ssh: handshake failed: ssh: no supported methods remain",
	} {
		if got := gitverify.Classify(errors.New(msg)); got != registry.RepoVerifyAuthFailed {
			t.Errorf("Classify(%.40q…) = %s, want AUTH_FAILED —— "+
				"报成不可达会把人送去查网络，而要做的是装公钥", msg, got)
		}
	}
}

// 别的错误不许被误判成认证失败：判据放宽的方向必须是退回 default，
// 而不是把网络问题说成权限问题。
func TestOtherFailuresAreNotCalledAuthFailures(t *testing.T) {
	for _, msg := range []string{
		"dial tcp 10.0.0.1:22: i/o timeout",
		"no such host",
		"repository not found",
	} {
		if got := gitverify.Classify(errors.New(msg)); got == registry.RepoVerifyAuthFailed {
			t.Errorf("Classify(%q) = AUTH_FAILED —— 这不是一次认证失败", msg)
		}
	}
}

// panickingError 的 Error() 会 panic —— go-git v5.19.2 的
// NoMatchingRefSpecError 零值就是这个形状（refspec.go:54 对空 RefSpec
// 取下标）。
type panickingError struct{}

func (panickingError) Error() string {
	var empty []string
	//nolint:gosec // G602: 越界正是这里要复现的行为——go-git 那处就是这么 panic 的。
	return empty[0]
}

// **一个第三方类型的 Error() 不该能把一次校验变成一次 500。**
//
// 分类器跑在请求路径上，而它会对未知错误调 Error() 做文本匹配。
// 第一版没有挡这一层，一次「分支不存在」（NoMatchingRefSpecError）
// 直接把 Classify 打 panic 了。
func TestClassifySurvivesAPanickingError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Classify 被一个会 panic 的 Error() 打挂了: %v", r)
		}
	}()
	if got := gitverify.Classify(panickingError{}); got != registry.RepoVerifyRepoUnreachable {
		t.Errorf("Classify() = %s, want REPO_UNREACHABLE（退回 default）", got)
	}
}
