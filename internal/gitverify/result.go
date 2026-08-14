// Package gitverify 对策略仓库与集群绑定的策略路径做只读校验。
package gitverify

import (
	"context"
	"errors"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/imkerbos/Distill/internal/gitssh"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
)

// Classify 把校验过程中的错误映射成封闭枚举的结论。
//
// 单独摘成纯函数：网络调用留在它之外，映射本身才能被完整测试。
//
// 返回的是**仓库级**结论：这里认得出的每一种失败都是关于仓库的 ——
// 凭据、认证、可达性、分支。路径级只有三个取值，且 PATH_MISSING 只有
// 在 tree 查找那一步才谈得上，那一步不经过这里（design doc §3.3）。
//
// 返回值永远是 registry.RepoVerifyResult 的已登记取值，永远不含底层错误
// 文本。未知错误归入 REPO_UNREACHABLE，**不新增一个「其他」取值兜底**
// —— 那个取值会变成事实上的自由文本承载处，把仓库地址、用户名甚至
// 凭据片段带到 API 边界（spec §2.5、§3.2）。
func Classify(err error) registry.RepoVerifyResult {
	switch {
	case err == nil:
		return registry.RepoVerifyOK

	// 引用取不到是平台侧配置问题，与「取到了但仓库拒绝」处置人不同，
	// 不能合并（spec §2.4）。
	//
	// ErrInvalidRef 与 ErrNotFound 同归一类，且这不是凑数：绑定允许先
	// 记下来、凭据稍后再配（validateGit 明确放行空 credentialRef），于是
	// 空引用是一个常规状态，而它送进任何一个 Resolver 都是 ErrInvalidRef。
	// 落进 default 的话，一次连拨号都没有的失败会被报成「仓库不可达」，
	// 把操作者送去查网络，而实际要做的是补一个 credential_ref
	// （spec §3.2：这两类混起来会把排查引向错误的方向）。
	//
	// gitssh.ErrUnusableCredential 同归这一类：取到了内容但它不是一把可用的
	// 私钥，仍然是平台侧的凭据问题，不是仓库侧拒绝。
	case errors.Is(err, secrets.ErrNotFound),
		errors.Is(err, secrets.ErrInvalidRef),
		errors.Is(err, gitssh.ErrUnusableCredential):
		return registry.RepoVerifyCredentialUnresolved

	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return registry.RepoVerifyAuthFailed

	// 分支不存在有三种形态：直接解析引用得到 ErrReferenceNotFound；
	// 带 ReferenceName 的 clone 在远端没有该 ref 时得到的是
	// NoMatchingRefSpecError；空仓库则是 ErrEmptyRemoteRepository ——
	// 仓库是到得了的，只是任何分支都不存在。只认第一种会把「分支写错」
	// 误报成仓库不可达，而这两件事该找的人不是同一个。
	case errors.Is(err, plumbing.ErrReferenceNotFound),
		errors.Is(err, git.NoMatchingRefSpecError{}),
		errors.Is(err, transport.ErrEmptyRemoteRepository):
		return registry.RepoVerifyBranchMissing

	// 与 default 同值，但显式列出：这三类是已知的可达性失败，default 是
	// 未知错误的兜底。将来若改动 default 的去向，不应连带改变这三类。
	// 超时按 spec §4 归入 REPO_UNREACHABLE，不单列取值。
	//
	// gitssh.ErrBlockedDestination 也归这里，且不新增取值：从操作者视角这是一个
	// 「地址填错了、平台没去到仓库」的配置问题，与 RepoVerifyRepoUnreachable
	// 的定义（网络或地址问题，未能到达仓库）完全吻合。单列一个取值既要
	// 同步前端文案映射与统计口径，又等于把「这个地址是内网」这件事告诉
	// 调用方 —— 那正是 §19 不许外泄的内部网络信息。
	case errors.Is(err, transport.ErrRepositoryNotFound),
		errors.Is(err, gitssh.ErrBlockedDestination),
		errors.Is(err, context.DeadlineExceeded):
		return registry.RepoVerifyRepoUnreachable

	default:
		return registry.RepoVerifyRepoUnreachable
	}
}
