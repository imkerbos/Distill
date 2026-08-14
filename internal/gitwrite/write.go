// Package gitwrite 把一份已确认的写回计划推进策略仓库的一条**新分支**。
//
// 这是平台第一条往自身之外写的路径（design doc 2026-08-14 §1）。此前所有
// 出站都是只读的。四条硬规则各自对应一种具体伤害，逐条落在代码上：
//
//  1. **永不推送绑定里配置的那条分支。** 那条分支是 Config Sync 应用的
//     对象，推它等于界面上点一下就是一次生产变更，中间没有人（§2）。
//  2. **永不强制推送。** 分支名带 UTC 时间戳，天然不冲突；真冲突了说明
//     某个假设破了，而假设破了时诚实的反应是失败，不是覆盖（§2）。
//  3. **只新增与更新，永不删除。** 删掉一个策略文件等于把一片命名空间从
//     「有规则」变成「默认拒绝」，是这个平台能造成的最大伤害；而判断一个
//     文件多余需要知道集群现在真实跑着什么，平台今天看不到（§3）。
//  4. **内容逐字节相同时什么都不做。** 不建分支、不提交、不推送 —— 空提交
//     会把「平台真的改过什么」埋进噪声里（§6）。
//
// **出站只经由 gitssh.Transport。** 认证、钉死的 host key 与目的地址判定
// 只有那一份实现，本包不自建认证。写路径尤其不得另开一条：它行使的是
// deploy key 的**写**权限，那是至今完全没被行使过的一段（§10）。
package gitwrite

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/imkerbos/Distill/internal/gitssh"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
)

// platformName 与 platformEmail 是提交的作者身份。
//
// 用平台身份、不冒充操作者（design doc §7）：冒充会让 Git 的作者字段失去
// 意义 —— 一次由平台生成的提交挂着某个人的名字，事后没人分得清哪些提交是
// 他自己写的。操作者记在提交信息与审计里。
//
// .invalid 是 RFC 2606 保留后缀，永远解析不到真实邮箱，因此这个地址不会
// 意外指向某个真人，也收不到任何东西。
const (
	platformName  = "distill"
	platformEmail = "distill@platform.invalid"
)

// Writer 把写回计划推进策略仓库。
//
// 它**不持有私钥** —— 私钥每次推送时由 transport 现取，只在该次调用栈里
// 存在（design doc 2026-08-13 spec §2.5）。仓库内容同样不落盘：克隆走内存
// 存储与内存文件系统。
type Writer struct {
	transport *gitssh.Transport
}

// New 构造一个 Writer。
//
// 参数原样交给 gitssh.New，构造期的拒绝（解析器为空、超时非正、没有一把
// 可用的 host key）都在那里做，与 gitverify.New 是同一条链路的同一道门。
// **没有 host key 就构造失败**，不存在「未配置就不校验」的分支。
func New(r secrets.Resolver, hostKeys []byte, timeout time.Duration) (*Writer, error) {
	t, err := gitssh.New(r, hostKeys, timeout)
	if err != nil {
		return nil, err
	}
	return &Writer{transport: t}, nil
}

// Push 把计划推到一条新分支上，返回平台推上去的那个 commit SHA。
//
// 返回的 SHA 是**平台交出去的那个提交**，不是合并后的（design doc §8）：
// 合并由人做，平台不知道也不该假装知道。
//
// 内容与目标分支上现有内容逐字节相同时返回 ErrNothingToPush，且此前不建
// 任何分支、不产生任何提交、不发起推送（§6）。
//
// plan 必须由 registry.NewWritebackPlan 构造 —— 那里做了「每条路径都落在
// 绑定的 policyPath 之内」的判断，而本函数拿不到绑定，复核不了。这不是一句
// 约定：一份绕过构造函数的计划能写出 policyPath 之外，等于让平台改写仓库里
// 任意文件。
func (w *Writer) Push(ctx context.Context, repo registry.GitRepo, plan registry.WritebackPlan) (string, error) {
	target, err := targetRef(repo, plan)
	if err != nil {
		return "", err
	}
	// 计划批准的是哪个仓库，就只能写到哪个仓库（design doc §4）。空的
	// RepoID 一律拒绝：一份没说清落点的计划，它的指纹里也就没有落点这一维。
	if plan.RepoID == "" || plan.RepoID != repo.ID {
		return "", ErrPlanRepoMismatch
	}
	if len(plan.Files) == 0 {
		// 一份不写任何文件的计划推出去只会是一个空提交（§6）。
		return "", ErrNothingToPush
	}
	if strings.TrimSpace(plan.CommitMessage) == "" {
		// 提交信息是永久历史的一部分，也是评审人唯一会读的那句话。没有它就
		// 没有可评审的依据 —— 失败方向必须是关，不是补一句平台自己编的话。
		return "", ErrNoCommitMessage
	}

	ctx, cancel := context.WithTimeout(ctx, w.transport.Timeout())
	defer cancel()

	// 认证只有这一条来路。**不得在这里内联构造 gogitssh.NewPublicKeys** ——
	// 那样会绕开钉死的 host key 与目的地址判定，而绕开之后走 file:// 的那些
	// 用例一个都不会变红（file:// 根本不建 SSH 认证）。读路径上出过一次这个
	// 缺陷，写路径行使的是写权限，重蹈的代价更大。
	//
	// 绑住它的是两条：TestPushAuthenticatesThroughTheGuardedTransport（认证
	// 对象出自 Transport.Auth）与 TestPushRefusesToConnectToAnInternalAddress
	// （回环地址上一次**真实的 SSH 握手**，证明目的地址判定跑在了真实拨号上）。
	auth, err := w.transport.Auth(ctx, repo.CredentialRef)
	if err != nil {
		return "", err
	}

	local, err := w.cloneDeployBranch(ctx, repo, auth)
	if err != nil {
		return "", err
	}
	tree, err := local.Worktree()
	if err != nil {
		return "", err
	}

	// 幂等判断放在建分支与查远端之前：内容没变时「什么都不做」必须是真的
	// 什么都没做，包括不多打一次远端。
	changed, err := changedFiles(tree, plan.Files)
	if err != nil {
		return "", err
	}
	if len(changed) == 0 {
		return "", ErrNothingToPush
	}

	if err := checkTargetAbsent(ctx, local, auth, target); err != nil {
		return "", err
	}

	if err := tree.Checkout(&git.CheckoutOptions{Branch: target, Create: true, Keep: true}); err != nil {
		return "", err
	}
	if err := writeFiles(tree, changed); err != nil {
		return "", err
	}

	now := time.Now().UTC()
	// 用计划里那段文字，逐字节照抄，本包不自己拼一句。它是指纹覆盖的东西，
	// 也就是操作者确认过的那一份；现场另拼一句，落进仓库历史的就会是一段
	// 没有人批准过的文字，而合并请求上的评审人正是读着它决定合不合。
	commit, err := tree.Commit(plan.CommitMessage, &git.CommitOptions{
		Author:    platformSignature(now),
		Committer: platformSignature(now),
	})
	if err != nil {
		return "", err
	}

	if err := local.PushContext(ctx, pushOptions(target, auth)); err != nil {
		return "", err
	}
	return commit.String(), nil
}

// cloneDeployBranch 把绑定里配置的那条分支克隆进内存，作为本次写回的基底。
//
// **必须检出工作区**（不像 gitverify 那样 NoCheckout）：检出把该分支上已有的
// 全部条目放进索引，提交才会带着它们 —— 这正是「只新增与更新、从不删除」
// （§3）赖以成立的机制。空索引上提交会产生一个只含本次文件的树，等于一次性
// 删掉该路径下其余全部策略文件。
//
// 存储与工作区都在内存里：仓库内容与私钥一样不落盘。
//
// 浅克隆（Depth 1）：新提交的父就是远端当前的 HEAD，推送时远端已经有它，
// 因此这次推送只需要送出新提交本身（design doc §12 的量级）。
func (w *Writer) cloneDeployBranch(
	ctx context.Context,
	repo registry.GitRepo,
	auth *gogitssh.PublicKeys,
) (*git.Repository, error) {
	return git.CloneContext(ctx, memory.NewStorage(), memfs.New(), &git.CloneOptions{
		URL:           repo.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(repo.Branch),
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	})
}

// changedFiles 挑出内容与基底分支不同的那些文件。
//
// 逐字节比较，不比时间戳也不比大小：§6 说的就是「逐字节相同」。全部相同时
// 返回空切片，调用方据此一步不走。
//
// 只返回变化的那些、而不是全部文件，是为了让索引里只出现真正改动过的条目 ——
// 一次写回的提交应当只包含它改的东西。
func changedFiles(tree *git.Worktree, files []registry.WritebackFile) ([]registry.WritebackFile, error) {
	changed := make([]registry.WritebackFile, 0, len(files))
	for _, f := range files {
		existing, found, err := readFile(tree, f.Path)
		if err != nil {
			return nil, err
		}
		if !found || existing != f.Content {
			changed = append(changed, f)
		}
	}
	return changed, nil
}

// readFile 读出工作区里的一个文件。第二个返回值为 false 表示它还不存在。
//
// 「不存在」与「读失败」分开：把读失败也当成不存在，会让一次读错误变成一次
// 无声的整份覆盖。
func readFile(tree *git.Worktree, p string) (string, bool, error) {
	f, err := tree.Filesystem.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer func() { _ = f.Close() }()

	b, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// writeFiles 把变化的文件写进工作区并逐条加进索引。
//
// **只 Add 计划里的路径**，不做 AddGlob、不做 AddWithOptions{All: true}：
// 仓库里其余条目留在检出时进索引的那份状态里，因而原样进入新提交（§3）。
// 一次 All 的暂存会把工作区里任何一处意外的缺失变成一次删除。
func writeFiles(tree *git.Worktree, files []registry.WritebackFile) error {
	for _, f := range files {
		if dir := path.Dir(f.Path); dir != "." {
			if err := tree.Filesystem.MkdirAll(dir, 0o750); err != nil {
				return err
			}
		}
		if err := writeFile(tree, f); err != nil {
			return err
		}
		if _, err := tree.Add(f.Path); err != nil {
			return err
		}
	}
	return nil
}

// writeFile 整份写入一个文件（已存在则截断重写）。
//
// 写的是渲染好的完整文件而不是补丁：写回只新增与更新整份文件（§3），而
// 一份「改哪几行」的描述要靠仓库当前内容才解释得了，那正是操作者在屏幕上
// 看不到的东西。
func writeFile(tree *git.Worktree, f registry.WritebackFile) (err error) {
	dst, err := tree.Filesystem.Create(f.Path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}()

	_, err = io.WriteString(dst, f.Content)
	return err
}

// platformSignature 是提交的作者与提交者身份。
func platformSignature(when time.Time) *object.Signature {
	return &object.Signature{Name: platformName, Email: platformEmail, When: when}
}
