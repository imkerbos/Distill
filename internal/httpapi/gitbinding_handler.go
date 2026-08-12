package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// gitBindingPayload 是 Git 绑定的请求体形状。
//
// 四个字段就是绑定的全部可写内容（design doc 2026-08-13 §5）。它们的
// 共同点是：都由操作者提供，平台自己产生不了。凡是平台自己产生的东西，
// 都不在这里。
//
// 刻意**不含** lastWrittenCommit：策略存在 Git 里的理由是双重审计 ——
// Git 保存记录，这个字段是平台对那份记录的声明，漂移检测拿两者比对。
// 允许调用方设置它，等于允许调用方让平台的说法与仓库现状对上：基准从此
// 无法被证伪，而平台会带着信心报告「无漂移」，比根本没有基准更糟。
//
// verifyResult 与 verifiedAt 同样**不在**请求形状里，理由完全一样：结论是
// 平台自己校验出来的事实，一个能被请求体设置的 OK 就是一句无法被证伪的
// 「可以下发」。它们由本 handler 从 verifyBinding 的返回值填入。
//
// 三者都不是「收下再忽略」：一个永远不会被采纳的字段留在请求形状里，
// 只会让下一个调用方以为它有用。
//
// credentialRef 不在此列：它是 Secret Manager 里的引用，是操作者填写的
// 配置，不是平台对外部世界的断言 —— 由调用方提供正是它的设计意图。
type gitBindingPayload struct {
	RepoURL       string `json:"repoUrl"`
	Branch        string `json:"branch"`
	PolicyPath    string `json:"policyPath"`
	CredentialRef string `json:"credentialRef"`
}

// toBinding 把请求体转成领域对象。
//
// LastWrittenCommit 一律留空，且这里不从库里的现值继承：BindGitRepo 是
// 整体替换，改绑之后旧 SHA 指向的历史与新目标无关（见 mysqlregistry
// 那侧的同名说明）。
func (p gitBindingPayload) toBinding() registry.GitBinding {
	return registry.GitBinding{
		RepoURL:       p.RepoURL,
		Branch:        p.Branch,
		PolicyPath:    p.PolicyPath,
		CredentialRef: p.CredentialRef,
	}
}

// handleBindGitRepo 绑定或改绑一个集群的策略仓库。
//
// PUT 而非 PATCH：四个字段是绑定的全部可写内容，这里写整行。挂成 PATCH
// 是在邀请调用方只发一个字段，然后把 policyPath 清空 —— 那不是一次失败的
// 请求，而是此后策略被写去了仓库根目录。
//
// 保存时自动校验一次（spec 2026-08-12 §3.3），**校验失败不阻止保存**：
// 一次网络抖动不该让操作者无法记录一个正确的绑定。存下来和可信是两件事 ——
// 合并它们，要么是拒绝正确的数据，要么是让未经校验的数据看上去可以下发
// （与 verdict / confidence 分离同一条原则）。
func handleBindGitRepo(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p gitBindingPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			// 与 decodeClusterPayload 同一处置：请求体不是合法 JSON 是协议层
			// 问题，不是业务失败，必须是真实的 400。
			response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
			return
		}
		id := chi.URLParam(r, "clusterID")
		b := p.toBinding()
		// 领域校验先于出站：BindGitRepo 会自己再校验一次，那才是不可绕过的
		// 关卡；这一道是**时序**上的。最刺眼的形态是 repoUrl 不是 SSH 地址 ——
		// 那种绑定在保存这一步就会被指名拒绝，可要是先出站，操作者得先等一个
		// 带秒级超时的握手，才等到一句和超时无关的报错。
		if err := registry.ValidateGitBinding(b); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 集群不存在同样先于出站问一次：一次针对不存在集群的 SSH 握手救不了
		// 这个请求，只会在远端日志里留下一条没人能解释的连接。存在性最终由
		// BindGitRepo 在事务内加锁断言，这里这道不是它的替代品。
		_, found, err := d.Registry.Cluster(r.Context(), id)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if !found {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		// 出站必须在事务之外，即在 BindGitRepo 之前：握着数据库事务等一次
		// 带秒级超时的网络请求回来，会把一次网络抖动放大成一次锁竞争故障。
		b.VerifyResult, b.VerifiedAt = verifyBinding(r.Context(), d, b)
		if err := d.Registry.BindGitRepo(r.Context(), actorOf(r), id, b); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 回结论而不是回一句「已保存」：保存触发了一次校验，而操作者点完
		// 保存最想知道的正是那次校验的结果。只落库不回，界面得再发一次请求
		// 才看得到 —— 形状与重校验端点一致，两处显示的是同一套结论。
		response.WriteOK(w, verifyStatus{VerifyResult: b.VerifyResult, VerifiedAt: b.VerifiedAt})
	}
}

// handleUnbindGitRepo 解除一个集群的 Git 绑定。
//
// 解绑是自己的动词，不是「四个字段全空的一次保存」：后者要靠调用方与
// 服务端就一个约定俗成的空值形状达成默契，而任何一次误发的空请求体都会
// 变成一次无声的解绑。
//
// 集群不存在与未绑定同码：从调用方视角两者都是「要解绑的那个东西不在」，
// 而 UnbindGitRepo 对两者都返回 ErrNotFound。
func handleUnbindGitRepo(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "clusterID")
		if err := d.Registry.UnbindGitRepo(r.Context(), actorOf(r), id); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"id": id})
	}
}
