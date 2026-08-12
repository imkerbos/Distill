// Package gitverify 对集群的 Git 绑定做只读校验。
package gitverify

import (
	"context"
	"errors"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
)

// Classify 把校验过程中的错误映射成封闭枚举的结论。
//
// 单独摘成纯函数：网络调用留在它之外，映射本身才能被完整测试。
//
// 返回值永远是 registry.VerifyResult 的已登记取值，永远不含底层错误
// 文本。未知错误归入 REPO_UNREACHABLE，**不新增一个「其他」取值兜底**
// —— 那个取值会变成事实上的自由文本承载处，把仓库地址、用户名甚至
// 凭据片段带到 API 边界（spec §2.5、§3.2）。
func Classify(err error) registry.VerifyResult {
	switch {
	case err == nil:
		return registry.VerifyOK

	// 引用取不到是平台侧配置问题，与「取到了但仓库拒绝」处置人不同，
	// 不能合并（spec §2.4）。
	case errors.Is(err, secrets.ErrNotFound):
		return registry.VerifyCredentialUnresolved

	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return registry.VerifyAuthFailed

	// 分支不存在有三种形态：直接解析引用得到 ErrReferenceNotFound；
	// 带 ReferenceName 的 clone 在远端没有该 ref 时得到的是
	// NoMatchingRefSpecError；空仓库则是 ErrEmptyRemoteRepository ——
	// 仓库是到得了的，只是任何分支都不存在。只认第一种会把「分支写错」
	// 误报成仓库不可达，而这两件事该找的人不是同一个。
	case errors.Is(err, plumbing.ErrReferenceNotFound),
		errors.Is(err, git.NoMatchingRefSpecError{}),
		errors.Is(err, transport.ErrEmptyRemoteRepository):
		return registry.VerifyBranchMissing

	// 与 default 同值，但显式列出：这两类是已知的可达性失败，default 是
	// 未知错误的兜底。将来若改动 default 的去向，不应连带改变这两类。
	// 超时按 spec §4 归入 REPO_UNREACHABLE，不单列取值。
	case errors.Is(err, transport.ErrRepositoryNotFound),
		errors.Is(err, context.DeadlineExceeded):
		return registry.VerifyRepoUnreachable

	default:
		return registry.VerifyRepoUnreachable
	}
}
