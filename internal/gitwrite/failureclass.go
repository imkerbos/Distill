package gitwrite

import (
	"context"
	"errors"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// FailureClass 是一次推送失败的粗分类，封闭枚举。
//
// **存在的理由是：今天一次失败之后没有任何人查得出原因，包括平台自己。**
// 推送失败走的是 writeGitWriteError 的 default 分支，那里只记 request_id 与
// 集群，错误本身一个字都不进日志 —— 而那是刻意的：SSH 的错误文本带主机、
// 端口与 known_hosts 行号，泄露它等于把内网拓扑写进日志（
// TestWritebackPushErrorNeverLeaksTheTransportDetail 同时约束响应体与日志）。
//
// 分类解开这个死结：取值是本文件里的固定字符串，不含对端返回的任何内容，
// 却足以把人指到对的方向 —— 认证失败去装公钥，不可达去查网络，被远端拒绝
// 去看仓库的保护分支与钩子。UAT 上一次 500 之后无从下手，正是缺这一层。
//
// 与 UnknownReason 同一条纪律：封闭枚举，不用自由文本，这样"失败构成"能被
// 聚合成一个看得见的指标。
type FailureClass string

const (
	// FailureAuth 表示认证被拒 —— 公钥没装上，或装的不是这一把。
	FailureAuth FailureClass = "AUTH"
	// FailureHostKey 表示主机密钥对不上，可能是中间人，也可能是对端换了机器。
	FailureHostKey FailureClass = "HOST_KEY"
	// FailureUnreachable 表示连不上：网络、DNS、端口、出站守卫。
	FailureUnreachable FailureClass = "UNREACHABLE"
	// FailureRejected 表示连上了、认证过了，远端拒绝这次写入
	// （保护分支、pre-receive 钩子、非快进）。
	FailureRejected FailureClass = "REJECTED"
	// FailureTimeout 表示这次操作超时。
	FailureTimeout FailureClass = "TIMEOUT"
	// FailureUnknown 表示认不出来。
	//
	// **不并进任何一类。** 认不出就说认不出，比塞进一个看起来具体的分类
	// 好：后者会把人指去查一件没发生的事。它出现得多，说明这张表该加一条。
	FailureUnknown FailureClass = "UNKNOWN"
)

// classifyFailure 把一个推送错误归进上面的枚举。
//
// 先比哨兵，再比文本 —— 与 gitverify.isSSHAuthFailure 同一条理由：go-git 的
// SSH 传输在多数失败上不返回可 errors.Is 的哨兵，只有一个普通 error。匹配的
// 是 x/crypto/ssh 与 go-git 自己的固定英文，不是对端返回的内容，因此不随
// 服务端实现变；对不上时退回 UNKNOWN，也就是退回今天的信息量。
//
// 返回值不含 err 的任何文本。
func classifyFailure(err error) (class FailureClass) {
	if err == nil {
		return FailureUnknown
	}
	// 挡住 Error() 自己 panic，同 gitverify 那一处：go-git 有零值上调
	// Error() 会越界的类型，而这段跑在请求路径上。
	defer func() {
		if recover() != nil {
			class = FailureUnknown
		}
	}()

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return FailureAuth
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return FailureUnreachable
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "knownhosts") || strings.Contains(msg, "key mismatch"):
		return FailureHostKey
	case strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain"):
		return FailureAuth
	case strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "i/o timeout"):
		return FailureUnreachable
	case strings.Contains(msg, "pre-receive hook declined") ||
		strings.Contains(msg, "protected branch") ||
		strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "remote rejected") ||
		strings.Contains(msg, "shallow update not allowed"):
		return FailureRejected
	}
	return FailureUnknown
}

// ClassifyFailure 是 classifyFailure 的导出形式，供边缘层记日志。
func ClassifyFailure(err error) FailureClass { return classifyFailure(err) }
