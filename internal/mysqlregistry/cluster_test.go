package mysqlregistry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
)

// newTestStore 建库、迁移到最新、清空数据，返回可用的 Store。
func newTestStore(t *testing.T) (*mysqlregistry.Store, *sql.DB) {
	t.Helper()
	cfg := config.DatabaseConfig{DSN: testDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}
	db, err := mysqlregistry.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := mysqlregistry.Migrate(cfg, "../../migrations"); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	// 每个测试从干净状态开始。删除顺序与外键依赖相反。
	for _, tbl := range []string{
		"audit_log", "rule_override", "policy_import", "cluster_git_binding",
		"git_repo", "cluster_health_check_source", "cluster_apiserver",
		"cluster_agent", "cluster",
	} {
		//nolint:gosec // G202: tbl comes from the fixed literal slice above, not external input
		if _, err := db.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return mysqlregistry.New(db), db
}

// sampleCluster 不带 Git 绑定：绑定由 BindGitRepo 单独写入
// （design doc 2026-08-13 §2），需要绑定的测试自己调它。
func sampleCluster() registry.Cluster {
	return registry.Cluster{
		ID: "prod-asia-1", DisplayName: "Asia Prod",
		PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		CCNPPresent: false, State: registry.StateRegistered,
		// 短名，不是 kubeconfig：这一列存的一直只是 Secret Manager 里的
		// 引用（design doc 2026-08-16 §3.5）。放进公共样本而不是只在
		// 专门的用例里填，图的是 TestClusterSurvivesAFullRoundTripThroughMySQL
		// 那趟 DeepEqual 往返自动覆盖它 —— 挑着断言挡不住写错值。
		KubeconfigRef:      "prod-asia-1-kubeconfig",
		APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
		HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
	}
}

func TestCreateAndReadBackCluster(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	got, ok, err := s.Cluster(ctx, "prod-asia-1")
	if err != nil || !ok {
		t.Fatalf("Cluster() = %v, %v, %v", got, ok, err)
	}
	if got.NodeCIDR != "10.128.0.0/20" {
		t.Errorf("NodeCIDR = %q, want 10.128.0.0/20", got.NodeCIDR)
	}
	if len(got.APIServers) != 1 || got.APIServers[0].Port != 443 {
		t.Errorf("APIServers = %+v, want one entry on 443", got.APIServers)
	}
	if len(got.HealthCheckSources) != 2 {
		t.Errorf("HealthCheckSources = %v, want 2 entries", got.HealthCheckSources)
	}
}

// 这是本包存在的理由：审计与业务写入必须同生共死。
// 业务写入失败时若审计行留了下来，审计就在记录从未发生过的事。
func TestAuditRollsBackWithTheBusinessWrite(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("first CreateCluster() error = %v", err)
	}
	// 同一个 ID 再插一次：主键冲突，业务写入必然失败。
	if err := s.CreateCluster(ctx, actor, sampleCluster()); err == nil {
		t.Fatal("duplicate CreateCluster() succeeded, want an error")
	}

	var audits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if audits != 1 {
		t.Errorf("audit_log rows = %d, want 1 — the failed write must not leave an audit row", audits)
	}
}

func TestUpdateClusterWritesAudit(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	c := sampleCluster()
	c.DisplayName = "Asia Production"
	if err := s.UpdateCluster(ctx, actor, c); err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}

	got, _, _ := s.Cluster(ctx, "prod-asia-1")
	if got.DisplayName != "Asia Production" {
		t.Errorf("DisplayName = %q, want the updated value", got.DisplayName)
	}
	var actions int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'UPDATE_CLUSTER'`).Scan(&actions); err != nil {
		t.Fatalf("count: %v", err)
	}
	if actions != 1 {
		t.Errorf("UPDATE_CLUSTER audit rows = %d, want 1", actions)
	}

	// 一行审计只证明「有人改过」；复盘要问的是「改之前是什么」。
	// 没有这条断言，把 UpdateCluster 里的 before 传成 nil，上面的计数
	// 照样是 1，测试照样绿，而那一行从此回答不了任何问题。
	var before sql.NullString
	if err := db.QueryRow(
		`SELECT before_val FROM audit_log WHERE action = 'UPDATE_CLUSTER'`).Scan(&before); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if !before.Valid {
		t.Fatal("before_val is NULL; the audit row says nothing about what the cluster looked like")
	}
	var prior registry.Cluster
	if err := json.Unmarshal([]byte(before.String), &prior); err != nil {
		t.Fatalf("decode before_val: %v", err)
	}
	if prior.DisplayName != sampleCluster().DisplayName {
		t.Errorf("before_val displayName = %q, want %q — the value that was replaced",
			prior.DisplayName, sampleCluster().DisplayName)
	}
	if prior.ID != "prod-asia-1" || prior.PodCIDR != sampleCluster().PodCIDR {
		t.Errorf("before_val = %+v, want the pre-update cluster", prior)
	}
}

func TestUpdateMissingClusterReturnsNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	err := s.UpdateCluster(context.Background(),
		registry.Actor{Username: "admin"}, sampleCluster())
	if !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// 软删除之后集群不再出现在列表里，但它的审计记录必须仍然查得到 ——
// 一次下线操作不该让平台失忆。
func TestSoftDeleteHidesClusterButKeepsAudit(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.SoftDeleteCluster(ctx, actor, "prod-asia-1"); err != nil {
		t.Fatalf("SoftDeleteCluster() error = %v", err)
	}

	if _, ok, _ := s.Cluster(ctx, "prod-asia-1"); ok {
		t.Error("Cluster() still returns a soft-deleted cluster")
	}
	list, err := s.Clusters(ctx)
	if err != nil {
		t.Fatalf("Clusters() error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("Clusters() = %d entries, want 0", len(list))
	}

	var audits int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE cluster_id = 'prod-asia-1'`).Scan(&audits); err != nil {
		t.Fatalf("count: %v", err)
	}
	// 恰好两条，不是「至少两条」：多出来的审计行意味着某条写路径重复
	// 记账，而复盘时一条被记了两次的操作与两次真实操作无法区分。
	if audits != 2 {
		t.Errorf("audit rows for the deleted cluster = %d, want exactly 2 (create + delete)", audits)
	}
}

func TestCreateClusterRejectsInvalidInput(t *testing.T) {
	s, _ := newTestStore(t)
	c := sampleCluster()
	c.PodCIDR = "10.4.0/14"
	err := s.CreateCluster(context.Background(), registry.Actor{Username: "admin"}, c)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// 重复注册一个已有的集群 ID 是操作者的输入问题，不是服务故障。
//
// 判据是「该不该计入服务错误率」：翻译之前，1062 会一路冒泡成
// CodeInternal + HTTP 500，让注册页在一次正常的手滑上显示「服务器错误」，
// 并把它计进可用性指标。翻译落在捕获驱动错误的这一层，HTTP 层因此
// 只需要认识 registry.ErrInvalid，不必按 MySQL 错误号分支。
func TestDuplicateClusterIDIsAnInputError(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("first CreateCluster() error = %v", err)
	}
	err := s.CreateCluster(ctx, actor, sampleCluster())
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid — a duplicate cluster id is the caller's problem", err)
	}
	// 回传通道只读 InvalidError.Detail，所以文案必须点名冲突的是什么；
	// 同时它必须是我们自己写的话，不能是驱动那句带表名与键名的原文。
	var ie *registry.InvalidError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want an *InvalidError carrying a returnable Detail", err)
	}
	if !strings.Contains(ie.Detail, "prod-asia-1") {
		t.Errorf("Detail = %q, want it to name the conflicting cluster id", ie.Detail)
	}
	for _, leaked := range []string{"Duplicate entry", "PRIMARY", "Error 1062"} {
		if strings.Contains(ie.Detail, leaked) {
			t.Errorf("Detail = %q leaked driver text %q", ie.Detail, leaked)
		}
	}
}

// registry.Cluster → MySQL → registry.Cluster 的整体比对。
//
// 逐字段挑着断言挡不住写错值：审阅时的实证是，把 insertChildren 里的
// apiserver cidr 写成 "0.0.0.0/0"、把 CreateCluster 的 onboard_state 写成
// "READY"，本包全部测试依旧通过。这两个字段一个是 control-plane Baseline
// 的推导依据、一个决定集群能不能出候选策略。
func TestClusterSurvivesAFullRoundTripThroughMySQL(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	in := sampleCluster()
	// 两个 apiserver：HA 控制面通常不止一个端点，只写一条测不出
	// insertChildren 少插了后面几行这种缺口。
	in.APIServers = []registry.APIServer{
		{Host: "10.9.0.3", CIDR: "10.9.0.16/28", Port: 6443},
		{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
	}
	if err := s.CreateCluster(ctx, registry.Actor{Username: "admin"}, in); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	// 绑定单独写入，但读模型仍然内嵌它：这一趟往返要覆盖的正是
	// 「写路径拆开之后，读回来的形状没变」。
	mustCreateGitRepo(t, s, registry.Actor{Username: "admin"})
	if err := s.BindGitRepo(ctx, registry.Actor{Username: "admin"}, in.ID,
		sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}
	bound := sampleGitBinding()
	in.Git = &bound
	got, ok, err := s.Cluster(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}

	// 期望值里子表按 host / cidr 升序，与写入顺序不同：读路径带
	// ORDER BY，图的是两次读同一个集群必然得到同一份结果 —— 缺了它，
	// 一份候选策略会因为 Baseline 输入的顺序抖动而在两次预览之间变样。
	// 顺序对推导本身无意义（这些网段最终展开成一组规则），但「稳定」有。
	want := in
	want.APIServers = []registry.APIServer{
		{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
		{Host: "10.9.0.3", CIDR: "10.9.0.16/28", Port: 6443},
	}
	want.HealthCheckSources = []string{"130.211.0.0/22", "35.191.0.0/16"}
	// sampleGitBinding 没填 VerifyResult：Go 零值 "" 不是一个登记过的枚举值，
	// 写入时被落成 NOT_VERIFIED（gitbinding.go BindGitRepo），读回来
	// 因此就是 NOT_VERIFIED 而不是空串。
	want.Git.VerifyResult = registry.BindingVerifyNotVerified
	// 数据来源不在写路径上：CreateCluster 不落这一列，新注册的集群一律停在
	// 列默认值 COLLECTED 上（000014）。它出现在 want 里而不是 sampleCluster
	// 里，正是因为它是读回来的登记，不是写进去的输入。
	want.DataSource = registry.DataSourceCollected
	// 策略平面同理，且方向相反：CreateCluster 不落这一列，新注册的集群一律
	// 停在列默认值 **UNKNOWN** 上（000021）——「还没查过」而不是「确认没有」。
	// 这一行是这条纪律在落库层的落点：一个新集群的判定从第一天起就是
	// DEGRADED，直到采集真的探测过一次（design doc 2026-08-25 §2.2）。
	want.OtherPlanes = registry.PlanesUnknown
	// 纳入清单同理：那一列的默认值是 JSON 的 []，读回来是一个**空切片**，
	// 而写进去的是 nil。两者在 DeepEqual 下不相等，而这个差别是刻意的 ——
	// 落库层恒写数组、恒读出数组，让"没人声明过"只有一种形状
	// （managedNamespacesJSON 与 000028 那句 UPDATE 是同一条纪律）。
	want.ManagedSystemNamespaces = []string{}
	// CNI 同理，且理由与 OtherPlanes 一样：CreateCluster 不落这一列，
	// 新注册的集群停在列默认值 **UNKNOWN** 上 ——「还没认过」而不是
	// 「没有 CNI」。每个集群都有 CNI，认不出只说明还没采过一轮。
	want.CNI = cluster.CNIUnknown

	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-tripped cluster =\n%+v\nwant\n%+v", got, want)
	}
	if got.Git == nil || *got.Git != *want.Git {
		t.Errorf("git binding = %+v, want %+v", got.Git, want.Git)
	}
}

// kubeconfigRef 必须走完写、改、列表三条路径，且 kubeconfig 本身一列都不占。
//
// 三条路径分开钉：DeepEqual 那趟往返只经过 CreateCluster 与单条 Cluster()，
// UpdateCluster 的 SET 子句和 Clusters() 的列清单各自是另一份 SQL ——
// 少写一处的症状是「换了凭据、保存成功、采集器仍然拿旧的那一条」，
// 或者「列表里每个集群看起来都没配凭据」，两者都不会报错。
func TestKubeconfigRefIsStoredUpdatedAndListed(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}

	list, err := s.Clusters(ctx)
	if err != nil {
		t.Fatalf("Clusters() error = %v", err)
	}
	if len(list) != 1 || list[0].KubeconfigRef != "prod-asia-1-kubeconfig" {
		t.Errorf("Clusters()[0].KubeconfigRef = %q, want the stored reference", list[0].KubeconfigRef)
	}

	c := sampleCluster()
	c.KubeconfigRef = "prod-asia-1-rotated"
	if err := s.UpdateCluster(ctx, actor, c); err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}
	got, ok, err := s.Cluster(ctx, c.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}
	if got.KubeconfigRef != "prod-asia-1-rotated" {
		t.Errorf("KubeconfigRef after update = %q, want the rotated reference — "+
			"a reference that cannot be changed cannot be rotated", got.KubeconfigRef)
	}

	// 清空必须能表达：凭据被撤销时，平台记得的引用也要能被撤下来。
	c.KubeconfigRef = ""
	if err := s.UpdateCluster(ctx, actor, c); err != nil {
		t.Fatalf("UpdateCluster() clearing the ref error = %v", err)
	}
	if got, _, _ := s.Cluster(ctx, c.ID); got.KubeconfigRef != "" {
		t.Errorf("KubeconfigRef after clearing = %q, want empty", got.KubeconfigRef)
	}

	// 这张表上不得出现一个能装下 kubeconfig 的列。VARCHAR(256) 装不下
	// 一份 kubeconfig，这条断言钉的是「引用而非凭据」这个决定本身：
	// 有人把它改成 TEXT，就是准备往里塞内容了。
	var dataType, colType string
	if err := db.QueryRow(
		`SELECT data_type, column_type FROM information_schema.columns
		  WHERE table_schema = DATABASE() AND table_name = 'cluster'
		    AND column_name = 'kubeconfig_ref'`).Scan(&dataType, &colType); err != nil {
		t.Fatalf("query information_schema for kubeconfig_ref: %v", err)
	}
	if dataType != "varchar" || colType != "varchar(256)" {
		t.Errorf("kubeconfig_ref is %s (%s), want varchar(256): this column holds a Secret "+
			"Manager short name, never the kubeconfig itself", colType, dataType)
	}
}

// verified_at 为 NULL 时必须映射成 nil，不能映射成零值 time.Time：
// 零值是 1970 年的一个真实时间点，任何新鲜度检查都会把它当成
// 「校验过」而不是「从未校验」放行（design doc §3.4 讲的正是这类混淆）。
func TestUnverifiedGitBindingReadsBackAsNilNotZeroTime(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	in := sampleCluster()
	if err := s.CreateCluster(ctx, registry.Actor{Username: "admin"}, in); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	// 绑定不带 VerifiedAt / VerifyResult：刚绑上、还没跑过校验。
	mustCreateGitRepo(t, s, registry.Actor{Username: "admin"})
	if err := s.BindGitRepo(ctx, registry.Actor{Username: "admin"}, in.ID,
		sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}
	got, ok, err := s.Cluster(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}
	if got.Git == nil {
		t.Fatal("Git binding missing after round trip")
	}
	if got.Git.VerifiedAt != nil {
		t.Errorf("VerifiedAt = %v, want nil for a never-verified binding", got.Git.VerifiedAt)
	}
	if got.Git.VerifyResult != registry.BindingVerifyNotVerified {
		t.Errorf("VerifyResult = %q, want %q", got.Git.VerifyResult, registry.BindingVerifyNotVerified)
	}
}

// 校验结论与时间戳的往返：写入 OK 加一个具体时间戳，读回逐字相等。
func TestVerifiedGitBindingRoundTripsResultAndTimestamp(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	verifiedAt := time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	actor := registry.Actor{Username: "admin"}
	in := sampleCluster()
	if err := s.CreateCluster(ctx, actor, in); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	mustCreateGitRepo(t, s, actor)
	if err := s.BindGitRepo(ctx, actor, in.ID, sampleGitBinding()); err != nil {
		t.Fatalf("BindGitRepo() error = %v", err)
	}
	if err := s.SetGitVerifyResult(ctx, actor, in.ID, registry.BindingVerifyOK, verifiedAt); err != nil {
		t.Fatalf("SetGitVerifyResult() error = %v", err)
	}

	got, ok, err := s.Cluster(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}
	if got.Git == nil {
		t.Fatal("Git binding missing after round trip")
	}
	if got.Git.VerifyResult != registry.BindingVerifyOK {
		t.Errorf("VerifyResult = %q, want %q", got.Git.VerifyResult, registry.BindingVerifyOK)
	}
	if got.Git.VerifiedAt == nil || !got.Git.VerifiedAt.Equal(verifiedAt) {
		t.Errorf("VerifiedAt = %v, want %v", got.Git.VerifiedAt, verifiedAt)
	}
}

// 数据来源是库里**登记的一列**，读路径原样带出来，不由「有没有采集数据」推断。
//
// 这个测试要挡的是推断的两个方向，各自都会造成事故：
//
//   - 「有采集数据 ⇒ COLLECTED」：一个登记为 FIXTURE 的演示集群一旦被人跑过
//     一次采集，就会被当成真集群，演示环境在没人改代码的情况下自己坏掉。
//   - 「没采集数据 ⇒ FIXTURE」：一次采集故障会让一个真集群悄悄变回演示集群，
//     操作者据一份虚构的报告批准下发（design doc 2026-08-17 §2）。
//
// 因此两个集群一起断言，且**故意反着造**：带采集记录的那个登记为 FIXTURE，
// 一条采集记录都没有的那个登记为 COLLECTED。任何一种推断都会把这两行翻过来。
//
// 单条 Cluster() 与列表 Clusters() 都要断言：它们是两份各自维护的 SQL，
// 而一份列错了列的读路径不会报错，只会把另一个集群的登记安在这个集群头上
// （事实层列映射那次的教训）。两行的取值互不相同，一个把整列读成常量的
// 实现也过不去。
func TestClusterDataSourceIsReadFromTheDeclarationNotInferred(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	// 真集群：走正规注册路径，落在列默认值 COLLECTED 上，且一条采集记录都没有。
	collected := sampleCluster()
	if err := s.CreateCluster(ctx, actor, collected); err != nil {
		t.Fatalf("CreateCluster(%s) error = %v", collected.ID, err)
	}

	// 演示集群：来源由库登记（000014 对种子行做的正是这一句），并且**有**采集记录。
	demo := sampleCluster()
	demo.ID = "prod-eu-1"
	demo.DisplayName = "EU Prod"
	demo.KubeconfigRef = "prod-eu-1-kubeconfig"
	if err := s.CreateCluster(ctx, actor, demo); err != nil {
		t.Fatalf("CreateCluster(%s) error = %v", demo.ID, err)
	}
	if _, err := db.Exec(
		`UPDATE cluster SET data_source = ? WHERE cluster_id = ?`,
		string(registry.DataSourceFixture), demo.ID); err != nil {
		t.Fatalf("declare %s as FIXTURE: %v", demo.ID, err)
	}
	if _, err := db.Exec(
		`INSERT INTO collection_run
		   (cluster_id, run_id, observed_at, started_at, finished_at, status)
		 VALUES (?, 'run-1', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 'OK')`,
		demo.ID); err != nil {
		t.Fatalf("insert collection_run for %s: %v", demo.ID, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM collection_run`) })

	want := map[string]registry.DataSource{
		collected.ID: registry.DataSourceCollected,
		demo.ID:      registry.DataSourceFixture,
	}

	for id, wantSource := range want {
		got, ok, err := s.Cluster(ctx, id)
		if err != nil || !ok {
			t.Fatalf("Cluster(%s) = %+v, %v, %v", id, got, ok, err)
		}
		if got.DataSource != wantSource {
			t.Errorf("Cluster(%s).DataSource = %q, want the declared %q",
				id, got.DataSource, wantSource)
		}
	}

	list, err := s.Clusters(ctx)
	if err != nil {
		t.Fatalf("Clusters() error = %v", err)
	}
	if len(list) != len(want) {
		t.Fatalf("Clusters() returned %d clusters, want %d", len(list), len(want))
	}
	for _, c := range list {
		if c.DataSource != want[c.ID] {
			t.Errorf("Clusters()[%s].DataSource = %q, want the declared %q",
				c.ID, c.DataSource, want[c.ID])
		}
	}
}

// 集群的写路径不碰 data_source：带着一个值也不落库。
//
// 与 c.Git 同一条纪律。把一个真集群改成 FIXTURE 恰好是本轮要防的那个最坏
// 结果的手动版本，而平台没有任何针对它的授权与审计设计（规范 §7、§28）；
// 顺带也挡住「改一次网段把来源一起换掉」这种没人打算做的改动。
func TestUpdateClusterDoesNotRewriteTheDeclaredDataSource(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	in := sampleCluster()
	if err := s.CreateCluster(ctx, actor, in); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if _, err := db.Exec(
		`UPDATE cluster SET data_source = ? WHERE cluster_id = ?`,
		string(registry.DataSourceFixture), in.ID); err != nil {
		t.Fatalf("declare %s as FIXTURE: %v", in.ID, err)
	}

	// 请求体里带一个相反的来源：必须被忽略。
	mutated := sampleCluster()
	mutated.DataSource = registry.DataSourceCollected
	mutated.DisplayName = "Asia Prod (renamed)"
	if err := s.UpdateCluster(ctx, actor, mutated); err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}

	got, ok, err := s.Cluster(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}
	if got.DataSource != registry.DataSourceFixture {
		t.Errorf("DataSource = %q after an update that asked for %q, want the declaration untouched",
			got.DataSource, registry.DataSourceCollected)
	}
	// 对照：这次更新确实生效了。少了它，一个整体不生效的 UpdateCluster
	// 也能让上面那条通过。
	if got.DisplayName != "Asia Prod (renamed)" {
		t.Errorf("DisplayName = %q, want the update to have taken effect", got.DisplayName)
	}
}

// metrics 抓取端登记必须原样往返。
//
// 它是 METRICS_SCRAPE Baseline 依据的一半，而这一半观测不出来
// （design doc 2026-08-18-metrics-scrape-evidence §3.2）。静默丢掉它的症状是：
// 运维在页面上登记了抓取端，那一类 Baseline 却仍然报缺失，而没有任何东西
// 指向登记没落库。
func TestMetricsScrapersSurviveTheRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	actor := registry.Actor{Username: "admin"}

	c := sampleCluster()
	c.MetricsScrapers = []registry.MetricsScraper{
		{Namespace: "monitoring", Labels: map[string]string{
			"app.kubernetes.io/name": "prometheus", "release": "kube-prometheus-stack",
		}},
		{Namespace: "observability", Labels: map[string]string{"app": "vmagent"}},
	}
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}

	got, ok, err := s.Cluster(ctx, c.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = (_, %v, %v)", ok, err)
	}
	if len(got.MetricsScrapers) != 2 {
		t.Fatalf("MetricsScrapers = %+v, want 2", got.MetricsScrapers)
	}
	// 一个集群可以有多个抓取端：Prometheus 与一个 agent 型采集器并存是
	// 常见形态。少了这一条，第二个会静默覆盖第一个。
	if got.MetricsScrapers[0].Namespace != "monitoring" {
		t.Errorf("first scraper namespace = %q, want monitoring", got.MetricsScrapers[0].Namespace)
	}
	if got.MetricsScrapers[0].Labels["release"] != "kube-prometheus-stack" {
		t.Errorf("labels round-tripped as %v", got.MetricsScrapers[0].Labels)
	}
}

func TestUpdateClusterReplacesMetricsScrapers(t *testing.T) {
	// 整体替换语义与 apiServers / healthCheckSources 一致：一次 PUT 描述的
	// 是"这个集群现在有哪些抓取端"，而不是"再加一个"。
	ctx := context.Background()
	s, _ := newTestStore(t)
	actor := registry.Actor{Username: "admin"}

	c := sampleCluster()
	c.MetricsScrapers = []registry.MetricsScraper{
		{Namespace: "monitoring", Labels: map[string]string{"app": "prometheus"}},
	}
	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	c.MetricsScrapers = nil
	if err := s.UpdateCluster(ctx, actor, c); err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}
	got, _, _ := s.Cluster(ctx, c.ID)
	if len(got.MetricsScrapers) != 0 {
		t.Errorf("MetricsScrapers = %+v after removing them, want none", got.MetricsScrapers)
	}
}

// 探测结论连续两轮相同是常态，因此写入必须幂等（2026-08-25 真集群上发现）。
//
// MySQL 对一次值没有变化的 UPDATE 报 0 行受影响。把 0 直接当成「这个集群
// 不存在」，每一轮采集都会记一条"写不下去"的告警 —— 而没有人会一直看
// 一类每轮都出现的告警。
func TestSetOtherPlanesIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateCluster(ctx, registry.Actor{Username: "admin"}, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}

	for i := range 2 {
		if err := s.SetOtherPlanes(ctx, "prod-asia-1", registry.PlanesNone); err != nil {
			t.Fatalf("第 %d 次写入 = %v, want nil", i+1, err)
		}
	}
	got, ok, err := s.Cluster(ctx, "prod-asia-1")
	if err != nil || !ok {
		t.Fatalf("Cluster() = %v, %v", ok, err)
	}
	if got.OtherPlanes != registry.PlanesNone {
		t.Errorf("OtherPlanes = %q, want %q", got.OtherPlanes, registry.PlanesNone)
	}
	// 不存在的集群仍然要报 ErrNotFound —— 上面那条放宽不能把它一起放过。
	if err := s.SetOtherPlanes(ctx, "no-such-cluster", registry.PlanesNone); !errors.Is(
		err, registry.ErrNotFound) {
		t.Errorf("对不存在的集群写入 = %v, want ErrNotFound", err)
	}
}
