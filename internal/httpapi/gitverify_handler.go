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

// applyVerdict 把一次校验的结论写进绑定。
//
// 结论与时间一律取本次校验的产物，不从库里的现值继承：上一次结论描述的
// 是上一次那个绑定，而这次保存可能换了仓库、分支或引用。沿用它等于用
// 一个针对别的目标得出的判断，去声明当前这个目标可信。
func applyVerdict(b *registry.GitBinding, result registry.VerifyResult, at *time.Time) {
	if b == nil {
		return
	}
	b.VerifyResult = result
	b.VerifiedAt = at
}

// verifyOnSave 在落库之前校验一次绑定，并把结论写进 b。
//
// **校验失败不阻止保存。** 它没有返回值，调用点也就没有"校验没过就不写了"
// 这个分支可写：一次网络抖动不该让操作者无法记录一个正确的绑定。存下来和
// 可信是两件事 —— 合并它们，要么是拒绝正确的数据，要么是让未经校验的数据
// 看上去可以下发（spec §3.2，与 verdict / confidence 分离同一条原则）。
//
// 调用点必须在事务之外，即在 Registry 的写方法之前：这里会做一次带秒级
// 超时的出站请求。b 为 nil（本次保存不带绑定）时什么也不做，不发出站。
func verifyOnSave(r *http.Request, d Deps, b *registry.GitBinding) {
	if b == nil {
		return
	}
	result, at := verifyBinding(r.Context(), d, *b)
	applyVerdict(b, result, at)
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
// UpdateCluster，审计行由它在同事务里写下。
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
		// https:// 绑定）。这种记录走完出站也白走：结论回写要经过
		// UpdateCluster，而那里的 ValidateCluster 会拒掉整行。先在这里判一次，
		// 操作者拿到的是「repoUrl 不是 SSH 形态」这个真实原因，而不是一个
		// 花掉一次握手才得到、随后又存不下去的结论。
		if !validatedBeforeVerifying(w, r, d, c) {
			return
		}

		result, at := verifyBinding(r.Context(), d, *c.Git)
		applyVerdict(c.Git, result, at)

		if err := d.Registry.UpdateCluster(r.Context(), actorOf(r), c); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, verifyStatus{VerifyResult: c.Git.VerifyResult, VerifiedAt: c.Git.VerifiedAt})
	}
}
