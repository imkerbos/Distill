package httpapi_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// 这条用例是 cmd/distill-agent/agent_wire_fanout_test.go 的另一半。那一份
// 钉住"agent 有没有把快照字段发出去"，这一份钉住"平台有没有把发过来的
// 字段收进 snapshot.Run"——两半合起来才是一次完整的往返。只守发送侧的话，
// toRun() 漏读一个已经在报文里的字段照样不会有任何症状：JSON 解码不出错，
// 只是那一列在落库时悄悄变回零值。
//
// 判据同上：反射出快照类型的全部字段，在 toRun() 里对应的 for-range 循环
// 体内找 <循环变量>.<字段名>——用循环变量名与 range 表达式的源码文本定位
// 那个循环，不是整个函数体，因为 toRun() 里九个循环全部管这一件事，循环
// 变量重名是常态（大多数都叫 in）。
//
// 豁免表与发送侧那一份保持同一套字段与理由：一个字段要么两侧都带，
// 要么两侧都记在案，不该出现"agent 发了、平台没收"或反过来的半吊子状态。

type wireFanoutSite struct {
	label     string
	fields    []string
	rangeExpr string
	src       string
	exempt    map[string]string
}

const toRunFile = "agent_run_handler.go"
const toRunFunc = "toRun"

const clusterIDExempt = "集群归属只来自 token（design doc §2），由调用方在 toRun 里逐条填上，" +
	"不从报文读。报文里带着这个字段会先在 decodeAgentRun 那一步就被 DisallowUnknownFields 拒绝，" +
	"根本到不了 toRun。"

const preExistingGapExempt = "既有缺口，与本轮改动（LoadBalancer 暴露判定、命名端口）无关：" +
	"agent 报文本来就没有这个字段（见 cmd/distill-agent 那一侧的同一条豁免），" +
	"toRun 自然也读不到它。本轮不补，先记下来。"

func wireFanoutSites() []wireFanoutSite {
	return []wireFanoutSite{
		{
			label:     toRunFile + ":" + toRunFunc + " (Namespace)",
			fields:    exportedFields(snapshot.Namespace{}),
			rangeExpr: "p.Observation.Namespaces",
			src:       "in",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (Node)",
			fields:    exportedFields(snapshot.Node{}),
			rangeExpr: "p.Observation.Nodes",
			src:       "in",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (Service)",
			fields:    exportedFields(snapshot.Service{}),
			rangeExpr: "p.Observation.Services",
			src:       "in",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (ServicePort)",
			fields:    exportedFields(snapshot.ServicePort{}),
			rangeExpr: "in.Ports",
			src:       "sp",
			exempt:    nil,
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (Endpoints)",
			fields:    exportedFields(snapshot.Endpoints{}),
			rangeExpr: "p.Observation.Endpoints",
			src:       "in",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (NetworkPolicy)",
			fields:    exportedFields(snapshot.NetworkPolicy{}),
			rangeExpr: "p.Observation.Policies",
			src:       "in",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (Gateway)",
			fields:    exportedFields(snapshot.Gateway{}),
			rangeExpr: "p.Observation.Gateways",
			src:       "in",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (Warning)",
			fields:    exportedFields(snapshot.Warning{}),
			rangeExpr: "p.Observation.Warnings",
			src:       "in",
			exempt:    nil,
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (AdminPolicy)",
			fields:    exportedFields(snapshot.AdminPolicy{}),
			rangeExpr: "p.Observation.AdminPolicies",
			src:       "a",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (Pod)",
			fields:    exportedFields(snapshot.Pod{}),
			rangeExpr: "p.Observation.Pods",
			src:       "in",
			exempt: map[string]string{
				"ClusterID": clusterIDExempt,
				"IPScope": "归属是平台的判定（design doc §3.4），发生在 Classify() 里，" +
					"不是从这次报文解码出来的——toRun 只负责把报文变成 Run，Classify 在它之后另外一步跑。",
				"IPScopeReason":     "同 IPScope。",
				"ExtraIPs":          preExistingGapExempt,
				"InMesh":            preExistingGapExempt,
				"MeshSource":        preExistingGapExempt,
				"MeshDetail":        preExistingGapExempt,
				"ScrapeAnnotations": preExistingGapExempt,
			},
		},
		{
			label:     toRunFile + ":" + toRunFunc + " (NamedPort)",
			fields:    exportedFields(snapshot.NamedPort{}),
			rangeExpr: "in.NamedPorts",
			src:       "np",
			exempt:    nil,
		},
	}
}

func TestAgentWireCarriesEverySnapshotFieldIntoTheRun(t *testing.T) {
	for _, site := range wireFanoutSites() {
		t.Run(site.label, func(t *testing.T) {
			read := fieldsReadInLoop(t, toRunFile, toRunFunc, site.rangeExpr, site.src)
			for _, f := range site.fields {
				if read[f] {
					if reason, ok := site.exempt[f]; ok {
						t.Errorf("%s 已经读了 %s，但豁免表里还写着「%s」——"+
							"过期的豁免会掩护下一次遗漏", site.label, f, reason)
					}
					continue
				}
				if reason, ok := site.exempt[f]; ok {
					if strings.TrimSpace(reason) == "" {
						t.Errorf("%s 豁免了 %s 却没写理由", site.label, f)
					}
					continue
				}
				t.Errorf("%s 没有把 %s 从报文读进 snapshot.Run。\n"+
					"要么在这里把它读出来，要么在 wireFanoutSites 的 exempt 里写下为什么不带。\n"+
					"漏掉它不会编译失败，也不会有任何症状——那一列在落库时悄悄变回零值，"+
					"哪怕 agent 明明把它发过来了。", site.label, f)
			}
			for f := range site.exempt {
				if !containsString(site.fields, f) {
					t.Errorf("%s 的豁免表里的 %s 不再是这个类型的字段了，删掉这一条", site.label, f)
				}
			}
		})
	}
}

// exportedFields 反射出一个类型的全部导出字段名，按名字排序。
func exportedFields(v any) []string {
	rt := reflect.TypeOf(v)
	out := make([]string, 0, rt.NumField())
	for i := range rt.NumField() {
		if f := rt.Field(i); f.IsExported() {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// fieldsReadInLoop 解析 file 里的函数 fn，定位其中形如
// `for _, <src> := range <rangeExpr>` 的循环，收集循环体内出现过的
// <src>.<字段名> 选择表达式。
//
// **必须精确到某一个循环，不能扫整个函数体**：toRun() 里九个循环大多共用
// 循环变量名 in，扫整个函数体会把"在别的循环里读过同名字段"误判成"这个
// 循环读过"，而不同循环对应的是完全不同的报文类型（比如 Namespace 与
// Endpoints 都有 Name 字段）。
func fieldsReadInLoop(t *testing.T, file, fn, rangeExpr, src string) map[string]bool {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", file, err)
	}

	var body *ast.BlockStmt
	ast.Inspect(parsed, func(n ast.Node) bool {
		f, ok := n.(*ast.FuncDecl)
		if ok && f.Name.Name == fn && f.Body != nil {
			body = f.Body
			return false
		}
		return true
	})
	if body == nil {
		t.Fatalf("%s 里找不到函数 %s —— 抄写点被移动或改名了，wireFanoutSites 要跟着改", file, fn)
	}

	var loop *ast.RangeStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if loop != nil {
			return false
		}
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		ident, ok := rs.Value.(*ast.Ident)
		if !ok || ident.Name != src {
			return true
		}
		if exprSource(rs.X) != rangeExpr {
			return true
		}
		loop = rs
		return false
	})
	if loop == nil {
		t.Fatalf("%s 的 %s 里找不到 `for _, %s := range %s` —— 抄写点被移动或改写了，"+
			"wireFanoutSites 要跟着改", file, fn, src, rangeExpr)
	}

	read := map[string]bool{}
	ast.Inspect(loop.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == src {
			read[sel.Sel.Name] = true
		}
		return true
	})
	if len(read) == 0 {
		t.Fatalf("%s 的 %s 里，`%s := range %s` 那个循环体一次都没读过 %s.*——"+
			"这条用例已经什么都不在守了", file, fn, src, rangeExpr, src)
	}
	return read
}

// exprSource 把一个只含标识符与选择表达式的 AST 渲染回点号连接的源码文本，
// 如 p.Observation.Pods。够用：这个文件里出现的 range 表达式都是这个形状。
func exprSource(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprSource(v.X) + "." + v.Sel.Name
	default:
		return ""
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
