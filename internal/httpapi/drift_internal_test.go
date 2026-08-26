package httpapi

import "testing"

// 集群漂移回答的是「GitOps 到底有没有把仓库里那份落下去」
// （design doc 2026-08-25 §5）。
//
// 既有的 driftResult 比的是「仓库 vs 平台最后写过的 commit」，答不了这个：
// controller 挂掉、同步失败、或有人手工 apply 了一份仓库里没有的对象，
// 那个字段全都看不出来。
func TestClusterDriftAnswersWhetherTheRepoLanded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inRepo []string // 仓库 distill/ 子树声明的对象，"ns/name"
		inLive []string // 集群里实际有的 candidate 对象
		want   string
	}{
		{"都落下去了", []string{"payment/candidate-api-ingress"},
			[]string{"payment/candidate-api-ingress"}, "CONVERGED"},
		{"仓库有集群没有", []string{"payment/candidate-api-ingress"},
			nil, "PENDING"},
		{"集群有仓库没有", nil,
			[]string{"payment/candidate-api-ingress"}, "CLUSTER_AHEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterDriftOf(tc.inRepo, tc.inLive); string(got) != tc.want {
				t.Errorf("clusterDriftOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

// 平台不认识的对象不参与判定：策略目录下别人放的东西不是"集群领先"。
func TestClusterDriftIgnoresObjectsThePlatformDoesNotOwn(t *testing.T) {
	got := clusterDriftOf(nil, []string{"payment/someones-own-policy"})
	if string(got) != "CONVERGED" {
		t.Errorf("clusterDriftOf() = %q, want CONVERGED —— 别人的对象不该被读成集群领先", got)
	}
}
