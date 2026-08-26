package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// driftStatus 是漂移检测结论的响应形状。
//
// 两个字段回答两个不同的问题，**不合并**（design doc 2026-08-25 §5）：
//
//	DriftResult        仓库 vs 平台最后写过的 commit —— 「仓库被别人改过吗」
//	ClusterDriftResult 集群 vs 仓库 —— 「GitOps 到底有没有把它落下去」
//
// 合并之后，一个「仓库没被动过、但 controller 三天没同步」的集群会显示成
// 一切正常，而那正是这个平台最该报出来的那种状态。
type driftStatus struct {
	DriftResult        registry.DriftResult `json:"driftResult"`
	ClusterDriftResult ClusterDrift         `json:"clusterDriftResult"`
}

// ClusterDrift 是「集群里跑着的」与「仓库里声明的」之间的关系。
type ClusterDrift string

const (
	// ClusterDriftConverged 表示仓库声明的平台对象在集群里都有。
	ClusterDriftConverged ClusterDrift = "CONVERGED"
	// ClusterDriftPending 表示仓库里有、集群里还没有：controller 还没同步，
	// 或者同步失败了。
	ClusterDriftPending ClusterDrift = "PENDING"
	// ClusterDriftClusterAhead 表示集群里有平台的对象、仓库里却没有：
	// 有人手工 apply 过，或者仓库被回退了而集群没跟上。
	ClusterDriftClusterAhead ClusterDrift = "CLUSTER_AHEAD"
	// ClusterDriftUnknown 表示采集数据不足以判断。
	//
	// **不是 CONVERGED。** 一个从没被采过的集群与一个真的收敛了的集群，
	// 在数据里长得一样，而后者会让人以为下发已经生效。
	ClusterDriftUnknown ClusterDrift = "UNKNOWN"
)

// clusterDriftOf 比对两份对象名清单，给出集群漂移结论。
//
// **只看平台自己的对象**（candidate- 前缀）：策略目录下别人放的东西不归平台
// 管，把它读成「集群领先」会让每一次检测都报异常，而报多了就没人看了。
//
// 纯函数、只吃两份清单：判定逻辑要能被逐个分支测到，而两个数据源（仓库枚举
// 与集群采集）各自的失败处置属于调用方。
func clusterDriftOf(inRepo, inCluster []string) ClusterDrift {
	repo := make(map[string]struct{}, len(inRepo))
	for _, k := range inRepo {
		if strings.Contains(k, "/"+candidatePrefix) {
			repo[k] = struct{}{}
		}
	}
	live := make(map[string]struct{}, len(inCluster))
	for _, k := range inCluster {
		if strings.Contains(k, "/"+candidatePrefix) {
			live[k] = struct{}{}
		}
	}

	for k := range repo {
		if _, ok := live[k]; !ok {
			// 仓库里有、集群里没有。这一条优先于 CLUSTER_AHEAD：
			// "该落的没落下去"比"多了个不该有的"更该先被处理。
			return ClusterDriftPending
		}
	}
	for k := range live {
		if _, ok := repo[k]; !ok {
			return ClusterDriftClusterAhead
		}
	}
	return ClusterDriftConverged
}

// candidatePrefix 是平台生成的策略对象名的前缀。
//
// 与 policygen 那边同一个约定。判定"这个对象归不归平台管"只认它 ——
// design doc 2026-08-25 §4 把它定为保留前缀，写回会拒绝覆盖任何一个
// 带这个前缀、却不由本仓库声明的对象。
const candidatePrefix = "candidate-"

// handleGitBindingDrift 报告写进去的那份策略现在还在不在。
//
// **GET 而非 POST**：与 verify 那条相反，这次调用只读 —— 不写仓库、不改绑定、
// 不动锚点，也不落任何结论（design doc 2026-08-18-drift-detection §4）。
// 重放它没有副作用，因此它是一次读取。
//
// 结论不落库：它是「此刻问一次」的答案，存下来就会有人读到一个过期的
// IN_SYNC。要留痕的是操作者据此做的动作（重推），而那条本来就有审计。
//
// **未配置校验器时答 UNKNOWN**，由 settingsGitVerifier 给出 —— 这一层不自己
// 判 nil，那会让"没去看过"这个结论有两处定义。
func handleGitBindingDrift(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "clusterID")
		c, found, err := d.Registry.Cluster(r.Context(), id)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 集群不存在与集群没有绑定同码，与 handleVerifyGitBinding 一致：
		// 从调用方视角两者都是「要检测的那个东西不在」。
		if !found || c.Git == nil {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		if d.GitVerifier == nil {
			// 没有校验器的部署没去看过仓库。答 UNKNOWN 而不是 IN_SYNC ——
			// 后者会让操作者以为下发的东西还在（安全规范 §49）。
			response.WriteOK(w, driftStatus{DriftResult: registry.DriftUnknown})
			return
		}
		repo, ok := bindingVerificationTarget(w, r, d, *c.Git)
		if !ok {
			return
		}
		result := d.GitVerifier.Drift(r.Context(), repo, c.Git.PolicyPath, c.Git.LastWrittenCommit)
		response.WriteOK(w, driftStatus{
			DriftResult:        result,
			ClusterDriftResult: clusterDrift(r.Context(), d, id, repo, c.Git.PolicyPath),
		})
	}
}

// clusterDrift 回答「仓库里声明的那份，GitOps 到底有没有落进集群」
// （design doc 2026-08-25 §5）。
//
// 两个数据源各自可能不可用：没装写入器就枚举不了仓库，集群没被采过就读不到
// 对象。**任一不可用一律答 UNKNOWN，不答 CONVERGED** —— 后者会让操作者以为
// 下发已经生效，而平台其实一个字节都没看过。
func clusterDrift(
	ctx context.Context, d Deps, clusterID string, repo registry.GitRepo, policyPath string,
) ClusterDrift {
	if d.PolicyWriter == nil {
		return ClusterDriftUnknown
	}
	listing, err := d.PolicyWriter.List(ctx, repo, policyPath)
	if err != nil {
		return ClusterDriftUnknown
	}
	var inRepo []string
	for _, f := range listing.Files {
		if f.Oversize {
			continue
		}
		policies, err := parseNetworkPolicies(f.Content)
		if err != nil {
			// 解析不了的文件不参与判定，与删除流程里的 UNPARSEABLE 同一处置：
			// 平台不知道它声明了什么，拿它去判"落没落下去"只会得出一个编的结论。
			continue
		}
		for _, p := range policies {
			inRepo = append(inRepo, p.Namespace+"/"+p.Name)
		}
	}

	live, err := d.Reader.LivePolicies(ctx, clusterID)
	if err != nil {
		return ClusterDriftUnknown
	}
	inCluster := make([]string, 0, len(live))
	for _, p := range live {
		inCluster = append(inCluster, p.Namespace+"/"+p.Name)
	}
	return clusterDriftOf(inRepo, inCluster)
}
