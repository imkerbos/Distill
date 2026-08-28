package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/imkerbos/Distill/internal/buildinfo"
	"github.com/imkerbos/Distill/internal/gitwrite"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// PolicyWriter 把一份已确认的写回计划推进策略仓库的一条新分支，
// 返回平台推上去的那个 commit SHA。*gitwrite.Writer 满足它。
//
// 接口收在边界层而不是直接依赖 *gitwrite.Writer，理由与 GitVerifier 同源：
// 这一层只需要「把这份计划推出去」，而 gitwrite 还带着 go-git、内存工作区
// 与 SSH transport。允许为 nil —— 未配置 secrets 的部署没有写回这回事，
// 而 nil 表示"没有这条路径"，**不是**"随便谁都能推"（见 handlePolicyWritebackPush）。
//
// 与 GitVerifier 不同的是它返回 error：gitwrite 是本轮已经定稿、只许消费的
// 包，它的失败以哨兵错误表达。因此收敛成封闭码这件事落在本层的
// writeGitWriteError 上，而那个函数的 default 分支不把错误写进响应，也不写进
// 日志 —— go-git 的报错里带着仓库路径、主机名与传输细节（规范 §19、§22）。
type PolicyWriter interface {
	Push(ctx context.Context, repo registry.GitRepo, plan registry.WritebackPlan) (string, error)
	// List 只读地枚举策略仓库：policyPath 下已有的文件，与现存的 distill/*
	// 分支。计划里那两份「平台不会碰、交人工处置」的清单由它填
	// （design doc §2、§3）。
	//
	// 与 Push 同一个接口、同一个装配开关，不另开一个字段：两者是同一条受
	// 守卫的出站链路，而"装了写入器却没装枚举器"这种半装配形态一旦表达得出来，
	// 就会有人在那种形态下出计划 —— 出来的计划会向操作者断言仓库里没有多余
	// 文件，而平台从没看过。
	List(ctx context.Context, repo registry.GitRepo, policyPath string) (gitwrite.RepoListing, error)
}

// writebackFileName 是写回落在 policyPath 下的文件名。
//
// 固定不变、不带时间戳：内容与分支上现有内容逐字节相同时什么都不做
// （design doc 2026-08-14 §6），而一个每次都换名字的文件让这条幂等永远
// 不成立，同时因为平台从不删除文件（§3），仓库里会积起一串谁都不敢清理的
// 旧策略。集群身份由 policyPath 承担 —— 绑定本来就是一集群一路径。
// writebackDirName 是平台在策略路径下独占的那一层目录
// （design doc 2026-08-24 §3.1）。
//
// 落点是 <policyPath>/distill/<namespace>.yaml：这一层子树属于平台，里面一个
// 命名空间一个文件，文件名就是命名空间。
//
// **文件名不能反过来固定成 distill-policy.yaml 再按目录区分命名空间**：那样一次
// 写回在 PR 的文件列表里是十几行同名条目，编辑器标签页、搜索结果、git log --stat
// 全都分辨不出谁是谁，而评审人正是靠这份列表决定先看哪一块。
//
// **也不能直接落在 <policyPath>/<namespace>.yaml**：那会让平台静默覆盖仓库里
// 与它无关的同名文件，且那条路径这时算「本次要写的文件」，于是它还会从多余
// 文件清单里消失 —— 没有任何一屏显示这件事发生过。这个缺陷是实现时由测试
// 发现的（fixture 里的 legacy 命名空间撞上了仓库里别人放的 legacy.yaml）。
// 多一层 distill/ 之后，别人的文件不在这棵子树里，撞名不可能发生。
//
// 名字不带时间戳：内容逐字节相同时什么都不做（2026-08-14 design doc §6 幂等），
// 而每次换名会在仓库里积起一串旧策略。命名空间受 DNS-1123 约束（小写字母、
// 数字、连字符），拼进路径是安全的 —— 仍然过一遍 checkWithinPolicyPath，
// 那道判断防的是将来约束被放宽。
const writebackDirName = "distill"

// writebackBranchPrefix 与 writebackStampLayout 组成目标分支名
// `distill/<clusterID>-<UTC 时间戳>`（design doc §2）。
//
// 时间戳的格式与导出文件名同一套，秒级：它同时是这次计划的时刻，写回文件的
// 注释头取的就是从这里解析回来的那个值（见 parseWritebackBranch）。
const (
	writebackBranchPrefix = "distill/"
	writebackStampLayout  = "20060102T150405Z"
)

// 写回被拒时给操作者的固定文案。
//
// 每一条都说明"为什么不给"而不是只说"不支持"：这三种拒绝各自对应一个
// 不同的下一步动作（清掉筛选、先修好绑定、先确认几条规则），一句
// "请求参数不合法"会把操作者引向检查拼写。
const (
	writebackNamespaceMsg = "写回的 dry-run 预测按整集群计算，无法描述只含单个命名空间的策略集；" +
		"筛选后的文件会带着一份把文件里没有的规则也算进去的结论。请清除命名空间筛选后再写回。"
	writebackEmptyMsg = "当前时间窗下没有任何启用中的规则，无可写回内容；" +
		"空的策略文件在不同集群上含义相反，因此不生成。"
	writebackBranchMsg = "目标分支不是这个集群的一份写回计划所能产出的分支名。请重新出一次计划。"
	writebackStaleMsg  = "这份计划的指纹与平台此刻重新算出的不一致：" +
		"集群、时间窗或别人的确认在你确认之后发生了变化。请重新出一次计划并核对四类计数。"
	writebackNoFingerprintMsg = "推送必须携带你确认过的那份计划的指纹。" +
		"不带指纹的请求只出计划，不写任何东西。"
	// 枚举失败时整次不出计划，而不是给一份把两份清单留空的计划：空清单在
	// 界面上读起来是"仓库里没有多余文件"，那是一句平台从没算过的断言，
	// 且偏在让人放心的方向（design doc §4）。
	writebackSurveyMsg = "平台没能列出这个仓库当前的内容，因此这次不出计划。" +
		"一份不知道仓库里已有什么的计划，会把「没有多余文件」当成结论说给你听，" +
		"而那句话平台并没有算过。请稍后重试；持续失败请检查仓库绑定。"
)

// writebackUnverifiedMsg 说明为什么一次校验结论不是 OK 的绑定不能写。
//
// 带上结论本身（封闭枚举，不是自由文本）：操作者要据此决定去修凭据、
// 改仓库地址还是改分支名，而一句"绑定未通过校验"三种情况都覆盖不了。
func writebackUnverifiedMsg(result registry.RepoVerifyResult) string {
	return fmt.Sprintf("写回前的仓库级重新校验结论是 %s，不是 OK，因此不写。"+
		"绑定上那个 verifiedAt 是历史事实，不是此刻的状态。", result)
}

// writebackPlanView 是出计划端点的响应。
//
// 两层校验结论与计划一起回：计划是操作者二次确认的对象，而"平台刚刚
// 重新校验过、结论是什么"正是他决定确不确认的依据之一（design doc §4）。
type writebackPlanView struct {
	// Plan 是这次写回的完整计划，含它的内容指纹。
	Plan registry.WritebackPlan `json:"plan"`
	// RepoVerifyResult 是写回前**刚刚**重新做的仓库级校验结论。
	RepoVerifyResult registry.RepoVerifyResult `json:"repoVerifyResult"`
	// BindingVerifyResult 是同一次前置里的路径级结论。
	BindingVerifyResult registry.BindingVerifyResult `json:"bindingVerifyResult"`
}

// writebackPushView 是推送端点的响应。
//
// 计数在这里再回一次，且是写之前重算的那一套：操作者点确认与平台真的推
// 出去之间还隔着一次重算，回传的必须是真正随这次提交落进仓库的那几个数。
type writebackPushView struct {
	// Branch 是平台推上去的那条新分支。
	Branch string `json:"branch"`
	// Commit 是平台推上去的那个 commit，不是合并后的（design doc §8）。
	Commit string `json:"commit"`
	// Files 是这次提交涉及的文件数。
	Files int `json:"files"`
	// Counts 是写前重算的四类计数。
	Counts map[predict.ChangeKind]int `json:"counts"`
}

// writebackPushRequest 是推送请求体。
//
// **只有两个字段，且都不是数字。** 写回请求不携带任何关于影响面的取值
// （design doc §4）：计数由平台在写前重算，一个能自述"我这次只会打断 0 条"
// 的调用方等于自己描述自己那次变更的爆炸半径。
//
// Branch 与 Fingerprint 都是从计划响应里原样回带的：目标分支进指纹
// （registry.FingerprintOf），因此平台重算出的那份计划必须用同一条分支名
// 才可能对得上 —— 分支名里的时间戳同时是这份计划的时刻，写回文件注释头
// 取的就是它。伪造分支只会让指纹对不上，而指纹是唯一放行的条件。
// writebackPlanRequest 是出计划时可带的确认删除清单。
//
// 可选：不带就是"这次不删任何东西"。缺省行为必须安全（design doc §5）。
type writebackPlanRequest struct {
	// Deletions 是操作者勾选的待删路径。
	Deletions []string `json:"deletions"`
}

type writebackPushRequest struct {
	// Branch 是计划里的目标分支，形如 distill/<clusterID>-<UTC 时间戳>。
	Branch string `json:"branch"`
	// Deletions 是操作者确认要删除的路径。
	//
	// 它是这个请求里唯一一项平台重算不出来的东西 —— 删哪几个是人的决定。
	// 但它不是授权：能不能删由平台重算出的清单说了算，而指纹钉住的正是
	// "文件 + 这份确认"这一整份计划（design doc 2026-08-24 §4.4）。
	Deletions []string `json:"deletions"`
	// Fingerprint 是操作者确认的那份计划的内容指纹。
	//
	// **缺省行为必须是安全的**：空指纹一律拒绝，不写任何东西（§5）。
	Fingerprint string `json:"fingerprint"`
}

// plannedWriteback 是两个端点共用的前置产物。
type plannedWriteback struct {
	// repo 是绑定指向的策略仓库。
	repo registry.GitRepo
	// plan 是这一刻重算出来的计划，指纹已由 registry.NewWritebackPlan 填好。
	plan registry.WritebackPlan
	// repoResult 与 pathResult 是这一刻重新校验的两层结论。
	repoResult registry.RepoVerifyResult
	pathResult registry.BindingVerifyResult
}

// handlePolicyWritebackPlan 出一份写回计划，不写任何东西。
//
// 这是写回的 dry-run，也是默认形态（design doc 2026-08-14 §5）：一个没带
// 指纹的请求永远不会写 —— 缺省行为是安全的，不是危险的。本端点唯一的写入
// 是那条 PLAN_POLICY_WRITEBACK 审计行。
func handlePolicyWritebackPlan(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 计划的时刻取当前，截到秒：它既进分支名，也是写回文件注释头里的
		// 那个时刻，而分支名的时间戳只有秒级精度。不截的话，推送时从分支名
		// 解析回来的时刻与出计划时用的那个差着毫秒，渲染出的文件逐字节不同，
		// 指纹永远对不上 —— 二次确认那道门会变成一道谁都过不去的墙。
		at := time.Now().UTC().Truncate(time.Second)
		// 计划端点也收确认清单：操作者勾完删除项要能拿到**覆盖了那份确认**
		// 的指纹，否则他确认的东西与他手上那个指纹说的不是一回事。
		var req writebackPlanRequest
		if r.Body != nil {
			// 空 body 与 `{}` 都是合法的一次计划请求：没有勾任何删除项。
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		planned, ok := planWriteback(w, r, d, at, req.Deletions)
		if !ok {
			return
		}

		// 审计先于响应体，与导出同一处置：一份计划描述的是平台打算往策略
		// 仓库里写什么（规范 §43）。写不进去就整次失败，不"记条日志照样把
		// 计划发出去"—— 事后没有任何东西能回答谁看过哪一份。
		if err := recordWriteback(r, d, planned.plan, ""); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, writebackPlanView{
			Plan:                planned.plan,
			RepoVerifyResult:    planned.repoResult,
			BindingVerifyResult: planned.pathResult,
		})
	}
}

// handlePolicyWritebackPush 把操作者确认过的那份计划推到一条新分支上。
//
// 三道门按顺序，缺一不可：
//
//  1. **没有指纹就不写。** 一个只是把地址打了一遍的调用方（或脚本）拿到的
//     必须是拒绝，而不是一次推送（design doc §5）。
//  2. **平台在写之前重新校验绑定、重新跑预测**（§4）。绑定上的 verified_at
//     是历史事实，不是当前状态；拿一个几天前的 OK 当作"现在可写"，正是
//     2026-08-13 spec §3.4 立下来要防的那件事。
//  3. **指纹必须与平台此刻重算出的那一份相等。** 对不上就拒绝 —— 操作者
//     确认的必须是他真正看过的那一份，不是"当时那一份的后续版本"。
//
// 推送成功后写 last_written_commit，与 PUSH_POLICY_WRITEBACK 审计行同事务
// （§8、§9）。
func handlePolicyWritebackPush(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req writebackPushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}
		// 第一道：指纹缺席就到此为止，**在读集群、发起任何出站之前**。
		// 判断放在最前面不是为了省事：它是本端点"缺省安全"这条性质本身，
		// 而排在后面就意味着某些前置失败会先于它被报出来，掩盖掉真正的原因。
		if strings.TrimSpace(req.Fingerprint) == "" {
			response.WriteInvalid(w, writebackNoFingerprintMsg)
			return
		}
		clusterID := chi.URLParam(r, "clusterID")
		at, ok := parseWritebackBranch(clusterID, req.Branch, time.Now().UTC())
		if !ok {
			response.WriteInvalid(w, writebackBranchMsg)
			return
		}

		// 第二道：重新校验、重新算。整份计划（文件内容、四类计数、提交信息）
		// 都在这里重新产出，请求体里没有任何一项参与。
		planned, ok := planWriteback(w, r, d, at, req.Deletions)
		if !ok {
			return
		}

		// 第三道：指纹。计划由平台重算，指纹由 registry.NewWritebackPlan 现算，
		// 请求体带来的那一串只做比对，不做输入。
		if planned.plan.Fingerprint != req.Fingerprint {
			response.WriteInvalid(w, writebackStaleMsg)
			return
		}

		if d.PolicyWriter == nil {
			// 没有装配写入器（未配置 secrets）时不是"随便推"，是"没有这条
			// 路径"。失败方向朝关（规范 §49）。
			d.Logger.Error("policy write-back push has no writer configured",
				"request_id", RequestIDFrom(r.Context()), "cluster", clusterID)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeDependencyUnavailable)
			return
		}
		commit, err := d.PolicyWriter.Push(r.Context(), planned.repo, planned.plan)
		if err != nil {
			writeGitWriteError(w, r, d, clusterID, err)
			return
		}

		if err := recordWriteback(r, d, planned.plan, commit); err != nil {
			// 提交已经在远端了，这次调用却记不下来。**不能报成功**：
			// last_written_commit 是漂移检测的基准，一次"推了但没记"之后，
			// 平台对"我最后交出去的是什么"的回答是错的。commit 进日志
			// （平台自己产出的十六进制，不是凭据），让人能手工补上。
			d.Logger.Error("policy write-back pushed but could not be recorded",
				"request_id", RequestIDFrom(r.Context()),
				"cluster", clusterID, "commit", commit, "err", err)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}
		response.WriteOK(w, writebackPushView{
			Branch: planned.plan.Branch, Commit: commit,
			Files: len(planned.plan.Files), Counts: planned.plan.Counts,
		})
	}
}

// planWriteback 是两个端点共用的前置：重新校验绑定、重新跑预测、产出计划。
//
// 出计划与推送走**同一段**，不是两段各自实现：指纹比对的前提是两次算出的
// 东西可比，而两条各自演化的产出路径会让"指纹对得上"退化成"这两段代码
// 今天还没分家"。轮 3 出过同形状的缺陷（导出自己走了一条生成路径），
// 四道门禁全绿。
//
// 出错时已经写好响应，返回 false，调用方直接返回即可。
func planWriteback(
	w http.ResponseWriter, r *http.Request, d Deps, at time.Time, confirmed []string,
) (plannedWriteback, bool) {
	clusterID := chi.URLParam(r, "clusterID")
	window, ok, err := parseWindow(r.Context(), r.URL.Query(), d.Reader, clusterID)
	if err != nil {
		writeReaderError(w, r, d, err)
		return plannedWriteback{}, false
	}
	if !ok {
		response.WriteBusiness(w, response.CodeInvalidParam)
		return plannedWriteback{}, false
	}
	// 筛选后的写回一律拒绝，理由与导出同源：预测恒按整集群跑，一份被裁剪
	// 的文件配上整集群口径的计数，描述的是另一套策略集，且朝着让人放心的
	// 方向偏。默默忽略这个参数更糟 —— 操作者以为自己只推了一个命名空间。
	if r.URL.Query().Get("namespace") != "" {
		response.WriteInvalid(w, writebackNamespaceMsg)
		return plannedWriteback{}, false
	}

	c, found, err := d.Registry.Cluster(r.Context(), clusterID)
	if err != nil {
		writeRegistryError(w, r, d, err)
		return plannedWriteback{}, false
	}
	// 集群不存在与集群没有绑定同码，与重校验端点同一处置：从调用方视角
	// 两者都是"要写的那个地方不在"。
	if !found || c.Git == nil {
		response.WriteBusiness(w, response.CodeNotFound)
		return plannedWriteback{}, false
	}
	repo, ok := bindingVerificationTarget(w, r, d, *c.Git)
	if !ok {
		return plannedWriteback{}, false
	}

	// 重新校验，两层都做：路径级以仓库级为前提（2026-08-13 design doc §3.3）。
	// 结论**不落库** —— 这次调用是写回，不是一次操作者发起的校验，顺手写下
	// VERIFY_GIT_BINDING 会让追溯的人读到一次没人做过的校验。
	repoResult, _ := verifyRepo(r.Context(), d, repo)
	pathResult, _ := verifyBindingPath(r.Context(), d, repo, repoResult, c.Git.PolicyPath)
	if repoResult != registry.RepoVerifyOK {
		// 仓库级不是 OK 就不写（design doc §4）。未配置校验器时这里是
		// NOT_VERIFIED —— 没做过的检查不是通过了的检查。
		response.WriteInvalid(w, writebackUnverifiedMsg(repoResult))
		return plannedWriteback{}, false
	}

	// 枚举一次仓库，拿到计划里那两份交人工处置的清单（design doc §2、§3）。
	// 排在预测之前：失败就整次不出计划，早一步失败省掉一次整集群预测。
	listing, ok := surveyRepo(w, r, d, repo, c.Git.PolicyPath)
	if !ok {
		return plannedWriteback{}, false
	}

	// 写回跟着当前粒度走，与导出同源：推进 Git 的必须是操作者确认过的那一份。
	// 粒度进指纹（计划由 registry.NewWritebackPlan 现算），因此换粒度会让
	// 旧指纹对不上 —— 那正是期望行为。
	pv, err := d.Reader.PolicyPreviewAtGranularity(r.Context(), clusterID, "", window,
		parseGranularity(r.URL.Query().Get("granularity")))
	if err != nil {
		writeReaderError(w, r, d, err)
		return plannedWriteback{}, false
	}
	if countEnabledRules(pv.Overridden.Enabled) == 0 {
		response.WriteInvalid(w, writebackEmptyMsg)
		return plannedWriteback{}, false
	}

	// Enforcing 门禁：Baseline 不齐备就不出计划，也就不可能推
	// （design doc 2026-08-18-enforcing-gate）。两个端点共用这一道门 ——
	// 出一份看起来齐备、确认之后必被拒的计划，是在训练人忽略它。
	//
	// 排在"没有可写内容"之后：一条规则都不推时没有任何 default-deny 落地，
	// 那句话更准确。
	if blockers := enforcingBlockers(pv); blockers != "" {
		response.WriteInvalid(w, blockers)
		return plannedWriteback{}, false
	}

	// 一致率门禁**先于渲染**：被排除的主体根本不该进入渲染，而不是渲染完
	// 再挑出来。渲染在后，files 与排除清单就必然一致 —— 两处各挑一次会多出
	// 一个可以互相分歧的位置。
	exclusions, ok := disagreementExclusions(r.Context(), w, r, d, clusterID, window)
	if !ok {
		return plannedWriteback{}, false
	}

	actor := actorOf(r)
	// **与导出同一段渲染**，不是第二次渲染（design doc §7）：写进 Git 的
	// 必须就是操作者能下载下来的那份内容。另起一段渲染，两份会慢慢分家，
	// 而分家之后没有任何东西看得出来。注释头里的账号与时刻换成写回的
	// 发起者与这次计划的时刻。
	files, held, err := writebackFiles(
		pv, strings.Trim(c.Git.PolicyPath, "/"), actor.Username, at, excludedSubjects(exclusions))
	if err != nil {
		d.Logger.Error("render policy write-back failed",
			"request_id", RequestIDFrom(r.Context()), "cluster", clusterID, "err", err)
		response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
		return plannedWriteback{}, false
	}

	// 全被排除时不出空计划：一份没有任何文件的计划推上去是一个空提交，
	// 而它在合并请求上读起来像"平台认为这个集群不需要策略"。
	if len(files) == 0 {
		response.WriteInvalid(w, disagreementAllExcludedMsg(exclusions))
		return plannedWriteback{}, false
	}

	counts := writebackCounts(pv)
	// 学习期门禁排在最前：观测还没覆盖一轮业务周期时，这套候选集描述的是
	// 「这一小段时间之外的一切都拦掉」（design doc 2026-08-25 §5）。
	if msg, ok := learningWindowBlockers(r.Context(), w, r, d, c); !ok {
		return plannedWriteback{}, false
	} else if msg != "" {
		response.WriteInvalid(w, msg)
		return plannedWriteback{}, false
	}

	// 冲突判定排在分类之前：一次会顶掉别人对象的写回，连计划都不该出
	// （design doc 2026-08-25 §4）。
	if msg, ok := conflictingObjects(r.Context(), d, clusterID, listing.Files, pv.Overridden.Enabled); !ok {
		writeReaderError(w, r, d, msg)
		return plannedWriteback{}, false
	} else if msg != nil {
		response.WriteInvalid(w, msg.Error())
		return plannedWriteback{}, false
	}

	// 多余文件逐个分类，可删的那一类带上删除影响（design doc 2026-08-24 §4）。
	// 排在计划构造之前：确认删除的判据取自这份清单，而清单必须由平台这一刻
	// 算出来，不是请求带来的。
	deletions, err := classifyDeletions(
		r.Context(), d, clusterID, pv.Window, listing.Files, files, held)
	if err != nil {
		writeReaderError(w, r, d, err)
		return plannedWriteback{}, false
	}

	plan, err := registry.NewWritebackPlan(*c.Git, registry.WritebackPlan{
		Files:     files,
		Deletions: deletions,
		Confirmed: confirmed,
		Branch:    writebackBranch(clusterID, at),
		CommitMessage: writebackCommitMessage(
			clusterID, actor.Username, pv, counts, deletions, confirmed, exclusions),
		Counts: counts,
		// 两份清单来自刚才那次枚举，不是留空：留空的那一份在界面上是一句
		// 断言，而不是一个空集（design doc §4）。
		Exclusions: exclusions,
		// 被排除主体的既有文件**不算多余**：它们不是残留，是这一轮平台不敢
		// 碰的东西。混进多余清单会让它们被分类成可删 —— 一次分歧最终把已经
		// 生效的策略删掉，方向完全反了。
		Extraneous:       extraneousFiles(listing.Files, files, held),
		ExistingBranches: listing.Branches,
	})
	if err != nil {
		// 构造函数是唯一一条能拿到指纹的路径，它同时判「每条路径都落在
		// policyPath 之内」。走到这里说明绑定本身或渲染出的落点不成立，
		// 那是一次业务级失败，不是一次可以绕过去的意外。
		writeRegistryError(w, r, d, err)
		return plannedWriteback{}, false
	}
	return plannedWriteback{repo: repo, plan: plan, repoResult: repoResult, pathResult: pathResult}, true
}

// surveyRepo 枚举一次策略仓库，拿到计划里那两份交人工处置的清单。
//
// 失败一律不出计划（design doc §4）：一份枚举失败、清单留空的计划，在界面上
// 读起来是"仓库里没有多余文件"，而那是一句没有人算过的断言 —— 失败方向必须
// 朝关。这与"重校验结论不是 OK 就不写"是同一条纪律。
//
// 错误不进响应也不进日志正文：go-git 的报错带着仓库路径、主机名与传输细节
// （规范 §19、§21、§22），而日志会被转发到 Cloud Logging。要定位是哪一次，
// 看 request_id。
//
// 出错时已经写好响应，返回 false。
func surveyRepo(
	w http.ResponseWriter, r *http.Request, d Deps, repo registry.GitRepo, policyPath string,
) (gitwrite.RepoListing, bool) {
	clusterID := chi.URLParam(r, "clusterID")
	if d.PolicyWriter == nil {
		// 没装配写回这条路径时同样不出计划：计划是推送的前一步，出一份推不出去
		// 的计划只会让操作者以为他离一次写回还差一次点击。
		d.Logger.Error("policy write-back plan has no writer configured",
			"request_id", RequestIDFrom(r.Context()), "cluster", clusterID)
		response.WriteSystem(w, http.StatusInternalServerError, response.CodeDependencyUnavailable)
		return gitwrite.RepoListing{}, false
	}
	listing, err := d.PolicyWriter.List(r.Context(), repo, policyPath)
	if err != nil {
		d.Logger.Error("policy write-back repository listing failed",
			"request_id", RequestIDFrom(r.Context()), "cluster", clusterID)
		response.WriteInvalid(w, writebackSurveyMsg)
		return gitwrite.RepoListing{}, false
	}
	return listing, true
}

// extraneousFiles 挑出仓库里已有、但本次候选集不包含的文件（design doc §3）。
//
// 平台从不删除它们，只列出来交人工处置：判断一个文件"多余"需要知道集群现在
// 真实跑着什么，平台今天看不到 —— 所以这份清单是给人看的，不是给平台执行的。
//
// 逐条按路径判等，不做任何归一化：枚举出的路径与计划里的路径都由平台产出，
// 一个"顺手规范一下"的比较会让两条不同的路径被当成同一条，而那正是让一个
// 真正多余的文件从清单里消失的方向。
func extraneousFiles(
	existing []gitwrite.RepoFile, files []registry.WritebackFile, held []string,
) []string {
	planned := plannedPaths(files, held)
	var extraneous []string
	for _, p := range existing {
		if _, ok := planned[p.Path]; !ok {
			extraneous = append(extraneous, p.Path)
		}
	}
	return extraneous
}

// plannedPaths 是"这一轮平台不该当成多余的"那些落点。
//
// 它是两部分：真的要写的文件，**加上被排除主体的落点**。后者上一轮可能已经
// 由平台写过，这一轮因为分歧超阈没有重写 —— 如果把它算成多余，它会被分类
// 成可删，于是一次分歧最终把一份已经生效的策略删掉。方向完全反了：分歧说明
// 的是"平台在这个主体上看不准"，那时最不该做的就是替它做减法。
func plannedPaths(files []registry.WritebackFile, held []string) map[string]struct{} {
	planned := make(map[string]struct{}, len(files)+len(held))
	for _, f := range files {
		planned[f.Path] = struct{}{}
	}
	for _, p := range held {
		planned[p] = struct{}{}
	}
	return planned
}

// classifyDeletions 给每一个多余文件一个处置结论（design doc 2026-08-24 §4.2）。
//
// 四类的判据各自独立，且**失败方向一律朝"不提供删除"**：读不全的、解析不了的、
// 影响算不出来的，都停在"只报存在"。一次没有依据的删除会把那一片从有规则
// 变回默认放行，而它不报错。
func classifyDeletions(
	ctx context.Context, d Deps, clusterID string, window store.TimeWindow,
	existing []gitwrite.RepoFile, files []registry.WritebackFile, held []string,
) ([]registry.WritebackDeletion, error) {
	// held 与 files 同等对待：被排除主体的既有文件不是残留（plannedPaths）。
	planned := plannedPaths(files, held)

	out := []registry.WritebackDeletion{}
	for _, file := range existing {
		if _, ours := planned[file.Path]; ours {
			continue
		}
		// 读不全的文件与解析不了的文件同一处置：平台没看懂的东西，它对集群
		// 的作用平台也算不出来。
		if file.Oversize {
			out = append(out, registry.WritebackDeletion{
				Path: file.Path, Class: registry.DeletionUnparseable,
			})
			continue
		}
		policies, err := parseNetworkPolicies(file.Content)
		if err != nil || len(policies) == 0 {
			out = append(out, registry.WritebackDeletion{
				Path: file.Path, Class: registry.DeletionUnparseable,
			})
			continue
		}

		impact, err := d.Reader.DeletionImpact(ctx, clusterID, window, policies)
		if err != nil {
			return nil, err
		}
		switch {
		case impact.Live == 0:
			// 仓库里有、集群里没有。删掉它对集群没有影响 —— 但仍然要人确认，
			// 因为"集群里没有"依据的是采集结果，而一次采集故障与"这个对象
			// 真的不存在"在数据里长得一样。
			out = append(out, registry.WritebackDeletion{
				Path: file.Path, Class: registry.DeletionNotApplied,
				Documents: len(policies),
			})
		case !impact.TrafficObserved:
			// 窗口里没有观测，四类计数全是 0，而那个 0 是没评估过。
			// 不给删除入口，也不给一个让人放心的零。
			out = append(out, registry.WritebackDeletion{
				Path: file.Path, Class: registry.DeletionImpactUnknown,
				Documents: len(policies),
			})
		case impact.InWindow < impact.Live:
			// 它现在在集群里，但观测窗口那一刻还不在（典型情形：这一批策略
			// 是在窗口之后才由 GitOps 下发的）。此时重放算出来的"删除影响"
			// 描述的是删掉一个当时并不存在的东西，结果恒为无变化 —— 一个
			// 朝让人放心的方向错的数字。**不给删除入口，说出"我算不出来"**
			// （2026-08-24 实测发现）。
			out = append(out, registry.WritebackDeletion{
				Path: file.Path, Class: registry.DeletionImpactUnknown,
				Documents: len(policies),
			})
		default:
			out = append(out, registry.WritebackDeletion{
				Path: file.Path, Class: registry.DeletionDeletable,
				Documents: len(policies), Counts: impact.Counts,
			})
		}
	}
	return out, nil
}

// parseNetworkPolicies 把一份 YAML 文档流解析成 NetworkPolicy 集合。
//
// 任何一份文档解析不了、或者不是 NetworkPolicy，整个文件即判为看不懂 ——
// 不做"能解出几份算几份"：一个混着别的东西的文件，删掉它的后果超出平台
// 算得出来的那部分。
func parseNetworkPolicies(content string) ([]networkingv1.NetworkPolicy, error) {
	var out []networkingv1.NetworkPolicy
	for _, doc := range strings.Split(content, "\n---") {
		if strings.TrimSpace(stripYAMLComments(doc)) == "" {
			continue
		}
		var p networkingv1.NetworkPolicy
		if err := yaml.UnmarshalStrict([]byte(doc), &p); err != nil {
			return nil, err
		}
		if p.Kind != policyExportKind || p.Name == "" || p.Namespace == "" {
			return nil, fmt.Errorf("not a NetworkPolicy document")
		}
		out = append(out, p)
	}
	return out, nil
}

// stripYAMLComments 去掉整行注释，用来判断一个分段是不是只有注释。
//
// 写回文件自己的注释头就是一整段注释，把它当成一份解析失败的文档会让平台
// 自己写出去的文件永远被判成"看不懂"。
func stripYAMLComments(doc string) string {
	var b strings.Builder
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// writebackCounts 取写回这一刻重算出的四类计数。
//
// 取 Overridden 那一套，与文件同源：文件渲染的正是应用人工决定之后的候选集，
// 配上默认推荐的数字就是轮 3 那条 Critical 的形状 —— 操作者推出去的是一份
// 文件、看过的是另一套数字，而屏幕上没有任何迹象。
//
// 按 AllChangeKinds 逐个取而不是把 map 原样交出去：
// registry.NewWritebackPlan 要求四类齐全，而"缺一类"不是"那一类为零"，
// 是"没算过那一类"，正是写前重算的全部意义所在。
func writebackCounts(pv store.PolicyPreview) map[predict.ChangeKind]int {
	counts := make(map[predict.ChangeKind]int, len(predict.AllChangeKinds()))
	for _, k := range predict.AllChangeKinds() {
		// **取并入已有策略的那一份**（design doc 2026-08-25-existing-policies §3）。
		//
		// 这几个数字会进提交信息，而那是合并请求上的评审人唯一会读的一段
		// 文字。平台的下发方式是只加不删：合并之后集群里是"已有 ∪ 候选"，
		// 因此真实影响必须按那一份算。只跑候选集的那一份会把旧策略额外
		// 放行的部分算成"会被拦断"，让一次实际无害的写回看起来要断几十条
		// 连接 —— 而反复出现的假警报，最终会让真的那次也没人看。
		counts[k] = pv.OverriddenPredictionWithExisting().Counts[k]
	}
	return counts
}

// writebackCommitMessage 拼出这次写回的提交信息。
//
// 在这一层拼、而且拼完就进计划（因此进指纹）：集群、时间窗、发起者与
// 重算后的计数只有这里同时拿得到，而它是合并请求上的评审人**唯一会读的
// 那句话**（design doc §7）。留给写入端现场拼，落进仓库历史的就会是一段
// 没有人批准过的文字。
//
// **每一个取值要么是调用方本来就知道的（集群、他自己的账号名、时间窗），
// 要么是本次判定的产物（四类计数、平台版本）。不含凭据、host key，也不含
// 任何内部地址** —— 仓库地址、凭据引用与 SSH host key 在这个函数里根本
// 拿不到，这是结构上的，不是靠这里记得不写。
//
// 取值逐个过 headerSafe：集群 ID 来自 URL、账号名来自会话，带换行的取值会
// 让提交信息裂成看起来像另外几行的东西（规范 §26 的同一形状）。上游已经
// 拒过不存在的集群与账号，这一层是纵深防御，不是它的替代。
func writebackCommitMessage(
	clusterID, actor string, pv store.PolicyPreview, counts map[predict.ChangeKind]int,
	deletions []registry.WritebackDeletion, confirmed []string,
	exclusions []registry.WritebackExclusion,
) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		b.WriteString(headerSafe(fmt.Sprintf(format, args...)) + "\n")
	}
	line("policy: distill 写回 %s", clusterID)
	b.WriteString("\n")
	line("集群: %s", clusterID)
	// 命名空间筛选进提交信息：一次只覆盖某一个命名空间的写回，与一次覆盖
	// 整集群的写回，在 diff 上分辨不出来（没被涉及的命名空间只是没有文件）。
	// 评审人要判断的第一件事就是这次动了多大范围。
	line("命名空间筛选: %s", namespaceLabel(pv.Namespace))
	renderPolicyBasis(pv, counts, line)
	line("发起者: %s", actor)
	line("平台版本: %s", buildinfo.Version())
	line("以上 dry-run 结论算的是出计划那一刻的集群状态，合并前请重新核对。")

	// 缺口与导出文件用同一个实现：评审人在合并请求上读到的、与下载下来
	// 那份文件上写的，必须是同一段话。分开写就会漂移，而漂移了屏幕上不会
	// 有任何迹象。
	b.WriteString("\n")
	renderPolicyCaveats(pv, line)

	// 排除逐条列出：一份少了三个 workload 的策略集，不说明的话评审人读到的
	// 就是"这个集群只有这些 workload"—— 而缺席恰恰意味着平台在那几个主体上
	// 看不准，是他最该知道的一件事。
	if len(exclusions) > 0 {
		b.WriteString("\n")
		line("本次排除 %d 个主体（平台判定与集群实际执行分歧超阈，方向为会造成阻断的那一侧）：",
			len(exclusions))
		for _, e := range exclusions {
			line("  %s（分歧 %.0f%%）", e.Label(), e.UnderPermissiveRate*100)
		}
		line("这些主体的既有策略文件保持不动，本次既不更新也不删除。")
	}

	// 删除逐条列出，与新增分段（design doc 2026-08-24 §4.5）。评审人在合并
	// 请求上唯一会读的就是这段文字 —— 删除只体现在 diff 里，等于让一次"把
	// 某一片策略从集群里撤掉"的变更在他读到的那段话里完全不存在。
	if len(confirmed) == 0 {
		return b.String()
	}
	impact := make(map[string]registry.WritebackDeletion, len(deletions))
	for _, d := range deletions {
		impact[d.Path] = d
	}
	paths := make([]string, len(confirmed))
	copy(paths, confirmed)
	sort.Strings(paths)
	b.WriteString("\n")
	line("本次删除 %d 个文件：", len(paths))
	for _, path := range paths {
		d := impact[path]
		switch {
		case d.Class == registry.DeletionNotApplied:
			line("  %s（%d 份策略；仓库里有、集群里没有，删除对集群无影响）",
				path, d.Documents)
		case d.Counts != nil:
			line("  %s（%d 份策略；删除影响 WOULD_BREAK %d / WOULD_OPEN %d）",
				path, d.Documents,
				d.Counts[predict.ChangeWouldBreak], d.Counts[predict.ChangeWouldOpen])
		default:
			line("  %s（%d 份策略）", path, d.Documents)
		}
	}
	return b.String()
}

// writebackBranch 拼出目标分支名 `distill/<clusterID>-<UTC 时间戳>`
// （design doc §2）。
//
// 永远是一条新建分支，绝不是绑定里配置的那条：那条是 Config Sync 应用的
// 对象，推它等于界面上点一下就是一次生产变更。这里只负责拼，真正挡住
// "推了部署分支"的是 gitwrite.targetRef —— 守卫必须紧贴发起写的那一步。
func writebackBranch(clusterID string, at time.Time) string {
	return writebackBranchPrefix + branchSafe(clusterID) + "-" + at.UTC().Format(writebackStampLayout)
}

// parseWritebackBranch 从请求带回的分支名里解出这份计划的时刻。
//
// 分支名进指纹（registry.FingerprintOf），因此推送时重算的那份计划必须用
// **同一条**分支名，否则指纹必然对不上；而分支名里的时间戳同时是写回文件
// 注释头里的那个时刻，两者必须同源，不能各自取"现在"。让分支名一个字段
// 同时承担这两件事，请求体里就只有它一个可回带的值。
//
// 校验方式是**拿解析出的时刻重新拼一遍、要求逐字节相等**，而不是逐段挑
// 字符：写法必须唯一，判等才有意义（与 registry.cleanRepoPath 同一条理由）。
// 集群 ID 也因此被钉死 —— 一条属于别的集群的分支名在这里就落不进来。
//
// 未来时刻一律拒绝：一个声称自己来自明天的分支与注释头，说的是一件没有
// 发生过的事，而它会永久留在仓库里。
func parseWritebackBranch(clusterID, branch string, now time.Time) (time.Time, bool) {
	prefix := writebackBranchPrefix + branchSafe(clusterID) + "-"
	if !strings.HasPrefix(branch, prefix) {
		return time.Time{}, false
	}
	at, err := time.Parse(writebackStampLayout, strings.TrimPrefix(branch, prefix))
	if err != nil {
		return time.Time{}, false
	}
	if at.After(now) {
		return time.Time{}, false
	}
	if writebackBranch(clusterID, at) != branch {
		return time.Time{}, false
	}
	return at, true
}

// branchSafe 把一个标识符收成可以安全嵌进 Git 引用名的形式。
//
// 与 exportFilename 里那份过滤不共用，因为规则不同：文件名允许点号，而
// 引用名里的点号会撞上 Git 自己的规则（`..`、结尾的 `.lock`、以点开头的
// 段）。合成一个"通用"的过滤，两边就得各自迁就对方，而迁就的方向总是更松。
//
// 逐字符白名单而不是黑名单：clusterID 来自 URL 路径，而引用名里出现空格、
// `~`、`^`、`:`、`?`、`*`、`[` 或控制字符都会让远端把它当成别的东西
// （规范 §26）。全部被过滤掉时退回一个固定名字，而不是产出 `distill/-<戳>`。
func branchSafe(s string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	if strings.Trim(safe, "-") == "" {
		return "cluster"
	}
	return safe
}

// recordWriteback 写这次写回的审计行。commit 为空表示这是一次出计划。
//
// **两个动作分开**（design doc §9）：出计划是常规动作，推送是一次仓库变更。
// 混成一条会让"谁真的改了仓库"淹没在计划请求里 —— 而事故复盘时要问的
// 恰恰是前者。分开这件事由 WritebackStore 的两个方法在类型上保证：这里
// 走哪一个由 commit 是否为空决定，不存在"两个动作共用一个动作名"的写法。
//
// 推送那一支同时写 last_written_commit，与审计同事务（§8）。
func recordWriteback(r *http.Request, d Deps, plan registry.WritebackPlan, commit string) error {
	if d.Writeback == nil {
		// 没有审计去处就不写：一次留不下痕迹的写回，事后没有任何东西能
		// 回答"谁把它推了出去"（规范 §43）。
		return errors.New("httpapi: no writeback store configured")
	}
	audit := registry.Writeback{
		Branch: plan.Branch, Files: len(plan.Files), Counts: plan.Counts,
	}
	clusterID := chi.URLParam(r, "clusterID")
	if commit == "" {
		return d.Writeback.RecordWritebackPlan(r.Context(), actorOf(r), clusterID, audit)
	}
	return d.Writeback.SetLastWrittenCommit(r.Context(), actorOf(r), clusterID, commit, audit)
}

// writeGitWriteError 把 gitwrite 的失败收敛成封闭的业务码。
//
// **default 分支既不回传错误，也不把它写进日志。** 走到那里的是 go-git、
// SSH 传输与凭据解析的原始报错，文本里带着仓库路径、主机名、端口，有时还
// 带着凭据引用（规范 §19、§21、§22）—— 而日志会被转发到 Cloud Logging，
// 和响应体一样是一条出境通道。要定位是哪一次，看 request_id。
//
// 已登记的哨兵各自映射成一句操作者能据以行动的话：这几种失败都不是服务
// 故障，而是"这次不能推，原因是这个"，走 500 会让界面只剩一句"服务内部
// 错误"，操作者只会重试，而重试永远得到同一个结果。
func writeGitWriteError(w http.ResponseWriter, r *http.Request, d Deps, clusterID string, err error) {
	switch {
	case errors.Is(err, gitwrite.ErrNothingToPush):
		response.WriteInvalid(w, "目标分支上的内容与这份计划逐字节相同，没有需要推送的变更，因此不建分支、不提交。")
	case errors.Is(err, gitwrite.ErrTargetBranchExists):
		response.WriteInvalid(w, "目标分支在远端已经存在。平台不覆盖任何已有分支，请重新出一次计划。")
	case errors.Is(err, gitwrite.ErrTargetIsDeployBranch):
		response.WriteInvalid(w, "目标分支就是绑定里配置的那条部署分支，平台永不推它。请检查该仓库的分支配置。")
	case errors.Is(err, gitwrite.ErrInvalidTargetBranch):
		response.WriteInvalid(w, writebackBranchMsg)
	case errors.Is(err, gitwrite.ErrPlanRepoMismatch):
		response.WriteInvalid(w, "这份计划批准的是另一个仓库，平台不会把它写到别处。"+
			"绑定在你确认之后被改过，请重新出一次计划。")
	case errors.Is(err, gitwrite.ErrNoCommitMessage):
		response.WriteInvalid(w, "这份计划没有提交信息，因此没有可供评审的依据。请重新出一次计划。")
	default:
		d.Logger.Error("policy write-back push failed",
			"request_id", RequestIDFrom(r.Context()), "cluster", clusterID)
		response.WriteSystem(w, http.StatusInternalServerError, response.CodeDependencyUnavailable)
	}
}

// writebackFiles 把一份候选集切成「一个命名空间一个目录、一个主体一个方向
// 一个文件」（design doc 2026-08-24 §3.1、§3.6）。
//
// 落点是 <policyPath>/distill/<namespace>/<workload>-<方向>.yaml，每个文件恰好
// 一份 NetworkPolicy。文件名直接由对象名去掉 candidate- 前缀得到：两者由同一
// 个名字推出，就不会出现「文件说这是 api 的入站、里面装的是别人的出站」。
//
// 按路径排序输出，不按候选集的遍历顺序：文件清单进指纹，而一个随遍历顺序
// 变化的清单会让同一份内容算出两个指纹 —— 表现为操作者确认过的计划在推送时
// "莫名其妙"作废。
//
// 渲染仍然走 renderPolicyDocs 那一段，与导出同源（2026-08-14 design doc §7）。
func writebackFiles(
	pv store.PolicyPreview, root, actor string, at time.Time, excluded map[string]bool,
) (files []registry.WritebackFile, held []string, err error) {
	files = make([]registry.WritebackFile, 0, len(pv.Overridden.Enabled))
	for _, p := range pv.Overridden.Enabled {
		target := path.Join(root, writebackDirName, p.Namespace, writebackFileName(p.Name))

		// 被排除的主体只记下落点，不渲染内容：那个落点要交给多余文件判定
		// 做豁免（它上一轮可能已经写过），而内容渲染出来没有任何人会用。
		if isExcluded(p, excluded) {
			held = append(held, target)
			continue
		}

		body, renderErr := renderPolicyDocs(pv, []networkingv1.NetworkPolicy{p}, p.Namespace, actor, at)
		if renderErr != nil {
			return nil, nil, renderErr
		}
		files = append(files, registry.WritebackFile{Path: target, Content: string(body)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Strings(held)
	return files, held, nil
}

// isExcluded 判断一份策略对象的主体是否在排除清单里。
//
// 主体从 podSelector 取，用的是 policygen 那一个定义（WorkloadOf）——
// 「什么算一个 workload」只能有一处说法，生成、对账与这里必须一致，否则
// 排除会挂到一个与对账里那个主体不同的东西上，而两边都不会报错。
//
// **取不到 workload 时按 namespace 判**：namespace 粒度下候选是折叠过的，
// podSelector 里没有 workload 标签，而那一层的排除本来就是整个 namespace。
func isExcluded(p networkingv1.NetworkPolicy, excluded map[string]bool) bool {
	if excluded[p.Namespace] {
		return true
	}
	_, workload, ok := policygen.WorkloadOf(p.Spec.PodSelector.MatchLabels)
	return ok && excluded[p.Namespace+"/"+workload]
}

// disagreementAllExcludedMsg 是全部主体都被排除时的拒绝文案。
func disagreementAllExcludedMsg(ex []registry.WritebackExclusion) string {
	labels := make([]string, len(ex))
	for i, e := range ex {
		labels[i] = fmt.Sprintf("%s（%.0f%%）", e.Label(), e.UnderPermissiveRate*100)
	}
	return fmt.Sprintf(
		"这个集群上每一个主体的平台判定都与集群实际执行对不上，且分歧落在会造成阻断的"+
			"那个方向（平台判 DENY、集群实际放行）：%s。全部被排除之后没有任何内容可写。"+
			"这多半不是某几个 workload 的问题，而是平台漏解释了一整个策略平面 —— "+
			"先去 /quality 看分歧明细。",
		strings.Join(labels, "、"))
}

// writebackFileName 由策略对象名推出文件名。
//
// 去掉 candidate- 前缀：那三个字是平台内部的命名习惯，落在仓库里只是每个
// 文件名都带的噪声，而目录本身（distill/）已经说明了它们的出处。
func writebackFileName(policyName string) string {
	return strings.TrimPrefix(policyName, "candidate-") + ".yaml"
}

// conflictingObjects 判断这次要写的对象里，有没有哪一个的名字已经被**别人**
// 占了（design doc 2026-08-25 §4）。
//
// 判据是三方比对，不是"集群里有同名对象就拒"：
//
//	要写的       —— 本次候选集渲染出的对象
//	仓库已声明的 —— policyPath 下现有文件里声明的对象（包括平台上一轮写的）
//	集群里有的   —— 最近一次采集看到的对象
//
// 冲突 = 要写的 ∧ 集群里有 ∧ 仓库没声明过。少了第三项，平台自己上一轮写出去、
// 已经由 GitOps 落进集群的那些对象会被判成冲突，于是第二次写回永远出不了计划。
//
// 三个返回值的形状：第二个为 false 表示读集群失败（调用方按读错误处理，
// 因为那时无法判断有没有冲突 —— 而"判不出来就放行"正是这道门要拦的）；
// 第一个非 nil 表示确实冲突，文案里点名对象。
func conflictingObjects(
	ctx context.Context, d Deps, clusterID string,
	existing []gitwrite.RepoFile, writing []networkingv1.NetworkPolicy,
) (error, bool) {
	live, err := d.Reader.LivePolicies(ctx, clusterID)
	if err != nil {
		return err, false
	}
	inCluster := make(map[string]struct{}, len(live))
	for _, p := range live {
		inCluster[p.Namespace+"/"+p.Name] = struct{}{}
	}

	declared := map[string]struct{}{}
	for _, f := range existing {
		if f.Oversize {
			continue
		}
		policies, err := parseNetworkPolicies(f.Content)
		if err != nil {
			// 解析不了的文件在删除流程里是 UNPARSEABLE。这里同样跳过：
			// 它声明了什么平台并不知道，不能拿它去证明某个名字是自己的。
			continue
		}
		for _, p := range policies {
			declared[p.Namespace+"/"+p.Name] = struct{}{}
		}
	}

	var clashes []string
	for _, p := range writing {
		key := p.Namespace + "/" + p.Name
		if _, taken := inCluster[key]; !taken {
			continue
		}
		if _, ours := declared[key]; ours {
			continue
		}
		clashes = append(clashes, key)
	}
	if len(clashes) == 0 {
		return nil, true
	}
	sort.Strings(clashes)
	return fmt.Errorf(
		"集群里已经有这些对象，而策略仓库里没有任何文件声明过它们 —— 它们不是平台写的：%s。"+
			"写回会把它们**覆盖**掉，而 Git 历史里只看得到平台写了一个文件。"+
			"请先确认这些对象归谁管：要么把它们纳入本仓库的 distill/ 子树，"+
			"要么给平台生成的对象换一个不冲突的名字。",
		strings.Join(clashes, "、")), true
}

// maxUnderPermissiveRate 是一个 workload 允许的「平台低估放行面」上限。
//
// 5%：二十条判定里有一条"平台以为不通、集群里实际通着"，就不该拿这个
// workload 的推荐去下发 —— 那一类是唯一能绕过 dry-run 造成阻断的分歧
// （design doc 2026-08-25 §3.3）。
//
// **默认值必须存在。** 一个没有默认阈值、等着人去配的门禁，在配好之前
// 等于不存在，而"等着配"的状态会一直持续到第一次事故。
const maxUnderPermissiveRate = 0.05

// disagreementBlockers 找出一致率不过关的 workload。
//
// 第二个返回值为 false 表示读对账失败（响应已写好）：**判不出来时不放行**，
// 与其余几道门禁同一方向。判不出来就放行，等于让一次读失败变成一次批准。
//
// 来源根本不报判定时（NODE_CONNTRACK 接入、合成数据集）整份报告只有
// SOURCE_SILENT，此时不拦 —— 那不是"一致率低"，是"这条接入方式对不了账"。
// 把它们一并锁死会让门禁在最需要人看的地方变成一句"反正都过不去"。
func disagreementExclusions(
	ctx context.Context, w http.ResponseWriter, r *http.Request, d Deps,
	clusterID string, window store.TimeWindow,
) ([]registry.WritebackExclusion, bool) {
	rec, err := d.Reader.Reconciliation(ctx, clusterID, window)
	if err != nil {
		writeReaderError(w, r, d, err)
		return nil, false
	}
	// 执行面不报判定时无从比对。**不排除任何主体**，而不是排除全部：
	// "对不上"与"没得比"是两件事，后者由完整度与学习期那两道门管。
	if !rec.SourceReportsVerdicts {
		return nil, true
	}

	var out []registry.WritebackExclusion
	for _, s := range rec.Report.BySubject {
		rate, ok := s.Counts.UnderPermissiveRate()
		// ok 为 false 表示这个主体上根本没有可比的样本，不是"分歧为零"。
		// 没有样本不构成排除理由 —— 那会把一个刚上线、还没有流量的 workload
		// 判成不可信。
		if !ok || rate <= maxUnderPermissiveRate {
			continue
		}
		out = append(out, registry.WritebackExclusion{
			Namespace:           s.Subject.Namespace,
			Workload:            s.Subject.Workload,
			UnderPermissiveRate: rate,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Workload < out[j].Workload
	})
	return out, true
}

// excludedSubjects 把排除清单摊成一个便于查的集合。
//
// 两个键：`namespace` 与 `namespace/workload`。namespace 粒度下候选是折叠过
// 的，只排除其中一个 workload 无从表达 —— 那一层上任一 workload 超阈就整个
// namespace 一起排除，保守方向。
func excludedSubjects(ex []registry.WritebackExclusion) map[string]bool {
	out := make(map[string]bool, len(ex)*2)
	for _, e := range ex {
		out[e.Namespace] = true
		if e.Workload != "" {
			out[e.Namespace+"/"+e.Workload] = true
		}
	}
	return out
}

// learningWindowBlockers 判断观测有没有覆盖这个集群声明的业务周期
// （design doc 2026-08-25 §5）。
//
// **没有登记业务周期时同样拒绝。** 「不知道这个集群多久能看全一轮」比
// 「知道它是七天」更危险：前者连"要不要等"这个问题都没有人回答过。默认放行
// 会让这道门禁在最需要它的集群上不存在。
//
// 第二个返回值为 false 表示读失败（响应已写好）：判不出来不放行，与其余
// 几道门禁同一方向。
func learningWindowBlockers(
	ctx context.Context, w http.ResponseWriter, r *http.Request, d Deps, c registry.Cluster,
) (string, bool) {
	if c.BusinessCycle <= 0 {
		return "这个集群还没有登记业务周期 —— 也就是「多久能看全一轮流量」。" +
			"候选策略只学观测窗口里见过的连接：月结批处理、季度对账、只在故障时走的" +
			"灾备链路，不在窗口里就不会有规则，而 dry-run 也看不出来（它只评估见过的连接）。" +
			"请在集群登记里填上业务周期与判断依据，再回来出计划。", true
	}
	if d.FlowIngest == nil {
		// 没有摄入读取端就答不出"从什么时候开始观测"。答不出不放行。
		return "本部署没有流量摄入读取端，无法判断观测是否已经覆盖一轮业务周期。", true
	}
	// **拿覆盖比，不拿跨度比。** 一个 90 天前摄入过一次、之后停了 89 天的
	// 集群，跨度够而观测只有两分钟 —— 而它恰恰是最不该放行的那一类。
	span, covered, ok, err := d.FlowIngest.ObservedCoverage(ctx, c.ID)
	if err != nil {
		writeReaderError(w, r, d, err)
		return "", false
	}
	if !ok {
		return "这个集群还没有过一次成功的流量摄入，观测覆盖为零。", true
	}
	if covered >= c.BusinessCycle {
		return "", true
	}
	// 说清**还差多久**：一句"观测不足"会让操作者不知道该等还是该改登记。
	msg := fmt.Sprintf(
		"观测还没覆盖这个集群声明的业务周期：实际观测到 %s，声明的周期是 %s，还差 %s。"+
			"登记里写的理由是「%s」。窗口之外的流量不在候选集里，也不在 dry-run 的"+
			"四类计数里 —— 现在下发，等于把那一段没看过的流量一并拦掉。"+
			"要么等观测补齐，要么改登记并写下新的理由。",
		humanDuration(covered), humanDuration(c.BusinessCycle),
		humanDuration(c.BusinessCycle-covered), c.BusinessCycleReason)

	// 跨度远大于覆盖时点出来：这不是"再等等就好"，是采集**断过**。
	// 只报"还差 6 天"会让操作者去等，而等下去并不会补上中间那一段 ——
	// 要修的是采集链路。两种处置完全不同，因此必须分开说。
	if span >= 2*covered {
		msg += fmt.Sprintf(
			" 另外：最早与最晚一次摄入之间跨了 %s，而其中只有 %s 真的收到了流量 ——"+
				"中间存在没有任何摄入的时段。先查采集链路，光等不会把那一段补回来。",
			humanDuration(span), humanDuration(covered))
	}
	return msg, true
}

// humanDuration 把时长写成人读得懂的样子。
//
// **不直接用 time.Duration.String()**：七天在那里是 "168h0m0s"，而登记里
// 写的是"七天"。一个要读者自己心算的数字，会让他放弃核对 —— 而这条文案
// 的全部意义就是让他核对"还差多久"。
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 24*time.Hour {
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		if hours == 0 {
			return fmt.Sprintf("%d 天", days)
		}
		return fmt.Sprintf("%d 天 %d 小时", days, hours)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d 分", int(d.Minutes()))
}
