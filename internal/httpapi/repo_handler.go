package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/ssh"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// gitRepoPayload 是策略仓库新建与修改的请求体形状。
//
// 四个字段就是仓库的全部可写内容（design doc 2026-08-13 §3.1）。它们的
// 共同点是：都由操作者提供，平台自己产生不了。
//
// verifyResult 与 verifiedAt **不在**这里，理由与绑定那侧完全一样：结论是
// 平台自己校验出来的事实，一个能被请求体设成 OK 的结论就是一句无法被
// 证伪的「这个仓库连得上」。它们由本文件的 handler 从 verifyRepo 的返回值
// 填入，或者由 SetGitRepoVerifyResult 单独写下。
//
// 不是「收下再忽略」：一个永远不会被采纳的字段留在请求形状里，只会让
// 下一个调用方以为它有用。
//
// credentialRef 不在此列：它是 Secret Manager 里的引用，是操作者填写的
// 配置，不是平台对外部世界的断言 —— 由调用方提供正是它的设计意图。
type gitRepoPayload struct {
	ID            string `json:"repoId"`
	URL           string `json:"repoUrl"`
	Branch        string `json:"branch"`
	CredentialRef string `json:"credentialRef"`
	// PrivateKey 是这个仓库的 SSH 私钥（PEM），**只入不出**。
	//
	// 平台把它加密存进 stored_secret，credentialRef 自动填成 repoId，
	// 调用方不需要理解 ref 这个概念（design doc 2026-09-01 §3.5）。
	//
	// 任何响应都不含它，列表与详情也不含 —— 它写进去之后除了平台自己
	// 拿去连 Git，没有任何路径能取出来。
	PrivateKey string `json:"privateKey,omitempty"`
}

// toRepo 把请求体转成领域对象。
func (p gitRepoPayload) toRepo() registry.GitRepo {
	repo := registry.GitRepo{
		ID:            p.ID,
		URL:           p.URL,
		Branch:        p.Branch,
		CredentialRef: p.CredentialRef,
	}
	// 带了私钥就自动把 ref 指向这个仓库自己：调用方填了钥匙却还要再想一个
	// 引用名，是把平台内部的概念摊给他。显式填了的 ref 不覆盖 —— 那是
	// 让一把钥匙给多个仓库共用的正当写法。
	if p.PrivateKey != "" && repo.CredentialRef == "" {
		repo.CredentialRef = p.ID
	}
	return repo
}

// decodeGitRepoPayload 解析请求体。
//
// 解析失败是协议层的问题（请求体本身不是合法 JSON），不是业务失败 ——
// 与集群与绑定端点保持一致，返回真实的 400 而不是 200 + code。
func decodeGitRepoPayload(w http.ResponseWriter, r *http.Request) (gitRepoPayload, bool) {
	var p gitRepoPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
		return gitRepoPayload{}, false
	}
	return p, true
}

// handleListGitRepos 返回全部未删除的策略仓库。
func handleListGitRepos(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := d.Registry.GitRepos(r.Context())
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if list == nil {
			list = []registry.GitRepo{}
		}
		response.WriteOK(w, list)
	}
}

// handleCreateGitRepo 登记一个策略仓库，并在登记之后自动校验一次
// （design doc 2026-08-13 §4）。
//
// 先落库再校验，不是先校验再落库：SetGitRepoVerifyResult 要写的那一行必须
// 已经存在。顺序反过来还有一个后果 —— 一个连 ValidateGitRepo 都过不了的
// 仓库（比如 https:// 地址）会先花掉一次出站，才等到一句和网络无关的报错。
//
// **校验失败不阻止登记**：存下来和可信是两件事。合并它们，要么是拒绝
// 一个正确的仓库，要么是让未经校验的仓库看上去可以下发。
func handleCreateGitRepo(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeGitRepoPayload(w, r)
		if !ok {
			return
		}
		repo := p.toRepo()
		if err := d.Registry.CreateGitRepo(r.Context(), actorOf(r), repo); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 私钥在仓库登记成功之后再存：反过来的话，一次登记失败会留下一条
		// 没有仓库的孤儿凭据，而没有任何东西会去清它。
		if !storePrivateKey(w, r, d, repo.CredentialRef, p.PrivateKey) {
			return
		}
		writeRepoVerification(w, r, d, repo)
	}
}

// handleUpdateGitRepo 整体替换一个仓库的登记信息。
//
// 语义是替换而非合并：请求体没给的字段会被写成空值，因此它挂在 PUT 上。
//
// **repoID 取自路径，请求体里的 repoId 不参与**：仓库 ID 是不可改键
// （controller 裁定）。允许改它，等于让一次「改地址」顺手把审计里
// git-repo/<旧 ID> 那一串行变成无主记录 —— 而审计行正是事后唯一能回答
// 「这个仓库的凭据是谁换的」的东西。
//
// 修改之后不自动校验：UpdateGitRepo 会把上一次的结论清成 NOT_VERIFIED
// （换了地址之后，旧的 OK 描述的是另一个仓库），操作者要新结论就点重新
// 校验。这里不顺手代劳，是为了让「改配置」与「跑校验」在审计里始终是
// 两条可以分开读的记录。
func handleUpdateGitRepo(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeGitRepoPayload(w, r)
		if !ok {
			return
		}
		p.ID = chi.URLParam(r, "repoID")
		repo := p.toRepo()
		if err := d.Registry.UpdateGitRepo(r.Context(), actorOf(r), repo); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 换钥匙走这条路：带了新私钥就覆盖，没带就保持原样。
		// **没带不等于要清空** —— 更新仓库地址时不该顺手把凭据删掉。
		if !storePrivateKey(w, r, d, repo.CredentialRef, p.PrivateKey) {
			return
		}
		response.WriteOK(w, map[string]string{"repoId": p.ID})
	}
}

// handleDeleteGitRepo 下线一个策略仓库。
//
// 仍被某个集群绑定时拒绝，不做级联（design doc §4）：级联会让一次仓库
// 清理静默解除某个集群的策略下发路径。拒绝由 SoftDeleteGitRepo 在事务内
// 判定并返回 registry.ErrRepoInUse，边界层把它映射成一个调用方能据以行动
// 的业务失败（见 writeRegistryError）。
func handleDeleteGitRepo(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "repoID")
		if err := d.Registry.SoftDeleteGitRepo(r.Context(), actorOf(r), id); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 仓库删掉了，它的凭据也跟着删：留下一条没有仓库的密文，
		// 没有任何东西会去清它，而它仍然是一把能连上那个仓库的钥匙。
		//
		// 删凭据失败不让整个删除失败（仓库已经删了，报错会让操作者以为
		// 没删掉而重试），但要留下痕迹供人工清理。
		if d.CredentialStore != nil {
			if err := d.CredentialStore.Delete(r.Context(), id); err != nil {
				d.Logger.Error("the repository is deleted but its credential remains",
					"request_id", RequestIDFrom(r.Context()), "repo", id, "error", err)
			}
		}
		response.WriteOK(w, map[string]string{"repoId": id})
	}
}

// handleVerifyGitRepo 对一个仓库重新做一次仓库级只读校验。
//
// POST 而非 GET：这次调用会真的发起一次出站认证连接，并写下新的结论与
// 一条审计行 —— 不是一次读取。
//
// 落库走 SetGitRepoVerifyResult 而不是 UpdateGitRepo：跑一次校验不是一次
// 配置变更，它没有理由改写仓库地址、分支或凭据。
func handleVerifyGitRepo(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo, found, err := d.Registry.GitRepo(r.Context(), chi.URLParam(r, "repoID"))
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if !found {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		writeRepoVerification(w, r, d, repo)
	}
}

// writeRepoVerification 校验一个仓库、落下结论并写出响应。
//
// 新建与重新校验共用它：两处必须给出同一套结论与同一种响应形状，否则
// 同一个仓库在"刚建好"与"点了重新校验"两个时刻会显示出互相矛盾的状态。
//
// 库里的仓库行未必还满足今天的校验规则 —— 迁移 6 把绑定里的 https:// 地址
// 原样搬进了 git_repo（design doc §6）。对这种记录做一次出站得到的不是
// 「校验没通过」，而是一句假的结论：认证方法与传输对不上，一次拨号都不会
// 发生，失败却会被报成「仓库不可达」（spec §2.2）。让操作者直接看到
// 「repoUrl 不是 SSH 形态」，才是他能据以行动的那句话。
func writeRepoVerification(w http.ResponseWriter, r *http.Request, d Deps, repo registry.GitRepo) {
	if err := registry.ValidateGitRepo(repo); err != nil {
		writeRegistryError(w, r, d, err)
		return
	}
	result, at := verifyRepo(r.Context(), d, repo)
	// at 为 nil 表示这次根本没有发生校验（未配置校验器，或实现返回了一个
	// 未登记的取值）。此时不落库：SetGitRepoVerifyResult 只接受一个具体的
	// 时刻，写进去就等于宣称某时某刻校验过一次，而那件事没有发生 ——
	// 顺带还会留下一条描述空白事件的 VERIFY_GIT_REPO 审计行。
	if at != nil {
		if err := d.Registry.SetGitRepoVerifyResult(r.Context(), actorOf(r), repo.ID, result, *at); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
	}
	response.WriteOK(w, repoVerifyStatus{VerifyResult: result, VerifiedAt: at})
}

// storePrivateKey 把请求里带的私钥加密存进平台自己的库。
//
// 空私钥是合法的：仓库可以先登记下来、凭据稍后再配（同 ValidateGitRepo
// 对 credentialRef 的处置）。返回 false 表示响应已写好，调用方直接返回。
func storePrivateKey(
	w http.ResponseWriter, r *http.Request, d Deps, ref, key string,
) bool {
	if key == "" {
		return true
	}
	if d.CredentialStore == nil {
		// **不静默忽略。** 忽略之后操作者以为钥匙配好了，而失败会推迟到
		// 写回那一刻，报成"仓库不可达"——排查方向完全错。
		response.WriteInvalid(w,
			"这个部署没有把凭据交给平台保管（secretsBackend 不是 DB），"+
				"因此不能在这里填私钥。请改用该后端约定的方式配置凭据，"+
				"或把 secretsBackend 切成 DB。")
		return false
	}
	// **落库之前先解析一次。** 存一段解析不了的私钥，失败会推迟到写回那一刻，
	// 而那时报的是"仓库不可达"（design doc 2026-09-01 §3.7）。
	if _, err := ssh.ParsePrivateKey([]byte(key)); err != nil {
		// 底层错误不外带：它可能带上私钥的片段。
		response.WriteInvalid(w,
			"这不是一把能用的 SSH 私钥。请粘贴私钥文件（PEM，"+
				"-----BEGIN ... PRIVATE KEY----- 开头）的完整内容，"+
				"不是公钥（.pub）、也不是访问令牌。")
		return false
	}
	if err := d.CredentialStore.Put(r.Context(), ref, []byte(key)); err != nil {
		d.Logger.Error("cannot store a repository credential",
			"request_id", RequestIDFrom(r.Context()), "repo", ref, "error", err)
		response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
		return false
	}
	return true
}
