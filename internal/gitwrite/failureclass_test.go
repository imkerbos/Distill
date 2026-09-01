package gitwrite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// 分类要把人指到对的方向，而这几类该找的人不是同一个：认证失败去装公钥，
// 不可达去查网络，被远端拒绝去看保护分支与钩子。
func TestClassifyFailurePointsAtTheRightSubsystem(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want FailureClass
	}{
		{"公钥被拒（无哨兵，只有文本）", errors.New(
			"ssh: handshake failed: ssh: unable to authenticate, " +
				"attempted methods [none publickey], no supported methods remain"), FailureAuth},
		{"认证哨兵", transport.ErrAuthenticationRequired, FailureAuth},
		{"授权哨兵", transport.ErrAuthorizationFailed, FailureAuth},
		{"主机密钥对不上", errors.New(
			"ssh: handshake failed for git@h:2222/a.git: knownhosts key mismatch"), FailureHostKey},
		{"连不上", errors.New("dial tcp 10.0.0.1:2222: connect: connection refused"), FailureUnreachable},
		{"域名解析不了", errors.New("dial tcp: lookup gl.example: no such host"), FailureUnreachable},
		{"仓库不存在", transport.ErrRepositoryNotFound, FailureUnreachable},
		{"保护分支", errors.New("remote: GitLab: protected branch hook declined"), FailureRejected},
		{"钩子拒绝", errors.New("pre-receive hook declined"), FailureRejected},
		{"浅克隆被拒", errors.New("shallow update not allowed"), FailureRejected},
		{"超时", fmt.Errorf("push: %w", context.DeadlineExceeded), FailureTimeout},
		{"认不出", errors.New("something entirely new"), FailureUnknown},
		{"nil", nil, FailureUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.err); got != tc.want {
				t.Errorf("classifyFailure() = %q, want %q", got, tc.want)
			}
		})
	}
}

// **分类不得包含错误原文的任何一段。** 它存在的前提就是能安全地进日志，
// 而 SSH 的错误文本带主机、端口与 known_hosts 行号 —— 那是内网拓扑
// （TestWritebackPushErrorNeverLeaksTheTransportDetail 同时约束日志）。
func TestClassifyFailureNeverEchoesTheError(t *testing.T) {
	const raw = "ssh: handshake failed for git@gitlab.internal.example:2222/net/policies.git: " +
		"knownhosts key mismatch (offending line 3)"
	got := string(classifyFailure(errors.New(raw)))
	for _, leak := range []string{"gitlab.internal.example", "2222", "offending line", "policies.git"} {
		if strings.Contains(got, leak) {
			t.Errorf("分类里带出了 %q: %q", leak, got)
		}
	}
}

// Error() 自己 panic 时退回 UNKNOWN，不把一次推送失败变成一次崩溃。
// go-git 里确实有零值上调 Error() 会越界的类型（见 gitverify 那一处）。
func TestClassifyFailureSurvivesAPanickingError(t *testing.T) {
	if got := classifyFailure(panicError{}); got != FailureUnknown {
		t.Errorf("classifyFailure() = %q, want UNKNOWN", got)
	}
}

type panicError struct{}

func (panicError) Error() string { panic("boom") }
