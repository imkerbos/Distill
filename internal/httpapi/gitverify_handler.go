package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// GitVerifier 对一个 Git 绑定做一次只读校验，返回封闭枚举的结论。
//
// 接口收在边界层而不是直接依赖 *gitverify.Verifier，形状上最重要的一点是
// **它不返回 error**：go-git、SSH 与凭据解析的原始报错在 gitverify 内部
// 就被收敛成枚举，边界这边连一个可以 %w 透传出去的错误都拿不到。这条
// 约束靠类型成立，不靠调用点自觉（Global Constraints、spec §3.2）。
type GitVerifier interface {
	Verify(ctx context.Context, b registry.GitBinding) registry.VerifyResult
}

// verifyStatus 是校验结论的响应形状。
//
// 与 registry.GitBinding 分开：接口形状属于边界层，而这个端点只回结论，
// 不回仓库地址与引用 —— 调用方已经有那些值了。
type verifyStatus struct {
	// VerifyResult 是本次校验的结论，取值限于已登记的枚举。
	VerifyResult registry.VerifyResult `json:"verifyResult"`
	// VerifiedAt 是本次校验发生的时间；未发生校验时不出现。
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
}

// verifyBinding 校验一个绑定，返回该记录的结论与校验发生的时间。
//
// 保存路径与手动重校验共用它：两处必须给出同一套结论，否则同一个绑定
// 在"刚保存"与"点了重新校验"两个时刻会显示出互相矛盾的状态。
//
// 未配置校验器（secrets 段留空）时结论是 NOT_VERIFIED，**绝不是 OK**：
// 没做过的检查不是通过了的检查。此时 verifiedAt 留空 —— 没有发生过校验，
// 就没有那个时刻，而一个带时间戳的 NOT_VERIFIED 会让界面显示出一次
// 从未发生的校验。
func verifyBinding(ctx context.Context, d Deps, b registry.GitBinding) (registry.VerifyResult, *time.Time) {
	if d.GitVerifier == nil {
		return registry.VerifyNotVerified, nil
	}

	result := d.GitVerifier.Verify(ctx, b)
	if !result.Valid() {
		// 实现返回了一个未登记的取值。它可能正是某个底层错误的文本，而这个
		// 值会同时写进 verify_result 列和响应体 —— 结论字段不能变成自由文本
		// 的载体。在这里收窄回枚举，失败方向朝「未确认」关，不朝「可信」开。
		//
		// 日志刻意不带上那个值：它的来源不可控，可能夹着仓库地址甚至凭据
		// 片段（spec §2.5）。要定位是哪个实现出的问题，看 request_id 对应的
		// 那次调用即可。
		d.Logger.Error("git verifier returned an unregistered verdict")
		return registry.VerifyNotVerified, nil
	}

	now := time.Now().UTC()
	return result, &now
}

// handleVerifyGitBinding 对一个集群的 Git 绑定重新做一次只读校验。
//
// 手动重校验存在的理由是凭据轮换与权限修复之后需要一个新鲜的结论
// （spec §3.3）。平台不做后台定时校验：那意味着持续对所有仓库发起认证
// 连接，成本与噪声都不划算。
//
// 校验发生在事务之外：它是一次带秒级超时的出站请求，握着数据库事务等它
// 回来，会把一次网络抖动放大成一次锁竞争故障。这里的形态本身保证了这
// 一点 —— registry.Store 不暴露事务句柄，落库是校验结束后一次独立的
// SetGitVerifyResult，审计行由它在同事务里写下。
//
// 落库走 SetGitVerifyResult 而不是 UpdateCluster：跑一次校验不是一次配置
// 变更，它没有理由改写仓库地址，更没有理由重写集群行
// （design doc 2026-08-13 §1）。
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
		// 库里的记录未必还满足今天的校验规则（比如收紧 repoUrl 之前存下的
		// https:// 绑定）。对这种记录做一次出站得到的不是「校验没通过」，
		// 而是一句假的结论：认证方法与传输对不上，一次拨号都不会发生，失败
		// 却会被报成「仓库不可达」（spec §2.2）。让操作者直接看到「repoUrl
		// 不是 SSH 形态」，才是他能据以行动的那句话。
		//
		// 校验的是**绑定**，不是整个集群：集群其余字段是否合法与这条路径
		// 无关，这正是把绑定拆成独立资源买到的东西（design doc §6）。
		if err := registry.ValidateGitBinding(*c.Git); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}

		result, at := verifyBinding(r.Context(), d, *c.Git)
		// at 为 nil 表示这次根本没有发生校验（未配置校验器，或实现返回了
		// 一个未登记的取值）。此时不落库：SetGitVerifyResult 只接受一个具体
		// 的时刻，写进去就等于宣称某时某刻校验过一次，而那件事没有发生 ——
		// 顺带还会留下一条描述空白事件的 VERIFY_GIT_BINDING 审计行。
		if at != nil {
			if err := d.Registry.SetGitVerifyResult(r.Context(), actorOf(r), id, result, *at); err != nil {
				writeRegistryError(w, r, d, err)
				return
			}
		}
		response.WriteOK(w, verifyStatus{VerifyResult: result, VerifiedAt: at})
	}
}
