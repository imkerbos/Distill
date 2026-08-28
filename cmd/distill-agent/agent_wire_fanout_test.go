package main

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

// agentPodPayload 与 agentServicePayload 是手写的、与 internal/snapshot
// 逐字段对应的镜像类型——给 snapshot.Service 或 snapshot.Pod 加一个字段，
// 编译器不会提醒任何人把它也搬到这两个类型、搬进 Save() 里去。漏掉的后果
// 不是编译失败，是那个字段在推送式接入下永远到不了平台：数据库里那一列
// 恒为 NULL，界面上的判定恒是"看不出结论"，而这条链路本身一切正常。
//
// **这正是 policygen.Result 那五处遗漏的同一个 bug 形状**（见
// internal/policygen/result_fanout_test.go），换了一个边界：那边是纯函数
// 之间的抄写，这边是 agent 进程与平台进程之间的一次 HTTP 往返。第一次真的
// 发生在这个边界上的是 LoadBalancerIngressIPs / LoadBalancerSourceRanges /
// Pod.NamedPorts——加了字段、agent 采到了、报文却没带，PUSH 模式下入口
// 判定整体退化成"看不出结论"。局部补上这三个字段解决不了这一类问题，
// 只有把"抄没抄全"变成一条反射出来的用例才能。
//
// 判据是反射出来的，不是手抄的清单：给 snapshot.Service / snapshot.Pod
// 加一个字段，这里立刻多一项要求，加字段的人被迫走到下面的豁免表前，
// 写清楚为什么这一处不带。
//
// 判"带没带"看的是源码，按抄写点的形状分两种定位：
//
//   - **元素类型**（Pod、Service、Failure……）在 Save() 里各有一个 for-range
//     循环。用循环变量名与 range 表达式的源码文本定位到那**一个**循环，不是
//     整个函数体 —— 这个函数里好几个循环变量重名（Namespaces 与 Nodes 都叫
//     n），混在一起扫会把"读过 Namespace.Name"误判成"读过 Node.Name"。
//
//   - **聚合类型**（snapshot.Run 与 snapshot.Observation 本身）没有循环。
//     它们的字段直接写成 run.X / run.Observation.X，因此按根表达式扫整个
//     函数体。
//
// **第二种是补上去的，而它的缺席正是这条用例第一次没守住的原因。**
// 只锚在 for-range 上时，这份清单覆盖了 11 个元素类型，却把产生它们的那
// 两个聚合漏在外面 —— 于是 Run.Failures（采集失败记录）与
// Observation.ForeignScopes / ForeignScopesComplete（第二策略平面的覆盖
// 范围）从来没有过线，而两个独立评审各自撞上了同一个盲区。前者让
// collection_run_failure 在推送模式下恒空，"Service 列表被 403 拒了"于是
// 读成"我们看过了，这个集群就是没有 Service"；后者让完整度标志恒为
// 零值。d36c94d 的提交信息说这条用例"要求每个字段要么被带上、要么写进
// 豁免表"，那句话当时只对元素类型成立（design review RI2 / SC1，2026-08-28）。

// wireFanoutSite 是快照类型到 agent 报文的一个抄写点。
//
// 两种定位二选一：给了 rootExpr 就按根表达式扫整个函数体（聚合类型），
// 否则按 rangeExpr + src 定位到一个循环（元素类型）。
type wireFanoutSite struct {
	// label 是失败信息里的可读标识。
	label string
	// fields 是被抄写的快照类型的全部导出字段名，反射得到。
	fields []string
	// rangeExpr 是定位循环的 range 表达式源码文本，如 "run.Observation.Pods"。
	rangeExpr string
	// src 是循环变量名。
	src string
	// rootExpr 是聚合类型在函数体里的根表达式，如 "run" 或 "run.Observation"。
	// 非空时忽略 rangeExpr / src。
	rootExpr string
	// exempt 是故意不带的字段，值是理由。空表示这一处必须全带。
	exempt map[string]string
}

const sinkFile = "sink.go"
const sinkFunc = "Save"

// clusterIDExempt 是全部资产类记录共用的一条豁免：集群归属只来自 agent
// 的认证身份（token），不来自报文内容——报文带上它会被平台的
// DisallowUnknownFields 整体拒绝。TestSinkNeverClaimsACluster 独立钉住
// 了"agent 侧确实没发"这一半。
const clusterIDExempt = "集群归属只来自 token（design doc §2），报文里没有这个字段的容身之处；" +
	"报文带上会被平台的 DisallowUnknownFields 整体拒绝。TestSinkNeverClaimsACluster 独立钉住了这一点。"

func wireFanoutSites() []wireFanoutSite {
	return []wireFanoutSite{
		{
			label:    sinkFile + ":" + sinkFunc + " (Run)",
			fields:   exportedFields(snapshot.Run{}),
			rootExpr: "run",
			exempt: map[string]string{
				"ErrorReason": "中止的运行走 SaveAbortedRun 那条报文（它自己带 errorReason）；" +
					"Save 收到的运行 ErrorReason 恒为空。在这里也发一遍，等于给" +
					"「失败却带着资产」这种自相矛盾的报文多开一个入口，而平台正是靠" +
					"errorReason 非空来判断该走哪条落库路径。",
			},
		},
		{
			label:    sinkFile + ":" + sinkFunc + " (Observation)",
			fields:   exportedFields(snapshot.Observation{}),
			rootExpr: "run.Observation",
			exempt:   map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Namespace)",
			fields:    exportedFields(snapshot.Namespace{}),
			rangeExpr: "run.Observation.Namespaces",
			src:       "n",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Node)",
			fields:    exportedFields(snapshot.Node{}),
			rangeExpr: "run.Observation.Nodes",
			src:       "n",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Service)",
			fields:    exportedFields(snapshot.Service{}),
			rangeExpr: "run.Observation.Services",
			src:       "svc",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (ServicePort)",
			fields:    exportedFields(snapshot.ServicePort{}),
			rangeExpr: "svc.Ports",
			src:       "sp",
			exempt:    nil,
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Endpoints)",
			fields:    exportedFields(snapshot.Endpoints{}),
			rangeExpr: "run.Observation.Endpoints",
			src:       "e",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (NetworkPolicy)",
			fields:    exportedFields(snapshot.NetworkPolicy{}),
			rangeExpr: "run.Observation.Policies",
			src:       "pol",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (AdminPolicy)",
			fields:    exportedFields(snapshot.AdminPolicy{}),
			rangeExpr: "run.Observation.AdminPolicies",
			src:       "a",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Gateway)",
			fields:    exportedFields(snapshot.Gateway{}),
			rangeExpr: "run.Observation.Gateways",
			src:       "g",
			exempt:    map[string]string{"ClusterID": clusterIDExempt},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Warning)",
			fields:    exportedFields(snapshot.Warning{}),
			rangeExpr: "run.Observation.Warnings",
			src:       "w",
			exempt:    nil,
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Pod)",
			fields:    exportedFields(snapshot.Pod{}),
			rangeExpr: "run.Observation.Pods",
			src:       "p",
			exempt: map[string]string{
				"ClusterID": clusterIDExempt,
				"IPScope": "归属是平台的判定（design doc §3.4），agent 连声明它的语法都不该有。" +
					"TestSinkNeverClaimsAScope 独立钉住了这一点。",
				"IPScopeReason": "同 IPScope：只在归属判不出来时才有意义，而归属本身就不该由 agent 声明。",
			},
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (ForeignScope)",
			fields:    exportedFields(snapshot.ForeignScope{}),
			rangeExpr: "run.Observation.ForeignScopes",
			src:       "fs",
			exempt:    nil,
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (Failure)",
			fields:    exportedFields(snapshot.Failure{}),
			rangeExpr: "run.Failures",
			src:       "f",
			exempt:    nil,
		},
		{
			label:     sinkFile + ":" + sinkFunc + " (NamedPort)",
			fields:    exportedFields(snapshot.NamedPort{}),
			rangeExpr: "p.NamedPorts",
			src:       "np",
			exempt:    nil,
		},
	}
}

func TestAgentWireCarriesEverySnapshotField(t *testing.T) {
	for _, site := range wireFanoutSites() {
		t.Run(site.label, func(t *testing.T) {
			read := site.fieldsRead(t)
			for _, f := range site.fields {
				if read[f] {
					if reason, ok := site.exempt[f]; ok {
						t.Errorf("%s 已经带上了 %s，但豁免表里还写着「%s」——"+
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
				t.Errorf("%s 没有把 %s 带过 agent 报文。\n"+
					"要么把它加进对应的 payload 类型并在这里读出来，"+
					"要么在 wireFanoutSites 的 exempt 里写下为什么不带。\n"+
					"漏掉它不会编译失败，也不会有任何症状——那一列在平台侧恒为空，"+
					"而空的含义是「没人报过」。", site.label, f)
			}
			for f := range site.exempt {
				if !containsString(site.fields, f) {
					t.Errorf("%s 的豁免表里的 %s 不再是这个类型的字段了，删掉这一条", site.label, f)
				}
			}
		})
	}
}

// fieldsRead 按这个抄写点的定位方式读出被抄到的字段名。
func (s wireFanoutSite) fieldsRead(t *testing.T) map[string]bool {
	t.Helper()
	if s.rootExpr != "" {
		return fieldsReadUnder(t, sinkFile, sinkFunc, s.rootExpr)
	}
	return fieldsReadInLoop(t, sinkFile, sinkFunc, s.rangeExpr, s.src)
}

// fieldsReadUnder 收集函数 fn 的整个函数体里，形如 <rootExpr>.<字段名> 的
// 选择表达式。
//
// 与 fieldsReadInLoop 相反，这里**要**扫整个函数体：聚合类型没有循环，它的
// 字段散落在整个函数里（run.Status 在开头的报文字面量里，run.Observation.Pods
// 在末尾的循环上）。歧义的风险也不存在 —— 根表达式是完整的点号路径，
// "run.Observation" 只可能指那一个东西，不像循环变量 n 那样会撞名。
func fieldsReadUnder(t *testing.T, file, fn, rootExpr string) map[string]bool {
	t.Helper()
	body := funcBody(t, file, fn)

	read := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if exprSource(sel.X) == rootExpr {
			read[sel.Sel.Name] = true
		}
		return true
	})
	if len(read) == 0 {
		t.Fatalf("%s 的 %s 里一次都没读过 %s.*——这条用例已经什么都不在守了",
			file, fn, rootExpr)
	}
	return read
}

// funcBody 解析 file，取出函数 fn 的函数体。
func funcBody(t *testing.T, file, fn string) *ast.BlockStmt {
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
	return body
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
// **必须精确到某一个循环，不能扫整个函数体**：这个包里好几个函数在不同
// 循环里重用同一个循环变量名（sink.go 的 Save 里 Namespaces 与 Nodes
// 都叫 n），扫整个函数体会把"在别的循环里读过同名字段"误判成"这个循环
// 读过"，而两个循环对应的是完全不同的快照类型。
func fieldsReadInLoop(t *testing.T, file, fn, rangeExpr, src string) map[string]bool {
	t.Helper()
	body := funcBody(t, file, fn)

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
// 如 run.Observation.Pods。够用：这个文件里出现的 range 表达式都是这个形状。
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
