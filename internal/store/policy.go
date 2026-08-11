package store

import (
	"context"
	"fmt"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
)

// PolicyPreview 是一次候选策略预览的完整产物。
//
// 四块同源返回而非拆成两个端点：拆开会让界面有机会展示"来自计算 A 的
// 策略"配"来自计算 B 的预测"。fixture 上两次计算必然一致，但接真存储后
// 窗口边界一漂就会出现策略与预测互相矛盾的一屏，而这种不一致不会报错。
type PolicyPreview struct {
	// Cluster 是目标集群。
	Cluster string `json:"cluster"`
	// Namespace 是筛选的命名空间；为空表示全集群。
	Namespace string `json:"namespace"`
	// Window 是实际生效的查询时间窗，必须回显。
	Window TimeWindow `json:"window"`
	// Candidates 是候选策略。
	Candidates []policygen.CandidatePolicy `json:"candidates"`
	// MissingBaselines 是尚未齐备的 Baseline 类型。
	//
	// 五类齐备是进入 Enforcing 的前提（spec §7.3 G3）。缺失必须与
	// 候选策略同屏，否则一份"看起来完整"的推荐会掩盖入口中断的风险。
	MissingBaselines []policygen.MissingBaseline `json:"missingBaselines"`
	// Ungeneratable 是无法表达为规则的流量。
	Ungeneratable []policygen.UngeneratableItem `json:"ungeneratable"`
	// Prediction 是 dry-run 预测结果。
	Prediction predict.Report `json:"prediction"`
	// Kinds 是必备 Baseline 的全集，随报告返回。
	//
	// 与 RiskPortCatalog 同理：缺失清单为空时，使用者必须能看到
	// "我们检查了哪五类"，否则一份空缺失与一次根本没做的校验无法区分。
	Kinds []baseline.Kind `json:"baselineKinds"`
}

// PolicyPreview 生成候选策略并回放预测。集群不存在时返回错误。
func (r *FixtureReader) PolicyPreview(
	_ context.Context, clusterID, namespace string, window TimeWindow,
) (PolicyPreview, error) {
	if !window.Valid() {
		return PolicyPreview{}, ErrWindowRequired
	}
	c, ok := r.fleet.Cluster(clusterID)
	if !ok {
		return PolicyPreview{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}

	obs := make([]policygen.Observation, 0, len(r.fleet.Flows))
	for _, f := range r.fleet.Flows {
		if !window.Contains(f.Flow.Timestamp) || !involvesCluster(f.Flow, clusterID) {
			continue
		}
		obs = append(obs, policygen.Observation{
			FlowID: f.ID, Flow: f.Flow, Decision: r.decide(f),
		})
	}

	gen := policygen.Generate(policygen.Input{
		ClusterID: clusterID, Namespace: namespace,
		Assets: c.Assets, Namespaces: c.Namespaces,
		// Pods 必须传入：候选策略按 workload 花名册生成而非按流量生成，
		// 缺了它，流量全 DEGRADED（mesh 内）或全 UNKNOWN（策略写坏）的
		// workload 会从候选集里悄悄消失，连带绕过它们的强制 Baseline 注入。
		Pods:         c.Pods,
		Observations: obs,
	})

	report := predict.Run(predict.Input{
		ClusterID:    clusterID,
		Policies:     gen.EnabledPolicies(),
		Namespaces:   c.Namespaces,
		CCNPPresent:  c.CCNPPresent,
		Observations: obs,
		// 展示名复用流量列表那一套，两个界面必须用同一个名字指同一个 Pod。
		Label: endpointLabel,
	})

	return PolicyPreview{
		Cluster: clusterID, Namespace: namespace, Window: window,
		Candidates:       gen.Policies,
		MissingBaselines: gen.MissingBaselines,
		Ungeneratable:    gen.Ungeneratable,
		Prediction:       report,
		Kinds:            baseline.AllKinds(),
	}, nil
}
