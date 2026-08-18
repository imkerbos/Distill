package collectstore_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// previewRunID 是本文件全部用例那一次采集的 ID，也是"补一类依据资源"
// 那个子用例要指向的运行。
const previewRunID = "run-preview"

// dnsServiceLabel 是 kube-dns 后端的标签，DNS Baseline 的 peer 取的就是它。
const dnsServiceLabel = "kube-dns"

// savePreviewRun 落一次带 Baseline 依据资产的采集运行。
//
// 与 saveRun 分开而不是给它加参数：那个函数是流量类用例的地基，几十处断言
// 挂在它固定的那份快照上，改它的形状等于同时改掉那些用例的前提。
func savePreviewRun(
	t *testing.T, s *snapshotstore.Store, at time.Time,
	pods []snapshot.Pod, services []snapshot.Service,
	endpoints []snapshot.Endpoints, gateways []snapshot.Gateway,
	failures ...snapshot.Failure,
) {
	t.Helper()
	// 状态由既有的推导函数给出，不在测试里写死：一次「Service 被拒、其余都
	// 采到了」的运行是 PARTIAL，而 PARTIAL 与 OK 的区别正是这一组用例的支点。
	attempted := []snapshot.ResourceKind{
		snapshot.ResourceNamespace, snapshot.ResourcePod, snapshot.ResourceNode,
		snapshot.ResourceService, snapshot.ResourceEndpointSlice,
		snapshot.ResourceNetworkPolicy, snapshot.ResourceIngress,
	}
	run := snapshot.Run{
		Status:     snapshot.DeriveRunStatus(attempted, failures),
		Failures:   failures,
		StartedAt:  at.Add(-30 * time.Second),
		FinishedAt: at.Add(5 * time.Second),
		Observation: snapshot.Observation{
			ClusterID:  collectedID,
			RunID:      previewRunID,
			ObservedAt: at,
			// 每个 namespace 都带上 kubernetes.io/metadata.name。
			//
			// 不是装饰：候选策略的 peer 与 DNS Baseline 都写成
			// namespaceSelector{kubernetes.io/metadata.name: ...}（policygen
			// 与 baseline 里的 nsNameLabel），而 kube-apiserver 保证这个标签
			// 存在。种子里漏掉它，每一条学出来的规则都匹配不上任何对端，于是
			// 整份预测退化成"什么都会被拦断"—— 一个看起来很可怕、却完全是
			// 测试数据造出来的结论。
			Namespaces: namespacesNamed("payment", "shop", "kube-system"),
			Pods:       pods,
			Services:   services,
			Endpoints:  endpoints,
			Gateways:   gateways,
			Policies: []snapshot.NetworkPolicy{{
				ClusterID: collectedID, Namespace: "payment", Name: "allow-api",
				UID:      "8f14e45f-ceea-467a-9ba5-7b5f0f1f0f01",
				Manifest: apiPolicy,
			}},
		},
	}
	ctx := context.Background()
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save(%s) error = %v", previewRunID, err)
	}
	if err := s.DeriveIdentityIntervals(ctx, collectedID, previewRunID); err != nil {
		t.Fatalf("DeriveIdentityIntervals(%s) error = %v", previewRunID, err)
	}
}

// namespacesNamed 造出带 kubernetes.io/metadata.name 标签的命名空间快照。
func namespacesNamed(names ...string) []snapshot.Namespace {
	out := make([]snapshot.Namespace, 0, len(names))
	for _, name := range names {
		out = append(out, snapshot.Namespace{
			ClusterID: collectedID, Name: name,
			Labels: map[string]string{"kubernetes.io/metadata.name": name},
		})
	}
	return out
}

// dnsAssets 是一份**依据齐备**的 DNS 推导材料：kube-dns 有 selector，
// 后端非空。有了它，DNS 就不该出现在缺失清单里 —— 那是这份证据链走通的
// 正面对照。
func dnsAssets() ([]snapshot.Service, []snapshot.Endpoints) {
	return []snapshot.Service{{
			ClusterID: collectedID, Namespace: "kube-system", Name: dnsServiceLabel,
			Type:     "ClusterIP",
			Selector: map[string]string{"k8s-app": dnsServiceLabel},
			Ports: []snapshot.ServicePort{
				{Name: "dns", Port: 53, TargetPort: 53, Protocol: "UDP"},
			},
		}}, []snapshot.Endpoints{{
			ClusterID: collectedID, Namespace: "kube-system", Name: dnsServiceLabel,
			Addresses: []string{"10.4.0.10"},
			Ports:     []int32{53},
		}}
}

// seedPreviewCluster 造出预览用的时间线：一次采集在窗口之前，资产因此
// 用得上；DNS 依据齐备。
//
// payment 刻意造成「**适用却推不出规则**」：
//
//   - 有一个 type=LoadBalancer 的 Service —— 健康检查确实会打进来，因此
//     LB_HEALTH_CHECK 这一类适用；而集群登记里没有健康检查网段，推不出规则。
//   - 有一个 Pod 声明 prometheus.io/scrape=true —— 因此 METRICS_SCRAPE 适用；
//     而没有登记任何抓取端，推不出规则。
//
// 这两条正是本组对照要的那种数据。**没有暴露对象、也没有被抓声明的
// namespace 走的是「不适用」而不是「缺失」**（design doc
// 2026-08-18-baseline-applicability）—— 拿那种 namespace 做对照，一个
// 把缺失清单恒返回空的实现照样能过。
func seedPreviewCluster(t *testing.T, s *snapshotstore.Store) {
	t.Helper()
	services, endpoints := dnsAssets()
	services = append(services, snapshot.Service{
		ClusterID: collectedID, Namespace: "payment", Name: "pay-lb",
		Type:     "LoadBalancer",
		Selector: map[string]string{"app": "api"},
		Ports: []snapshot.ServicePort{
			{Name: "https", Port: 443, TargetPort: 8443, Protocol: "TCP"},
		},
	})
	pods := stablePods()
	pods[0].ScrapeAnnotations = map[string]string{
		snapshot.ScrapeAnnotationScrape: "true",
		"prometheus.io/port":            "9090",
	}
	savePreviewRun(t, s, firstRunAt, pods, services, endpoints, nil)
}

// openTestDB 另开一条到同一个库的连接，供用例直接写入证据行。
//
// 只用来模拟"采集层日后补上一类依据资源"：那一步属于采集层，本轮不改它
// （brief 的最小改动），但判据必须能被证明是读证据、不是读常量表。
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := mysqlregistry.Open(config.DatabaseConfig{DSN: testDSN(t), MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// enumerateResource 给锚点那次运行补一行资源枚举，模拟采集层补上了这一类。
func enumerateResource(t *testing.T, resource string) {
	t.Helper()
	if _, err := openTestDB(t).Exec(
		`INSERT INTO collection_run_resource (cluster_id, run_id, resource, item_count)
		 VALUES (?, ?, ?, 0)`, collectedID, previewRunID, resource); err != nil {
		t.Fatalf("enumerate %s: %v", resource, err)
	}
}

// allChanges 把四类变化摊平成一串，供"整份预测"这类断言逐条检查。
func allChanges(rep predict.Report) []predict.ChangedFlow {
	var out []predict.ChangedFlow
	for _, kind := range predict.AllChangeKinds() {
		out = append(out, rep.Changes[kind]...)
	}
	return out
}

// findChange 在四类变化里按端口找出那一条。
func findChange(t *testing.T, rep predict.Report, kind predict.ChangeKind, port int32) predict.ChangedFlow {
	t.Helper()
	for _, f := range rep.Changes[kind] {
		if f.Port == port {
			return f
		}
	}
	t.Fatalf("no %s carrying port %d; the report holds %v", kind, port, rep.Counts)
	return predict.ChangedFlow{}
}

// 窗口完整度非 COMPLETE 时，整份预测标 DEGRADED（design doc §4）。
//
// 观测不全时说的"不会拦断任何连接"，与观测完整时的同一句话含义完全不同。
// 断言逐条铺开而不是只看计数：一条漏掉传导的连接在计数上会被其余的盖住，
// 而它恰恰是屏幕上那条读起来可信的结论。
//
// 覆盖前与覆盖后两份预测都要查 —— 两份并排显示，一份 TRUSTED、一份
// DEGRADED 时没有人知道该信哪一份。
//
// **对照组不可省**：同一份种子换成一个 COMPLETE 的窗口，必须重新出现
// TRUSTED。少了它，一个把可信度无条件写成 DEGRADED 的实现照样全绿，而那
// 会让"这段观测有问题"退化成一句永远为真、因而没有信息的话。
func TestAnIncompleteWindowDegradesTheWholePrediction(t *testing.T) {
	seed := func(t *testing.T, complete bool) store.PolicyPreview {
		t.Helper()
		r, s := newTestReader(t)
		seedPreviewCluster(t, s)
		conns := []flow.Connection{
			conn(recycledIP, peerIP, portResolved),
			conn(recycledIP, outsideIP, portOutside),
		}
		if complete {
			saveIngest(t, s, conns)
		} else {
			saveSampledIngest(t, s, conns)
		}
		pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
		if err != nil {
			t.Fatalf("PolicyPreview() error = %v", err)
		}
		return pv
	}

	t.Run("窗口漏过记录时整份预测不可信", func(t *testing.T) {
		pv := seed(t, false)
		for name, rep := range map[string]predict.Report{
			"Prediction": pv.Prediction, "Overridden.Prediction": pv.Overridden.Prediction,
		} {
			if rep.TotalEvaluated != 2 {
				t.Fatalf("%s.TotalEvaluated = %d, want 2: both observed connections must be predicted, "+
					"including the one whose ends could not be attributed", name, rep.TotalEvaluated)
			}
			if rep.TrustedCount != 0 || rep.DegradedCount != rep.TotalEvaluated {
				t.Errorf("%s trusted/degraded = %d/%d over %d, want 0/%d: the ingest reported dropped "+
					"records, so no conclusion drawn from this window is trustworthy",
					name, rep.TrustedCount, rep.DegradedCount, rep.TotalEvaluated, rep.TotalEvaluated)
			}
			for _, f := range allChanges(rep) {
				if f.Confidence != "DEGRADED" {
					t.Errorf("%s: port %d Confidence = %q, want DEGRADED", name, f.Port, f.Confidence)
				}
			}
		}
	})

	// 对照组：同样两条连接、同样的资产，只是这一次摄入没有报告丢弃。
	t.Run("对照组：完整窗口仍然给得出 TRUSTED", func(t *testing.T) {
		pv := seed(t, true)
		rep := pv.Prediction
		if rep.TotalEvaluated != 2 {
			t.Fatalf("TotalEvaluated = %d, want 2", rep.TotalEvaluated)
		}
		if rep.TrustedCount == 0 {
			t.Fatalf("TrustedCount = 0 over %d on a COMPLETE window; a reader that always answers "+
				"DEGRADED says nothing at all", rep.TotalEvaluated)
		}
		if rep.TrustedCount+rep.DegradedCount+rep.UnratedCount != rep.TotalEvaluated {
			t.Errorf("trusted %d + degraded %d + unrated %d != total %d",
				rep.TrustedCount, rep.DegradedCount, rep.UnratedCount, rep.TotalEvaluated)
		}
	})
}

// verdict 与 confidence 是两个字段：一次判定可以同时是"会拦断"且"不可信"。
//
// 支点是一条打在风险端口上的连接：它现在通着（shop 没有策略），而候选策略
// 学到的那条规则因为命中风险端口清单默认不启用，于是候选策略下它被拦断 ——
// 一条真正的 WOULD_BREAK。这正是操作者据以批准或叫停一次下发的那个数字。
//
// 两个窗口跑同一条连接：DEGRADED 的那个必须仍然报 WOULD_BREAK（只有可信度
// 变了），COMPLETE 的那个必须报 TRUSTED（只有可信度变了）。把两个字段合并
// 的实现两边都过不去 —— 合并意味着降级会把这条连接挪出 WOULD_BREAK，于是
// 一个漏过记录的窗口报"会拦断 0 条"，而那是本平台唯一那个要命的方向。
func TestVerdictAndConfidenceStaySeparate(t *testing.T) {
	preview := func(t *testing.T, complete bool) store.PolicyPreview {
		t.Helper()
		r, s := newTestReader(t)
		seedPreviewCluster(t, s)
		conns := []flow.Connection{
			conn(recycledIP, peerIP, portResolved),
			conn(recycledIP, peerIP, portDatabase),
		}
		if complete {
			saveIngest(t, s, conns)
		} else {
			saveSampledIngest(t, s, conns)
		}
		pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
		if err != nil {
			t.Fatalf("PolicyPreview() error = %v", err)
		}
		return pv
	}

	for _, tc := range []struct {
		name           string
		complete       bool
		wantConfidence string
	}{
		{"漏过记录的窗口：会拦断，且不可信", false, "DEGRADED"},
		{"对照组：完整窗口：会拦断，且可信", true, "TRUSTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := preview(t, tc.complete).Prediction

			broken := findChange(t, rep, predict.ChangeWouldBreak, portDatabase)
			// verdict 那一半：这条连接现在通着，候选策略下会被拦断。两个窗口
			// 里都必须是这句话 —— 降级不得把它挪出 WOULD_BREAK。
			if broken.Current != "ALLOW" || broken.Predicted != "DENY" {
				t.Errorf("port %d: %s → %s, want ALLOW → DENY: shop carries no policy today, and the "+
					"learned rule for a risky port is generated but not enabled",
					portDatabase, broken.Current, broken.Predicted)
			}
			// confidence 那一半：只有它随窗口完整度变。
			if broken.Confidence != tc.wantConfidence {
				t.Errorf("port %d: Confidence = %q, want %q: completeness moves confidence and nothing "+
					"else", portDatabase, broken.Confidence, tc.wantConfidence)
			}
		})
	}

	// 对照组：verdict 不是一个常量。完整窗口里同一份报告同时装着一条
	// UNCHANGED —— 少了它，一个"凡事都报 WOULD_BREAK"的实现照样能让上面
	// 两个子用例通过。
	//
	// 只在完整窗口里查得到，这是 policygen 的既有语义（aggregate.go 的
	// classify）：DEGRADED 的观测一律进 Ungeneratable，学不出放行规则，于是
	// 一个漏过记录的窗口里每条连接都会被候选策略拦下。方向朝关，且
	// Ungeneratable 会照实说出原因 —— 但它意味着 DEGRADED 窗口的
	// WOULD_BREAK 计数不可直接当作"上线会断多少条"来读。
	t.Run("对照组：完整窗口里 verdict 不是常量", func(t *testing.T) {
		rep := preview(t, true).Prediction
		unchanged := findChange(t, rep, predict.ChangeUnchanged, portResolved)
		if unchanged.Current != "ALLOW" || unchanged.Predicted != "ALLOW" {
			t.Errorf("port %d: %s → %s, want ALLOW → ALLOW: this port is not on the risk list, so the "+
				"learned rule is enabled", portResolved, unchanged.Current, unchanged.Predicted)
		}
		if unchanged.Confidence != "TRUSTED" {
			t.Errorf("port %d: Confidence = %q, want TRUSTED", portResolved, unchanged.Confidence)
		}
		if rep.Counts[predict.ChangeWouldBreak] != 1 {
			t.Errorf("WOULD_BREAK count = %d, want exactly 1; counts = %v",
				rep.Counts[predict.ChangeWouldBreak], rep.Counts)
		}
	})
}

// 「没看过」不得**只**报成「缺失」（design doc §11）。
//
// 函数名是 brief 定下的，读起来比实际强一格：未评估的类照旧留在
// MissingBaselines 里，本用例要的是它**同时**被标成未评估。摘除会让只读
// 缺失清单的 Enforcing 门禁看见比实际更少的阻塞项（§11 那条用户裁定）。
//
// 判据取自 collection_run_resource —— 锚点那次采集实际枚举了哪些
// ResourceKind。三件事一起钉住：
//
//  1. 依据从没被枚举的两类（METRICS_SCRAPE / NODE_AGENT）进未评估，
//     **同时照旧留在缺失清单里** —— 未评估是叠加的说明，不是摘除；
//  2. **对照组**：LB_HEALTH_CHECK 与 CONTROL_PLANE 的依据这次都枚举到了
//     （SERVICE / INGRESS 都有行，apiserver 端点来自集群登记），却确实
//     推导不出来，必须仍然报"缺失"。少了这一组，一个"凡是缺的都叫未评估"
//     的实现照样全绿，而那会把 Enforcing 门禁前真正该修的那条藏起来；
//  3. 判据是证据不是常量表：给那次运行补一行 SCRAPE_TARGET 之后，
//     METRICS_SCRAPE 必须**自动**从未评估挪进缺失，代码一个字不改。
func TestUncollectedEvidenceIsNotAMissingBaseline(t *testing.T) {
	preview := func(t *testing.T, r *collectstore.Reader) store.PolicyPreview {
		t.Helper()
		pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
		if err != nil {
			t.Fatalf("PolicyPreview() error = %v", err)
		}
		return pv
	}
	seed := func(t *testing.T) *collectstore.Reader {
		t.Helper()
		r, s := newTestReader(t)
		seedPreviewCluster(t, s)
		saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})
		return r
	}

	missingOf := func(t *testing.T, pv store.PolicyPreview, namespace string) map[baseline.Kind]bool {
		t.Helper()
		out := map[baseline.Kind]bool{}
		for _, m := range pv.MissingBaselines {
			if m.Namespace != namespace {
				continue
			}
			for _, k := range m.Kinds {
				out[k] = true
			}
		}
		return out
	}

	t.Run("依据没被枚举的那一类进未评估，同时留在缺失清单里", func(t *testing.T) {
		pv := preview(t, seed(t))

		// **2026-08-18：METRICS_SCRAPE 离开了这一栏。** 它的依据现在落在
		// Pod 的 prometheus.io/* 注解上，而 Pod 是每次采集都枚举的资源 ——
		// 于是它再出现在缺失清单里，含义变成"我们看过了，这个集群没有登记
		// 抓取端"，那是一个照着能做点什么的缺口，不是盲区。
		//
		// NODE_AGENT 仍在：它的依据（agent 实际访问的端口）不在任何资产里。
		want := []baseline.Kind{baseline.KindNodeAgent}
		if len(pv.NotAssessedBaselines) != len(want) {
			t.Fatalf("NotAssessedBaselines = %v, want %v: the collection layer still does not "+
				"enumerate NodeAgent, so that one cannot be called missing",
				pv.NotAssessedBaselines, want)
		}
		for i, k := range want {
			if pv.NotAssessedBaselines[i] != k {
				t.Errorf("NotAssessedBaselines[%d] = %s, want %s", i, pv.NotAssessedBaselines[i], k)
			}
		}
		// 两栏重叠是刻意的（design doc §11）：未评估是**叠加的**说明，不是
		// 一次从缺失清单里的摘除。摘除会让只读缺失清单的门禁看见比实际更少
		// 的阻塞项 —— 而那个门禁还没写，写它的人最自然的写法就是只读这一栏。
		missing := missingOf(t, pv, "payment")
		for _, k := range want {
			if !missing[k] {
				t.Errorf("%s is flagged not-assessed but has left MissingBaselines; a consumer reading "+
					"only the missing list would see fewer blockers than really exist (missing = %v)",
					k, pv.MissingBaselines)
			}
		}
	})

	t.Run("对照组：依据齐备却推导不出来的，仍然报缺失", func(t *testing.T) {
		pv := preview(t, seed(t))
		missing := missingOf(t, pv, "payment")

		// SERVICE 与 INGRESS 这次都被枚举了（一类一行，含计数为 0 的），
		// 集群登记里没有 apiserver 端点、也没有健康检查网段 —— 于是这两类
		// 是真的推导不出来，而不是没看过。
		for _, k := range []baseline.Kind{baseline.KindLBHealth, baseline.KindControlPlane} {
			if !missing[k] {
				t.Errorf("%s is not reported missing; its evidence WAS enumerated by this run, so "+
					"failing to derive it is a conclusion about the cluster and the one thing an "+
					"operator has to fix before Enforcing", k)
			}
			for _, na := range pv.NotAssessedBaselines {
				if na == k {
					t.Errorf("%s landed in NotAssessedBaselines; that hides a real gap behind a "+
						"statement about ourselves", k)
				}
			}
		}
		// 正面对照：依据齐备且推导得出来的那一类不该出现在任何一栏。
		if missing[baseline.KindDNS] {
			t.Errorf("DNS reported missing while kube-dns and its backends were both collected; "+
				"missing = %v", pv.MissingBaselines)
		}
	})

	t.Run("采集层补上依据之后自动收敛到缺失", func(t *testing.T) {
		r := seed(t)
		enumerateResource(t, string(baseline.SourceScrapeTarget))

		pv := preview(t, r)
		for _, k := range pv.NotAssessedBaselines {
			if k == baseline.KindMetrics {
				t.Fatalf("METRICS_SCRAPE still reported as not assessed after the run enumerated "+
					"SCRAPE_TARGET; the criterion is reading a constant table, not the evidence "+
					"(NotAssessedBaselines = %v)", pv.NotAssessedBaselines)
			}
		}
		// 它在缺失清单里 —— 补上依据前后都在（两栏叠加，不是二选一），
		// 所以这条不区分收敛与否；留着是为了守住"这一类没有整个消失"。
		// 有区分力的是上面那条与下面那条。
		if !missingOf(t, pv, "payment")[baseline.KindMetrics] {
			t.Errorf("METRICS_SCRAPE disappeared from MissingBaselines entirely; missing = %v",
				pv.MissingBaselines)
		}
		// NODE_AGENT 没有跟着动 —— 补的是另一类依据。
		if len(pv.NotAssessedBaselines) != 1 || pv.NotAssessedBaselines[0] != baseline.KindNodeAgent {
			t.Errorf("NotAssessedBaselines = %v, want only NODE_AGENT", pv.NotAssessedBaselines)
		}
	})
}

// 采集失败的依据资源同样是「没看过」，不得**只**报成「缺失」（§11 情形 2）。
//
// 与上一个用例同一条更正：它留在 MissingBaselines 里，另外被标成未评估。
//
// 这是上一版漏掉的那一种，也是三种情形里最常见的：写入侧对**每一类**资源都写
// 一行计数（`snapshot.Observation.Counts()` 是固定七项），包括那一类被 403 挡掉
// 的时候。于是「有没有计数行」在真实的 ResourceKind 上恒真，一个 RBAC 不全的
// 集群会永远挂着一条**没有标注**的 `DNS` 缺失 —— 运维去写一条 DNS 策略，
// 而真正要改的是 RBAC。
//
// 种子走的是 `snapshotstore.Save` 这条**真实写入路径**，不是裸 SQL：这个用例
// 要证明的第一件事就是「失败了照样留下计数行」，用裸 SQL 造行等于把被测的那
// 个前提换掉。
//
// **对照组不可省**：同一份种子去掉失败记录（Service 就是采回来了、集群里恰好
// 没有 kube-dns），`DNS` 必须报缺失**且不带未评估标注**。少了它，一个「凡是
// 推不出来的都顺手标未评估」的实现照样全绿，而那会把「补 RBAC」与「写策略」
// 这两个不同的修法混成一个。
func TestAFailedEvidenceCollectionIsNotAMissingBaseline(t *testing.T) {
	// 两份种子只差一件事：Service 那一类有没有失败记录。两边的
	// observed_service 都是空的，因此 DNS 两边都推导不出来 —— 唯一的变量是
	// 「我们看过没有」。
	seed := func(t *testing.T, failed bool) store.PolicyPreview {
		t.Helper()
		r, s := newTestReader(t)
		var failures []snapshot.Failure
		if failed {
			failures = []snapshot.Failure{{
				Resource: snapshot.ResourceService,
				Reason:   snapshot.FailureForbidden,
				Detail:   "services is forbidden: User cannot list resource \"services\"",
			}}
		}
		savePreviewRun(t, s, firstRunAt, stablePods(), nil, nil, nil, failures...)
		saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})
		pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
		if err != nil {
			t.Fatalf("PolicyPreview() error = %v", err)
		}
		return pv
	}
	holds := func(kinds []baseline.Kind, want baseline.Kind) bool {
		for _, k := range kinds {
			if k == want {
				return true
			}
		}
		return false
	}
	missingOf := func(pv store.PolicyPreview, namespace string) []baseline.Kind {
		var out []baseline.Kind
		for _, m := range pv.MissingBaselines {
			if m.Namespace == namespace {
				out = append(out, m.Kinds...)
			}
		}
		return out
	}

	t.Run("Service 被拒时 DNS 进未评估", func(t *testing.T) {
		pv := seed(t, true)
		if !holds(pv.NotAssessedBaselines, baseline.KindDNS) {
			t.Errorf("NotAssessedBaselines = %v, want DNS among them: the Service list came back 403, "+
				"so whether this cluster has a kube-dns is something we never saw",
				pv.NotAssessedBaselines)
		}
		// **两栏都要有它。** 未评估回答的是"为什么它在缺失清单里"，不是
		// "把它从缺失清单里拿掉"：一次 403 让 DNS 离开缺失清单，就等于让一个
		// 从没验证过 DNS 依据的集群在只读那一栏的门禁前被放行。
		if !holds(missingOf(pv, "payment"), baseline.KindDNS) {
			t.Errorf("DNS is flagged not-assessed but has left MissingBaselines; a forbidden Service "+
				"list must not make a blocker disappear — missing = %v", pv.MissingBaselines)
		}
		// LB_HEALTH_CHECK 的依据里也有 SERVICE，因此它跟着进未评估；
		// CONTROL_PLANE 只吃集群登记，不受这次失败影响，必须仍然报缺失。
		if !holds(pv.NotAssessedBaselines, baseline.KindLBHealth) {
			t.Errorf("LB_HEALTH_CHECK not among %v; its evidence includes SERVICE too",
				pv.NotAssessedBaselines)
		}
		if !holds(missingOf(pv, "payment"), baseline.KindLBHealth) {
			t.Errorf("LB_HEALTH_CHECK left MissingBaselines as well; missing = %v", pv.MissingBaselines)
		}
		if !holds(missingOf(pv, "payment"), baseline.KindControlPlane) {
			t.Errorf("CONTROL_PLANE is not reported missing; it derives from the cluster registry " +
				"alone and a forbidden Service list says nothing about it")
		}
	})

	t.Run("对照组：Service 采回来了、只是没有 kube-dns，DNS 仍报缺失", func(t *testing.T) {
		pv := seed(t, false)
		// 对照组要的是"缺失且**不带**未评估标注"：两栏都亮与只有缺失亮，
		// 指向的是两个不同的修法（补 RBAC / 写策略）。少了这条否定断言，
		// 一个"凡是缺的都顺手标未评估"的实现照样全绿，而 §11 的区分随之消失。
		if holds(pv.NotAssessedBaselines, baseline.KindDNS) {
			t.Errorf("DNS landed in NotAssessedBaselines = %v while the Service list came back "+
				"cleanly; the operator would go fix RBAC when the fix is to write a policy",
				pv.NotAssessedBaselines)
		}
		if !holds(missingOf(pv, "payment"), baseline.KindDNS) {
			t.Errorf("DNS is not reported missing; missing = %v", pv.MissingBaselines)
		}
	})
}

// 窗口完整度必须写在报告上，不能让调用方从 DegradedCount 推断（design doc §4.1）。
//
// 非 COMPLETE 的窗口里 policygen 学不出任何放行规则，于是 WOULD_BREAK 逼近
// 整个窗口的连接数 —— 那个数字不是一次关于上线影响的预测。判断这件事今天只能
// 去数 DegradedCount == TotalEvaluated，而一条前端必须自己推对的结论不是契约
// 说出来的事实。
//
// **对照组不可省**：同一份种子换成完整窗口必须回到 COMPLETE。少了它，一个把
// 字段写死成 DEGRADED 的实现全绿；而两个 Reader 各自钉一次，是因为一个把它
// 写死成 COMPLETE 的实现在 FixtureReader 那边同样全绿。
func TestTheReportStatesItsWindowCompleteness(t *testing.T) {
	preview := func(t *testing.T, complete bool) store.PolicyPreview {
		t.Helper()
		r, s := newTestReader(t)
		seedPreviewCluster(t, s)
		conns := []flow.Connection{conn(recycledIP, peerIP, portResolved)}
		if complete {
			saveIngest(t, s, conns)
		} else {
			saveSampledIngest(t, s, conns)
		}
		pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
		if err != nil {
			t.Fatalf("PolicyPreview() error = %v", err)
		}
		return pv
	}

	t.Run("漏过记录的窗口照实说自己 DEGRADED", func(t *testing.T) {
		pv := preview(t, false)
		if pv.WindowCompleteness != flow.CompletenessDegraded {
			t.Errorf("WindowCompleteness = %q, want DEGRADED: the ingest reported dropped records, "+
				"and the WOULD_BREAK count of such a window is not a rollout impact count",
				pv.WindowCompleteness)
		}
		// 与它要限定的那个数字一起断言：少了这条，字段可以是对的，而它
		// 描述的那份预测早已换成了别的东西。
		if pv.Prediction.TotalEvaluated == 0 {
			t.Fatal("TotalEvaluated = 0; the field would be describing an empty report")
		}
		if pv.Prediction.DegradedCount != pv.Prediction.TotalEvaluated {
			t.Errorf("DegradedCount %d != TotalEvaluated %d; the field and the report disagree about "+
				"the same window", pv.Prediction.DegradedCount, pv.Prediction.TotalEvaluated)
		}
	})

	t.Run("对照组：完整窗口说自己 COMPLETE", func(t *testing.T) {
		pv := preview(t, true)
		if pv.WindowCompleteness != flow.CompletenessComplete {
			t.Errorf("WindowCompleteness = %q, want COMPLETE: a reader that always answers DEGRADED "+
				"says nothing at all", pv.WindowCompleteness)
		}
	})
}
