package collectstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// **写回的数字必须算在"合并之后"的策略集上，不是候选集单独跑**
// （design doc 2026-08-25-existing-policies §3）。
//
// 平台只加不删：写回把候选写进仓库，GitOps apply 进集群，而集群里原有的
// 策略一条都不会因此消失。只跑候选集会把旧策略额外放行的部分算成"会被
// 拦断"，于是一次实际无害的写回看起来要断掉几十条连接 —— 而反复出现的
// 假警报，最终会让真的那次也没人看。
//
// 场景：观测窗口报告丢过记录（saveSampledIngest），于是学出来的规则全部
// 降级、默认不启用 → 候选集里没有覆盖这条连接的放行；而集群里那条已有的
// NetworkPolicy 一直放行着它。两份预测因此必然分叉。
func TestWritebackCountsComeFromTheMergedPolicySet(t *testing.T) {
	r, s := newTestReader(t)
	seedClusterWithAllowPolicy(t, s)
	// 明确报告丢过记录：证据因此全部降级，学出来的规则默认不进启用集。
	saveSampledIngest(t, s, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() = %v", err)
	}

	// 这条用例不带覆盖，取数走回落访问器 —— 缺席的含义是「与基础份恒等」。
	only := pv.OverriddenPrediction().Counts[predict.ChangeWouldBreak]
	merged := pv.OverriddenPredictionWithExisting().Counts[predict.ChangeWouldBreak]

	// 两份都必须给出来：少了任何一份，"旧策略额外放行了多少"这个差额就算不出来。
	if pv.OverriddenPredictionWithExisting().TotalEvaluated == 0 {
		t.Fatal("并入已有策略的那一份预测是空的 —— 它才是操作者点下去会发生的事")
	}
	if merged > only {
		t.Errorf("并入已有策略之后反而多拦断了：only=%d merged=%d。"+
			"平台只加不删，加进去的策略不可能让原本通着的连接变得更少", only, merged)
	}
	// **两份必须真的分叉**，否则下面那几条断言等于没跑：一个恒取错口径的
	// 实现照样能在两份相同时全绿。
	if only == merged {
		t.Fatalf("两份预测相同（only=merged=%d）—— 种子没能造出"+
			"「已有策略额外放行」的情形，这条用例区分不了口径", only)
	}

	// 提交信息里的数字必须是"合并之后"那一份。
	// 这是评审人唯一会读的一段文字，取错口径等于给他一个假警报。
	for _, k := range predict.AllChangeKinds() {
		want := pv.OverriddenPredictionWithExisting().Counts[k]
		if got := writebackCountsOf(pv)[k]; got != want {
			t.Errorf("写回口径的 %s = %d, want %d（合并之后那一份）", k, got, want)
		}
	}
}

// writebackCountsOf 复刻 httpapi.writebackCounts 的取数口径。
//
// 这里重写一遍而不是导出那个函数：这条用例守的正是"取的是哪一份"，
// 而一个直接调用被测函数的断言只会永远与它自己相等。
func writebackCountsOf(pv store.PolicyPreview) map[predict.ChangeKind]int {
	// 缺席时回落到顶层那一份 —— 含义是「与它恒等」。这里把回落**显式写出来**，
	// 而不是调 pv.OverriddenPredictionWithExisting()：那是被测方法，调它这条
	// 断言只会永远与它自己相等。
	rep := pv.PredictionWithExisting
	if pv.Overridden.PredictionWithExisting != nil {
		rep = *pv.Overridden.PredictionWithExisting
	}
	out := map[predict.ChangeKind]int{}
	for _, k := range predict.AllChangeKinds() {
		out[k] = rep.Counts[k]
	}
	return out
}

// webAllowsEgressPolicy 放行 shop/web 出向到 payment/api:8080。
//
// **两个方向都要放行才有意义**：NetworkPolicy 是双向判定的，源端一旦被
// egress 策略选中就必须显式放行。只种 ingress 那一条，候选集给 shop/web
// 生成的 egress default-deny 会让两份预测在出向上一起判 DENY —— 两份相同，
// 这条用例也就区分不了口径（第一版就是这么写的，跑出来 only=merged=1）。
const webAllowsEgressPolicy = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-web-egress
  namespace: shop
spec:
  podSelector:
    matchLabels:
      app: web
  policyTypes:
  - Egress
  egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: payment
      podSelector:
        matchLabels:
          app: api
    ports:
    - port: 8080
      protocol: TCP
`

// apiAllowsPeerPolicy 是一条**真的放行**的已有策略：允许 shop/web 访问
// payment/api 的 8080。
//
// 种子里那条 allow-api 是 default-deny（policyTypes 只写 Ingress、没有放行
// 规则），因此它不会让两份预测分叉 —— 要区分口径，集群里必须有一条候选集
// **不会重新生成**的放行。
const apiAllowsPeerPolicy = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-api
  namespace: payment
spec:
  podSelector:
    matchLabels:
      app: api
  policyTypes:
  - Ingress
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: shop
      podSelector:
        matchLabels:
          app: web
    ports:
    - port: 8080
      protocol: TCP
`

// seedClusterWithAllowPolicy 落一次带"真放行"策略的采集运行。
//
// 与 seedPreviewCluster 同形，只换掉那条策略的内容：其余每一项都必须一样，
// 否则两份预测的差异会来自别处，而这条用例要的恰恰是"只有策略集不同"。
func seedClusterWithAllowPolicy(t *testing.T, s *snapshotstore.Store) {
	t.Helper()
	services, endpoints := dnsAssets()
	run := snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  firstRunAt.Add(-30 * time.Second),
		FinishedAt: firstRunAt.Add(5 * time.Second),
		Observation: snapshot.Observation{
			ClusterID:  collectedID,
			RunID:      previewRunID,
			ObservedAt: firstRunAt,
			Namespaces: namespacesNamed("payment", "shop", "kube-system"),
			Pods:       stablePods(),
			Services:   services,
			Endpoints:  endpoints,
			Policies: []snapshot.NetworkPolicy{
				{
					ClusterID: collectedID, Namespace: "payment", Name: "allow-api",
					UID:      "8f14e45f-ceea-467a-9ba5-7b5f0f1f0f01",
					Manifest: apiAllowsPeerPolicy,
				},
				{
					ClusterID: collectedID, Namespace: "shop", Name: "allow-web-egress",
					UID:      "8f14e45f-ceea-467a-9ba5-7b5f0f1f0f02",
					Manifest: webAllowsEgressPolicy,
				},
			},
		},
	}
	ctx := context.Background()
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if err := s.DeriveIdentityIntervals(ctx, collectedID, previewRunID); err != nil {
		t.Fatalf("DeriveIdentityIntervals() = %v", err)
	}
}

// **没有任何人工确认时，覆盖后的两份预测不出现在响应里。**
//
// 它们与基础的那两份恒等 —— Apply 对空覆盖只是把候选集深拷贝一份。照旧算
// 一遍、再序列化一遍，等于把一次预览的预测量和响应体都翻倍：UAT 实测一份
// 预览 15.4 MB，其中 46% 正是这两份重复的明细，而前端在没有覆盖时一条都不读
// （dryRunView 用 overrideCount 分支）。
//
// 与同结构体里 Enabled 标 json:"-" 是同一条理由，同一份内容不序列化两次。
//
// **缺席的含义是"与基础份恒等"，不是"零拦断"** —— 因此是缺席而不是零值：
// 一个全零的预测读起来是"应用它什么都不会断"，那是这一屏最不能给出的错觉。
func TestWithoutOverridesTheOverriddenPredictionsAreAbsent(t *testing.T) {
	r, s := newTestReader(t)
	seedClusterWithAllowPolicy(t, s)
	saveSampledIngest(t, s, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() = %v", err)
	}
	if len(pv.Overrides) != 0 {
		t.Fatalf("这条用例的前提是没有人工确认，实际有 %d 条", len(pv.Overrides))
	}
	if pv.Overridden.Prediction != nil {
		t.Error("没有人工确认，却带上了覆盖后的预测 —— 与基础份逐字段相同，白算白传")
	}
	if pv.Overridden.PredictionWithExisting != nil {
		t.Error("没有人工确认，却带上了覆盖后的并入预测")
	}
	// 候选集这一份必须照旧给：它是导出与展示的取数来源，与预测不是一回事。
	if len(pv.Overridden.Candidates) == 0 {
		t.Error("覆盖后的候选集不见了 —— 导出与展示都从它取数")
	}
}

// 有人工确认时两份必须真的分叉，否则上面那条用例在一个"永远复用基础份"的
// 实现下也是绿的 —— 而那个实现会让人工确认在预览上完全不起作用。
//
// 把**全部**默认禁用的规则一次确认掉，而不是挑一条：挑中的那条未必覆盖这份
// 种子里那条唯一的观测连接，于是计数不动，用例读起来像"分叉失效"，实际只是
// 挑错了规则。全部启用之后，覆盖那条连接的规则必然在其中。
func TestWithOverridesTheOverriddenPredictionDiverges(t *testing.T) {
	// 先不带覆盖跑一次，把默认禁用的规则都取出来。
	r, s := newTestReader(t)
	seedClusterWithAllowPolicy(t, s)
	saveSampledIngest(t, s, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() = %v", err)
	}
	var overrides []registry.RuleOverride
	for _, c := range pv.Candidates {
		for _, rule := range c.Rules {
			if rule.Enabled || rule.Fingerprint == "" {
				continue
			}
			overrides = append(overrides, registry.RuleOverride{
				ClusterID: collectedID, Namespace: c.Namespace, Workload: c.Workload,
				Fingerprint: rule.Fingerprint,
				Decision:    policygen.DecisionEnable,
				Reason:      "用例：确认全部默认禁用的规则以构造分叉",
				DecidedBy:   "tester", DecidedAt: time.Now().UTC(),
			})
		}
	}
	if len(overrides) == 0 {
		t.Skip("这一份候选集里没有默认禁用的规则，无法构造分叉")
	}
	baseBreak := pv.Prediction.Counts[predict.ChangeWouldBreak]
	if baseBreak == 0 {
		t.Skip("基础份一条都不拦断，启用规则也无从体现分叉")
	}

	// 再带着这些确认跑一次；落库的事实原样保留（同一个 DSN、同一批表）。
	r2, s2 := newTestReaderWithOverrides(t,
		map[string][]registry.RuleOverride{collectedID: overrides})
	seedClusterWithAllowPolicy(t, s2)
	saveSampledIngest(t, s2, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	pv2, err := r2.PolicyPreview(context.Background(), collectedID, "", describedWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() = %v", err)
	}
	if len(pv2.Overrides) == 0 {
		t.Fatal("人工确认没有被读出来")
	}
	if pv2.Overridden.Prediction == nil {
		t.Fatal("有人工确认，覆盖后的预测却缺席")
	}
	overBreak := pv2.Overridden.Prediction.Counts[predict.ChangeWouldBreak]
	if overBreak >= pv2.Prediction.Counts[predict.ChangeWouldBreak] {
		t.Errorf("启用了 %d 条规则，覆盖后仍拦断 %d 条（基础份 %d）—— "+
			"人工确认在预览上没起作用", len(overrides), overBreak,
			pv2.Prediction.Counts[predict.ChangeWouldBreak])
	}
}
