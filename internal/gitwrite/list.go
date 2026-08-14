package gitwrite

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/imkerbos/Distill/internal/registry"
)

// RepoListing 是策略仓库当前状态的只读快照：写回计划里那两份「平台不会碰、
// 交人工处置」的清单（design doc 2026-08-14 §2、§3）就由它填。
//
// 两份清单一次拿齐、而不是分两个方法：它们出自同一次出站（一次浅克隆加一次
// ls-remote），拆开就是两次拨号，而计划要的是同一时刻的那张快照。
type RepoListing struct {
	// Files 是部署分支上 policyPath 下已有的文件，仓库根起算，按字典序。
	//
	// 只列文件、不列目录：交人工处置的对象是文件，而一个目录名在写回报告里
	// 读起来像一个可以删的东西。
	Files []string
	// Branches 是远端现存的 distill/* 分支，按字典序。
	//
	// 报的是「存在」，不是「未合并」：判断合并与否要在部署分支的历史里找它的
	// tip，而这里只有一个 Depth 1 的浅克隆，找不到；拉全量历史换这一个判断，
	// 成本随仓库历史无上限增长（§12）。攒着几条分支这个信号仍然给得出，
	// 而调用方必须照实说明合并状态没有被判断过。
	Branches []string
}

// List 只读地枚举策略仓库：policyPath 下已有的文件，与现存的 distill/* 分支。
//
// **本方法只读，任何时候都不推送**，与 gitverify 的只读契约同义：克隆不检出
// 工作区、不建任何引用、不发起 receive-pack。它挂在 Writer 上是因为出站的守卫
// 只能有一处（见包注释与 gitssh）—— 共用传输不等于共用能力。
//
// 出站同样只经由 w.transport.Auth：这里若内联构造 gogitssh.NewPublicKeys，
// 钉死的 host key 与目的地址判定一起消失，而 file:// 的测试一个都不会变红。
//
// 失败一律上抛、不就地吞掉：一份「枚举失败所以清单为空」的计划，在界面上
// 读起来是「仓库里没有多余文件」，那是一句没有人算过的断言（§4）。
func (w *Writer) List(
	ctx context.Context, repo registry.GitRepo, policyPath string,
) (RepoListing, error) {
	ctx, cancel := context.WithTimeout(ctx, w.transport.Timeout())
	defer cancel()

	auth, err := w.transport.Auth(ctx, repo.CredentialRef)
	if err != nil {
		return RepoListing{}, err
	}

	local, err := w.cloneForListing(ctx, repo, auth)
	if err != nil {
		return RepoListing{}, err
	}

	files, err := filesUnder(local, policyPath)
	if err != nil {
		return RepoListing{}, err
	}
	branches, err := distillBranches(ctx, local, auth)
	if err != nil {
		return RepoListing{}, err
	}
	return RepoListing{Files: files, Branches: branches}, nil
}

// cloneForListing 把部署分支克隆进内存，**不检出工作区**。
//
// 与 cloneDeployBranch 分开一份而不是共用：那一份必须检出，因为「只新增与
// 更新、从不删除」赖的正是检出把已有条目放进索引；而枚举只读树，检出对它是
// 一次没有用处的写。两处各自说明自己为什么这样克隆，比一个带布尔开关的
// 共用函数更难改错 —— 改错的方向是让写路径不检出，那等于一次性删光整个目录。
func (w *Writer) cloneForListing(
	ctx context.Context,
	repo registry.GitRepo,
	auth *gogitssh.PublicKeys,
) (*git.Repository, error) {
	return git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
		URL:           repo.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(repo.Branch),
		SingleBranch:  true,
		Depth:         1,
		NoCheckout:    true,
		Tags:          git.NoTags,
	})
}

// filesUnder 列出部署分支上 policyPath 之下的全部文件。
//
// 从 policyPath 那棵子树往下走，而不是遍历整个仓库再按前缀筛：前缀比对会把
// "clusters/prod-asia-1-old/api.yaml" 当成 "clusters/prod-asia-1" 下的文件
// （规范 §15 的同一形状），而按树走的边界天然落在路径段上。
//
// policyPath 不存在时返回空清单、不报错：绑定校验会把它报成 PATH_MISSING，
// 而在这里它只意味着「这次写回将会新建这个目录」，没有多余文件可报。
func filesUnder(repo *git.Repository, policyPath string) ([]string, error) {
	root := strings.Trim(policyPath, "/")
	if root == "" {
		// 空 policyPath 会把边界拉回仓库根，整个仓库都会被列成「多余文件」。
		// registry.writebackRoot 已经拒过它，这里不接着往下走。
		return nil, errors.New("gitwrite: empty policy path")
	}

	head, err := repo.Head()
	if err != nil {
		return nil, err
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	sub, err := tree.Tree(root)
	if err != nil {
		if isMissingEntry(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []string
	if err := sub.Files().ForEach(func(f *object.File) error {
		files = append(files, root+"/"+f.Name)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// distillBranches 列出远端现存的 distill/* 分支。
//
// 与 checkTargetAbsent 一样走 remote.ListContext 并复用同一个 auth：这也是
// 一次出站，同样必须经过钉死的 host key 与目的地址判定。
//
// 只认分支引用：tag 与 refs/distill/* 之类的引用不是这条流程建出来的东西，
// 把它们混进来会让「攒了几条没人合的分支」这个信号读起来是假的。
func distillBranches(
	ctx context.Context,
	local *git.Repository,
	auth *gogitssh.PublicKeys,
) ([]string, error) {
	remote, err := local.Remote(git.DefaultRemoteName)
	if err != nil {
		return nil, err
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return nil, err
	}

	var branches []string
	for _, ref := range refs {
		if !ref.Name().IsBranch() {
			continue
		}
		if name := ref.Name().Short(); strings.HasPrefix(name, branchPrefix) {
			branches = append(branches, name)
		}
	}
	sort.Strings(branches)
	return branches, nil
}

// isMissingEntry 判断 tree 查找的错误是否表示「这条路径不存在」。
//
// 与 gitverify.isMissingEntry 同一组错误、同一条理由：这三个错误只有在 tree
// 查找这一步才意味着路径缺失，放进通用判断会把别处一次真正的失败读成「没有」。
func isMissingEntry(err error) bool {
	return errors.Is(err, object.ErrEntryNotFound) ||
		errors.Is(err, object.ErrDirectoryNotFound) ||
		errors.Is(err, plumbing.ErrObjectNotFound)
}
