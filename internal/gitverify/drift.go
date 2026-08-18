package gitverify

import (
	"context"
	"errors"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/imkerbos/Distill/internal/registry"
)

// Drift 报告一个绑定写进去的那份策略现在还在不在。
//
// **只读**：clone、读两棵树、比对。不写仓库、不改绑定、不动锚点
// （design doc 2026-08-18-drift-detection §4）。
//
// 结论也不落库：它是「此刻问一次」的答案，存下来就会有人读到一个过期的
// IN_SYNC。要留痕的是操作者据此做的动作（重推），而那条本来就有审计。
//
// 比对的是 policyPath 子树，不是分支 HEAD（§2）：别人在别的路径上提交是
// 常态，把它报成漂移会让这个信号每天都在响。
//
// **任何读不出来的情形一律 UNKNOWN，绝不落到 IN_SYNC。** 一次网络抖动读成
// 「一致」，操作者就以为下发的东西还在，而它可能早被人删了（安全规范 §49）。
func (v *Verifier) Drift(
	ctx context.Context, r registry.GitRepo, policyPath, lastWrittenCommit string,
) registry.DriftResult {
	if lastWrittenCommit == "" {
		// 没有锚点不是"够不到"，是"从来没推过"。两者的处置不同：前者去修
		// 连通性，后者去推一次。
		return registry.DriftNeverWritten
	}

	ctx, cancel := context.WithTimeout(ctx, v.transport.Timeout())
	defer cancel()

	repo, err := v.cloneForDrift(ctx, r)
	if err != nil {
		return registry.DriftUnknown
	}

	anchorTree, err := treeAtPath(repo, plumbing.NewHash(lastWrittenCommit), policyPath)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			// 锚点提交不在这个仓库里了 —— 有人改写过历史。
			return registry.DriftAnchorMissing
		}
		return registry.DriftUnknown
	}

	head, err := repo.Head()
	if err != nil {
		return registry.DriftUnknown
	}
	headTreeHash, err := treeAtPath(repo, head.Hash(), policyPath)
	if err != nil {
		return registry.DriftUnknown
	}

	if anchorTree == headTreeHash {
		return registry.DriftInSync
	}
	return registry.DriftDrifted
}

// cloneForDrift 克隆整段历史。
//
// 与 cloneBranch 只差深度：那一个是 Depth 1，够回答"路径在不在"，但读不到
// 锚点那个旧提交。**认证、出站守卫与超时走同一条路** —— 两条各自演化的克隆
// 路径意味着两套地址守卫，而漏掉守卫的那一条会成为一次 SSRF 的入口
// （design doc §5）。
func (v *Verifier) cloneForDrift(ctx context.Context, r registry.GitRepo) (*git.Repository, error) {
	auth, err := v.transport.Auth(ctx, r.CredentialRef)
	if err != nil {
		return nil, err
	}
	return git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
		URL:           r.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(r.Branch),
		SingleBranch:  true,
		// 深度不设：漂移要读锚点那个提交，而它可能在很多次提交之前。
		NoCheckout: true,
		Tags:       git.NoTags,
	})
}

// treeAtPath 返回某个提交下 policyPath 子树的哈希。
//
// 返回子树哈希而不是逐文件比对：Git 的 tree 哈希本身就是那棵子树全部内容的
// 指纹，两个相同的哈希意味着逐字节相同。自己走一遍文件列表只会多出一处
// 可能与 Git 不一致的判定。
//
// 路径不存在时返回零值哈希：它与"存在但为空"在 Git 里是两回事，而对本函数
// 的调用方是同一件事 —— 两边都取到零值就是一致，一边有一边没有就是漂移。
func treeAtPath(repo *git.Repository, commit plumbing.Hash, policyPath string) (plumbing.Hash, error) {
	c, err := repo.CommitObject(commit)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	root, err := c.Tree()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	sub, err := root.Tree(normalizePolicyPath(policyPath))
	if err != nil {
		if isMissingEntry(err) {
			return plumbing.ZeroHash, nil
		}
		return plumbing.ZeroHash, err
	}
	return sub.Hash, nil
}

// 编译期确认 object 包仍被使用（isMissingEntry 依赖它的哨兵）。
var _ = object.ErrEntryNotFound
