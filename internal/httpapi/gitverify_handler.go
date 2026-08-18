package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// GitVerifier 对策略仓库与绑定的策略路径做只读校验，返回封闭枚举的结论。
//
// 接口收在边界层而不是直接依赖 *gitverify.Verifier，形状上最重要的一点是
// **它不返回 error**：go-git、SSH 与凭据解析的原始报错在 gitverify 内部
// 就被收敛成枚举，边界这边连一个可以 %w 透传出去的错误都拿不到。这条
// 约束靠类型成立，不靠调用点自觉（Global Constraints、spec §3.2）。
//
// 两层拆成两个方法而不是一个（design doc 2026-08-13 §3.3）：仓库级与路径级
// 的结论是两个各自封闭的枚举，合成一个方法就得再约定「这一层允许哪些
// 取值」，而那种约定只活在注释里。VerifyPath 把仓库级结论收成参数，于是
// 「仓库都没连上却报路径不存在」在类型上就写不出来。
type GitVerifier interface {
	// VerifyRepo 校验一个仓库：凭据可解析、仓库可达、认证通过、分支存在。
	VerifyRepo(ctx context.Context, r registry.GitRepo) (registry.RepoVerifyResult, *time.Time)
	// VerifyPath 校验 policyPath 是否存在于仓库的那个分支上。
	//
	// repoResult 是同一个仓库刚刚得到的仓库级结论，由调用方先取得再传进来。
	VerifyPath(
		ctx context.Context, r registry.GitRepo,
		repoResult registry.RepoVerifyResult, policyPath string,
	) (registry.BindingVerifyResult, *time.Time)
	// Drift 报告写进去的那份策略现在还在不在。
	//
	// 只读、不落库（design doc 2026-08-18-drift-detection §4）。任何读不出来
	// 的情形答 UNKNOWN，**绝不落到 IN_SYNC**。
	Drift(
		ctx context.Context, r registry.GitRepo, policyPath, lastWrittenCommit string,
	) registry.DriftResult
}

// repoVerifyStatus 是仓库级校验结论的响应形状。
//
// 与 registry.GitRepo 分开：接口形状属于边界层，而这个端点只回结论，
// 不回仓库地址与引用 —— 调用方已经有那些值了（规范 §35）。
type repoVerifyStatus struct {
	// VerifyResult 是本次仓库级校验的结论，取值限于已登记的枚举。
	VerifyResult registry.RepoVerifyResult `json:"verifyResult"`
	// VerifiedAt 是本次校验发生的时间；未发生校验时不出现。
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
}

// bindingVerifyStatus 是路径级校验结论的响应形状。
//
// 与 repoVerifyStatus 是两个类型而不是一个带 string 字段的通用形状：
// 两个枚举各自封闭，共用一个载体就等于允许把 AUTH_FAILED 写成路径级
// 的结论（design doc §3.3）。
type bindingVerifyStatus struct {
	// VerifyResult 是本次路径级校验的结论，取值限于已登记的枚举。
	VerifyResult registry.BindingVerifyResult `json:"verifyResult"`
	// VerifiedAt 是本次校验发生的时间；未发生校验时不出现。
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
}

// verifyRepo 校验一个仓库，返回结论与校验发生的时间。
//
// 新建仓库、重新校验与绑定路径级校验前的那一步共用它：三处必须给出同一套
// 结论，否则同一个仓库在"刚新建"与"点了重新校验"两个时刻会显示出互相
// 矛盾的状态。
//
// 未配置校验器（凭据后端选了 NONE）时结论是 NOT_VERIFIED，**绝不是 OK**：
// 没做过的检查不是通过了的检查。此时 verifiedAt 留空 —— 没有发生过校验，
// 就没有那个时刻，而一个带时间戳的 NOT_VERIFIED 会让界面显示出一次
// 从未发生的校验。
func verifyRepo(ctx context.Context, d Deps, r registry.GitRepo) (registry.RepoVerifyResult, *time.Time) {
	if d.GitVerifier == nil {
		return registry.RepoVerifyNotVerified, nil
	}

	result, at := d.GitVerifier.VerifyRepo(ctx, r)
	if !result.Valid() {
		// 实现返回了一个未登记的取值。它可能正是某个底层错误的文本，而这个
		// 值会同时写进 verify_result 列和响应体 —— 结论字段不能变成自由文本
		// 的载体。在这里收窄回枚举，失败方向朝「未确认」关，不朝「可信」开。
		//
		// 日志刻意不带上那个值：它的来源不可控，可能夹着仓库地址甚至凭据
		// 片段（spec §2.5）。要定位是哪个实现出的问题，看 request_id 对应的
		// 那次调用即可。
		d.Logger.Error("git verifier returned an unregistered repo verdict")
		return registry.RepoVerifyNotVerified, nil
	}
	return result, at
}

// verifyBindingPath 校验一个绑定的策略路径，返回结论与校验发生的时间。
//
// repoResult 必须是同一个仓库**刚刚**得到的仓库级结论：路径级以仓库级为
// 前提（design doc §3.3），拿一个几天前的 OK 顶上去，说出的是一句关于
// 另一个时刻的话。
func verifyBindingPath(
	ctx context.Context, d Deps, r registry.GitRepo,
	repoResult registry.RepoVerifyResult, policyPath string,
) (registry.BindingVerifyResult, *time.Time) {
	if d.GitVerifier == nil {
		return registry.BindingVerifyNotVerified, nil
	}

	result, at := d.GitVerifier.VerifyPath(ctx, r, repoResult, policyPath)
	if !result.Valid() {
		// 理由同 verifyRepo：不认识的值收窄成 NOT_VERIFIED，不落库、不回传。
		d.Logger.Error("git verifier returned an unregistered path verdict")
		return registry.BindingVerifyNotVerified, nil
	}
	return result, at
}

// bindingVerificationTarget 取出一次路径级校验要用到的两个对象：集群的绑定，
// 与它指向的那个仓库。
//
// 两个 handler（绑定保存与手动重校验）共用它：两处必须用同一套前置判断，
// 否则「保存时拒绝的形态」与「重校验时拒绝的形态」会各自演化，而它们描述
// 的是同一件事。写出响应时返回 false，调用方直接返回即可。
//
// 校验的是**绑定与它的仓库**，不是整个集群：集群其余字段是否合法与这条
// 路径无关，这正是把绑定拆成独立资源买到的东西（design doc §6）。
//
// 仓库这一道不可省：库里的仓库行未必还满足今天的校验规则 —— 迁移 6 把
// 绑定里的 https:// 地址原样搬进了 git_repo（design doc §6），而对这种记录
// 做一次出站得到的不是「校验没通过」，而是一句假的结论：认证方法与传输
// 对不上，一次拨号都不会发生，失败却会被报成「仓库不可达」。让操作者
// 直接看到「repoUrl 不是 SSH 形态」，才是他能据以行动的那句话。
func bindingVerificationTarget(
	w http.ResponseWriter, r *http.Request, d Deps, b registry.GitBinding,
) (registry.GitRepo, bool) {
	if err := registry.ValidateGitBinding(b); err != nil {
		writeRegistryError(w, r, d, err)
		return registry.GitRepo{}, false
	}
	repo, found, err := d.Registry.GitRepo(r.Context(), b.RepoID)
	if err != nil {
		writeRegistryError(w, r, d, err)
		return registry.GitRepo{}, false
	}
	if !found {
		response.WriteBusiness(w, response.CodeNotFound)
		return registry.GitRepo{}, false
	}
	if err := registry.ValidateGitRepo(repo); err != nil {
		writeRegistryError(w, r, d, err)
		return registry.GitRepo{}, false
	}
	return repo, true
}

// handleVerifyGitBinding 对一个集群的 Git 绑定重新做一次**路径级**只读校验。
//
// 手动重校验存在的理由是凭据轮换与权限修复之后需要一个新鲜的结论
// （spec §3.3）。平台不做后台定时校验：那意味着持续对所有仓库发起认证
// 连接，成本与噪声都不划算。
//
// 先取仓库级结论再做路径级，而不是给 VerifyPath 传一个方便的
// RepoVerifyOK 或库里那条几天前的旧结论：PATH_MISSING 成立的前提是仓库级
// **此刻**通过（design doc §3.3）。仓库都没连上时说路径不存在，是一句没有
// 依据的结论，而操作者会照着它去改 policyPath。
//
// 这一步得到的仓库级结论**不落库**：这个端点是路径级的（design doc §4），
// 顺手写一条 VERIFY_GIT_REPO 审计行，会让追溯的人读到一次操作者没有做过
// 的仓库校验。要刷新仓库级结论走 POST /git-repos/{repoID}/verify。
//
// 校验发生在事务之外：它是一次带秒级超时的出站请求，握着数据库事务等它
// 回来，会把一次网络抖动放大成一次锁竞争故障。这里的形态本身保证了这
// 一点 —— registry.Store 不暴露事务句柄，落库是校验结束后一次独立的
// SetGitVerifyResult，审计行由它在同事务里写下。
func handleVerifyGitBinding(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "clusterID")
		c, found, err := d.Registry.Cluster(r.Context(), id)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 集群不存在与集群没有绑定同码：从调用方视角两者都是「要校验的
		// 那个东西不在」。把「未绑定」说成参数错误，会让界面提示操作者
		// 去改请求，而该做的是先补一个绑定。
		if !found || c.Git == nil {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		repo, ok := bindingVerificationTarget(w, r, d, *c.Git)
		if !ok {
			return
		}

		repoResult, _ := verifyRepo(r.Context(), d, repo)
		result, at := verifyBindingPath(r.Context(), d, repo, repoResult, c.Git.PolicyPath)
		// at 为 nil 表示这一层根本没有发生校验（未配置校验器、仓库级不是
		// OK，或实现返回了一个未登记的取值）。此时不落库：SetGitVerifyResult
		// 只接受一个具体的时刻，写进去就等于宣称某时某刻校验过一次，而那件
		// 事没有发生 —— 顺带还会留下一条描述空白事件的 VERIFY_GIT_BINDING
		// 审计行。
		if at != nil {
			if err := d.Registry.SetGitVerifyResult(r.Context(), actorOf(r), id, result, *at); err != nil {
				writeRegistryError(w, r, d, err)
				return
			}
		}
		response.WriteOK(w, bindingVerifyStatus{VerifyResult: result, VerifiedAt: at})
	}
}
