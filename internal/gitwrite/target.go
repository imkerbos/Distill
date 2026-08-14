package gitwrite

import (
	"context"
	"errors"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/imkerbos/Distill/internal/registry"
)

// branchPrefix 是平台推出去的分支唯一允许的前缀（design doc §2：
// distill/<clusterID>-<UTC 时间戳>）。
//
// 前缀在这里再查一次，不是重复校验：ErrTargetIsDeployBranch 只挡得住「与
// 这个仓库配置的那条分支同名」，挡不住一份把 main 写进 Branch、而这个仓库
// 恰好部署 release 的计划 —— 那条 main 仍然是别人的部署分支。守卫的位置
// 必须紧贴真正发起写的那一步。
const branchPrefix = "distill/"

// ErrTargetIsDeployBranch 表示计划的目标分支就是绑定里配置的那条分支。
//
// 那条分支是 Config Sync 应用的对象：推它等于界面上点一下就是一次生产变更，
// 平台的任何缺陷都直达集群，中间没有一道人能看见的门（design doc §2）。
var ErrTargetIsDeployBranch = errors.New("gitwrite: refusing to push the branch configured on the binding")

// ErrTargetBranchExists 表示目标分支在远端已经存在。
//
// 分支名带 UTC 时间戳，天然不冲突；真撞上了说明某个假设破了，而假设破了时
// 诚实的反应是失败，不是把那条分支上已有的东西推着往前走（design doc §2）。
var ErrTargetBranchExists = errors.New("gitwrite: target branch already exists on the remote")

// ErrNothingToPush 表示计划的内容与基底分支逐字节相同，无事可做。
//
// 单列一个哨兵而不是返回空 SHA 加 nil：调用方要把「什么都没变」与「推成了」
// 分开报给操作者，而一个空字符串很容易被当成后者顺着往下走。
var ErrNothingToPush = errors.New("gitwrite: plan content matches the branch, nothing to push")

// ErrNoCommitMessage 表示计划没带提交信息。
//
// 提交信息由计划携带、进指纹（design doc §4、§7），因此它是操作者确认过的
// 那一份。缺了它不能就地补一句：平台现编的话没有人批准过，却会永久留在
// 仓库历史上，并且正是合并请求上的评审人读来判断合不合的那句。
var ErrNoCommitMessage = errors.New("gitwrite: plan carries no commit message")

// ErrInvalidTargetBranch 表示目标分支名不是一个可用的 distill/* 分支名。
//
// 刻意不带分支名：这个错误会一路冒泡到调用方的结论映射，而结论是封闭枚举
// （安全规范 §22）。
var ErrInvalidTargetBranch = errors.New("gitwrite: target branch is not a usable distill/* branch name")

// targetRef 校验计划的目标分支，返回它的完整引用名。
//
// 三道判断的次序不能换：先挡住「就是那条部署分支」，再要求 distill/ 前缀，
// 最后才是引用名本身是否合法。前两道各自对应一种具体伤害，把它们排在合法性
// 之后，会让一个语法上没问题的 main 先通过。
func targetRef(repo registry.GitRepo, plan registry.WritebackPlan) (plumbing.ReferenceName, error) {
	// 部署分支为空时无从判断目标是不是它，一律按「是」拒绝：这一层的失败
	// 方向必须是关，不是开。
	if repo.Branch == "" || plan.Branch == repo.Branch {
		return "", ErrTargetIsDeployBranch
	}
	if !strings.HasPrefix(plan.Branch, branchPrefix) || plan.Branch == branchPrefix {
		return "", ErrInvalidTargetBranch
	}
	ref := plumbing.NewBranchReferenceName(plan.Branch)
	// 交给 go-git 自己的引用名规则，不另写一套：控制字符、..、@{、结尾
	// 的 .lock 这类写法在这里挡掉，免得一个畸形的名字被远端当成别的引用。
	if err := ref.Validate(); err != nil {
		return "", ErrInvalidTargetBranch
	}
	return ref, nil
}

// checkTargetAbsent 断言目标分支在远端还不存在。
//
// 这一道与「推送不带 force」是两层，不是一件事的两种写法：
//   - 不带 force 只挡得住非快进的更新。目标分支若恰好指向基底分支上的某个
//     祖先提交，一次不带 force 的推送是**快进**，远端会接受 —— 而那同样是
//     推到了一条别人建的分支上。
//   - 反过来，本函数与推送之间存在一个时间窗，窗内新建的同名分支只有推送
//     那一层挡得住。
//
// 两层各自漏的东西不一样，所以都要在。剩下的窗口（同一秒内另一个写入者建
// 出同名分支、且它指向基底的祖先）由分支名里的 UTC 时间戳与集群名收窄。
func checkTargetAbsent(
	ctx context.Context,
	local *git.Repository,
	auth *gogitssh.PublicKeys,
	target plumbing.ReferenceName,
) error {
	remote, err := local.Remote(git.DefaultRemoteName)
	if err != nil {
		return err
	}
	// 走同一个 auth：这次 ls-remote 也是一次出站，同样必须经过钉死的
	// host key 与目的地址判定。
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.Name() == target {
			return ErrTargetBranchExists
		}
	}
	return nil
}

// pushOptions 构造这次推送的选项。
//
// 单独摘出来是为了让「这条推送永不强制」可以被直接断言：Force、
// ForceWithLease 与 refspec 前面那个 + 都只在这里出现，别处不构造
// PushOptions。留在调用点的内联字面量里，它被加上一个 Force: true 也不会有
// 任何测试变红 —— 远端已存在的分支在本包里先被 checkTargetAbsent 挡下了，
// 强制与否在行为上看不出来。
//
// RefSpecs 显式写成 <target>:<target>，不依赖默认 refspec：默认会推
// refs/heads/*，那会把基底分支一起推出去。
//
// Prune 保持 false：它会删除远端匹配 refspec 而本地没有的引用，而本包从不
// 删除任何东西（§3）。
func pushOptions(target plumbing.ReferenceName, auth *gogitssh.PublicKeys) *git.PushOptions {
	return &git.PushOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(target.String() + ":" + target.String())},
		Auth:       auth,
		Force:      false,
		Prune:      false,
	}
}
