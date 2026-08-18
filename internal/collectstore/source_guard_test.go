package collectstore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

// 本文件补的是"两道各自独立"的**反方向**那一半。
//
// 正方向（一个 COLLECTED 集群拿不到合成数据）在 cmd/distill-api 有探针；
// 反方向 —— 一个登记为 FIXTURE 的集群不得从采集数据里得到答案 —— 到本轮为止
// 全仓零测试：`testSource()` 把两个测试集群**都**登记成 COLLECTED，于是把
// collectstore.go 里那句 `c.DataSource != registry.DataSourceCollected` 删掉，
// 整套测试仍然全绿（branch review I2）。
//
// 这道守卫在本轮之前不承重（collectstore 没有生产调用方），从这次接线起开始
// 承重，所以欠的测试算在这次合并头上。
//
// 危害的形态：装配层那个 switch 若被拨反，一个登记为 FIXTURE、而 ID 恰好在
// 采集事实表里也存在的集群（同名重注册、demo 与真集群撞 ID），会拿到一份来自
// MySQL 的真实数据、挂着演示集群的标签与"演示数据"的来源标识。方向与主防线
// 相反，但同样是"标识与内容不是一回事"。

// fixtureDeclaredID 是一个登记为 FIXTURE 的集群 ID。
//
// 刻意与 collectedID 用同一种命名：这条性质要挡的正是"ID 在事实层里也存在"
// 那种情形，用一个明显不可能有数据的 ID 会让用例失去意义。
const fixtureDeclaredID = "col-read-a"

// errFactsUnreachable 是事实层这一侧的可观测标记。
//
// 用一个连不上的 *sql.DB 而不是 nil：nil 上取数是 panic，两种失败区分不开。
// 连不上的库让"守卫拦下了"与"守卫放行、去读了事实层"成为两个可以断言的
// 不同结果 —— 对照组因此不需要一个真实的 MySQL，这条测试跑在 `make check`
// 里，而不是只跑在 `make test-integration` 里。
var errFactsUnreachable = errors.New("facts layer deliberately unreachable in this test")

type unreachableConnector struct{}

func (unreachableConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errFactsUnreachable
}

func (unreachableConnector) Driver() driver.Driver { return unreachableDriver{} }

type unreachableDriver struct{}

func (unreachableDriver) Open(string) (driver.Conn, error) { return nil, errFactsUnreachable }

// sourceGuardCase 是"这个读方法确实先确认了登记的来源"在某一个方法上的落点。
//
// refusal 返回一句「泄漏了什么」，空串表示这个读方法确实拒绝了。用字符串而
// 不是 error：Flow 的拒绝形态是 ok=false、err=nil，与其余几个断言不到同一个
// error 上（与 cmd/distill-api 的 leakCase 同一形态、同一理由）。
type sourceGuardCase struct {
	method  string
	name    string
	refusal func(r *collectstore.Reader, clusterID string) string
}

// wantClusterNotFound 把「必须报 ErrClusterNotFound」翻译成 refusal 要的描述。
//
// 特意要求**这个**哨兵而不是"随便什么错误"：事实层连不上时返回的也是错误，
// 而那种绿是假的 —— 它证明的是库连不上，不是守卫拦下了。
func wantClusterNotFound(err error) string {
	if errors.Is(err, store.ErrClusterNotFound) {
		return ""
	}
	return fmt.Sprintf("error = %v, want ErrClusterNotFound", err)
}

func sourceGuardCases(ctx context.Context, window store.TimeWindow) []sourceGuardCase {
	return []sourceGuardCase{
		{method: "DefaultWindow", name: "DefaultWindow", refusal: func(r *collectstore.Reader, id string) string {
			_, err := r.DefaultWindow(ctx, id)
			return wantClusterNotFound(err)
		}},
		{method: "Topology", name: "Topology", refusal: func(r *collectstore.Reader, id string) string {
			_, err := r.Topology(ctx, id, store.LevelNamespace)
			return wantClusterNotFound(err)
		}},
		{method: "Quality", name: "Quality", refusal: func(r *collectstore.Reader, id string) string {
			_, err := r.Quality(ctx, id)
			return wantClusterNotFound(err)
		}},
		{method: "Security", name: "Security", refusal: func(r *collectstore.Reader, id string) string {
			_, err := r.Security(ctx, id, window)
			return wantClusterNotFound(err)
		}},
		{method: "PolicyPreviewAtGranularity", name: "PolicyPreviewAtGranularity",
			refusal: func(r *collectstore.Reader, id string) string {
				_, err := r.PolicyPreviewAtGranularity(ctx, id, "payment", window,
					policygen.GranularityNamespace)
				return wantClusterNotFound(err)
			}},
		{method: "Flows", name: "Flows(cluster named)", refusal: func(r *collectstore.Reader, id string) string {
			_, err := r.Flows(ctx, store.FlowFilter{Cluster: id, Window: window})
			return wantClusterNotFound(err)
		}},
		{method: "Flow", name: "Flow(id naming the cluster)", refusal: func(r *collectstore.Reader, id string) string {
			// 拒绝形态是 ok=false（见 collectstore.Flow 的注释：集群不存在、
			// ID 解不开、指纹对不上合并成同一个响应，调用方才无法靠响应差异
			// 探出一个自己看不见的集群是否存在）。
			_, ok, err := r.Flow(ctx, collectedShapedFlowID(id, window))
			if err != nil {
				return fmt.Sprintf("unexpected error %v", err)
			}
			if ok {
				return "resolved a decision for a cluster declared FIXTURE"
			}
			return ""
		}},
		{method: "EnsureRuleExists", name: "EnsureRuleExists", refusal: func(r *collectstore.Reader, id string) string {
			// 这一个不经过来源门禁：它无条件拒绝（notyet.go），因此对任何
			// 来源都交不出答案。判据相应放宽成"拒绝了"，而"它对 COLLECTED
			// 也一样拒绝"由 TestEnsureRuleExistsRefusesRegardlessOfSource
			// 单独钉住 —— 放宽必须有一条断言撑着，不能只是表里的一行豁免。
			err := r.EnsureRuleExists(ctx, id, "payment", "payment", "deadbeef",
				policygen.DecisionDisable, window)
			if err == nil {
				return "accepted a rule check for a cluster declared FIXTURE"
			}
			return ""
		}},
	}
}

// 一个登记为 FIXTURE 的集群，在采集侧 Reader 上一个读方法都答不出来。
//
// 对照组用**同一个 Reader、同一个连不上的事实层**去问一个登记为 COLLECTED 的
// 集群：那一侧必须走到事实层（errFactsUnreachable），证明拒绝来自来源门禁，
// 而不是"这个 Reader 什么都答不出来"。少了对照组，一个坏掉的 Reader 也能让
// 上半全绿。
func TestAFixtureClusterIsNotAnswerableFromCollectedData(t *testing.T) {
	src := stubSource{clusters: []registry.Cluster{
		{ID: fixtureDeclaredID, DataSource: registry.DataSourceFixture,
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20"},
		{ID: silentID, DataSource: registry.DataSourceCollected,
			PodCIDR: "10.6.0.0/14", NodeCIDR: "10.130.0.0/20"},
	}}
	db := sql.OpenDB(unreachableConnector{})
	defer func() { _ = db.Close() }()
	r := collectstore.New(db, src)

	ctx := t.Context()
	window := store.TimeWindow{
		From: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC),
	}
	cases := sourceGuardCases(ctx, window)

	// 先钉住覆盖面：新增第八个读方法却忘记在这里补一格，表现必须是这里红并
	// 点名那个方法，而不是沉默地少测一格。按 reflect 取接口方法名，不写清单 ——
	// 这个项目两次漏网都出在手写表格上。
	assertEveryReaderMethodCovered(t, cases)

	for _, tc := range cases {
		if leak := tc.refusal(r, fixtureDeclaredID); leak != "" {
			t.Errorf("%s [%s]: %s — the collected reader must not answer for a cluster "+
				"declared FIXTURE (design doc 2026-08-18 §2, mirror half)", tc.name, tc.method, leak)
		}
	}

	// 对照组：同一个 Reader 对登记为 COLLECTED 的集群会**走到事实层**。
	// 拿 Quality 一格即可 —— 要证的是门禁放行了，不是每个方法各自的取数。
	_, err := r.Quality(ctx, silentID)
	if errors.Is(err, store.ErrClusterNotFound) {
		t.Errorf("Quality(%s declared COLLECTED) = ErrClusterNotFound; the guard is refusing "+
			"everything, so the assertions above prove nothing", silentID)
	}
	if !errors.Is(err, errFactsUnreachable) {
		t.Errorf("Quality(%s declared COLLECTED) error = %v, want the facts layer to have been "+
			"reached (%v)", silentID, err, errFactsUnreachable)
	}
}

// EnsureRuleExists 对**两种**来源都拒绝。
//
// 上一条表里给了它一格放宽的判据（"拒绝了即可"，而不是 ErrClusterNotFound），
// 理由是它压根不经过来源门禁。这条测试是那次放宽的凭据：它一旦开始对
// COLLECTED 放行，放宽就不再成立，而这里会红 —— 豁免靠断言撑着，不是靠表里
// 的一行字。
func TestEnsureRuleExistsRefusesRegardlessOfSource(t *testing.T) {
	src := stubSource{clusters: []registry.Cluster{
		{ID: fixtureDeclaredID, DataSource: registry.DataSourceFixture},
		{ID: silentID, DataSource: registry.DataSourceCollected},
	}}
	db := sql.OpenDB(unreachableConnector{})
	defer func() { _ = db.Close() }()
	r := collectstore.New(db, src)
	window := store.TimeWindow{
		From: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC),
	}

	for _, id := range []string{fixtureDeclaredID, silentID} {
		err := r.EnsureRuleExists(t.Context(), id, "payment", "payment", "deadbeef",
			policygen.DecisionDisable, window)
		if err == nil {
			t.Errorf("EnsureRuleExists(%s) = nil; the write-path precheck is not backed by "+
				"collected data yet and must refuse (安全规范 §49，失败方向朝关)", id)
		}
	}
}

// collectedShapedFlowID 造一个采集侧形状、点名 clusterID 的流量 ID。
//
// 手写编码是不得已：flowIDOf 是包内的，外部测试包拿不到。因此
// TestCollectedShapedFlowIDProbeIsGenuine 每次都用导出的 ClusterOfFlowID
// 自检一遍 —— 编码一旦改动，探针会当场失败，而不是悄悄退化成一个"解不开的
// ID"，让 Flow 那一格改去测另一条早退路径并照常变绿。
func collectedShapedFlowID(clusterID string, w store.TimeWindow) string {
	payload := strings.Join([]string{
		clusterID,
		strconv.FormatInt(w.From.UTC().UnixMicro(), 10),
		strconv.FormatInt(w.To.UTC().UnixMicro(), 10),
		"0",
		"0123456789abcdef",
	}, "\x1f")
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// 探针本身必须是一个真正的采集侧 ID。
//
// 单列一条测试：探针失效的症状是"用例测的不是它以为在测的那条路"，而那种
// 失败必须自己有名字，否则它只会表现为另一条用例莫名其妙地变绿。
func TestCollectedShapedFlowIDProbeIsGenuine(t *testing.T) {
	w := store.TimeWindow{
		From: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC),
	}
	got, ok := collectstore.ClusterOfFlowID(collectedShapedFlowID(fixtureDeclaredID, w))
	if !ok || got != fixtureDeclaredID {
		t.Fatalf("probe decodes to (%q, %v), want (%q, true); the Flow case would exercise "+
			"the unparseable-id early return instead of the source guard", got, ok, fixtureDeclaredID)
	}
}

// assertEveryReaderMethodCovered 要求 store.Reader 的每个方法在表里都有一格。
//
// 按接口反射取方法名，不写清单：漏掉一格不会让任何东西变红，而这个项目两次
// 漏网都是这样发生的。反向也对账 —— 表里出现一个接口上没有的方法名，多半是
// 重构改了方法名而这一格被留在原地，那一格从此测的是一个不存在的入口。
func assertEveryReaderMethodCovered(t *testing.T, cases []sourceGuardCase) {
	t.Helper()
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.method] = true
	}
	iface := reflect.TypeOf((*store.Reader)(nil)).Elem()
	declared := map[string]bool{}
	var missing []string
	for i := range iface.NumMethod() {
		name := iface.Method(i).Name
		declared[name] = true
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("store.Reader methods with no FIXTURE-cluster case: %v — every read method "+
			"needs one; a method merely absent from this table leaks silently", missing)
	}
	for _, c := range cases {
		if !declared[c.method] {
			t.Errorf("case %q names method %q, which store.Reader does not declare", c.name, c.method)
		}
	}
}
