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
// 判"带没带"看的是源码：在 Save() 里找到对应资源类型的那个 for-range
// 循环（用循环变量名与 range 表达式的源码文本定位，不是整个函数体——
// 这个函数里好几个循环变量重名，比如 Namespaces 与 Nodes 都叫 n，
// 混在一起扫会把"读过 Namespace.Name"误判成"读过 Node.Name"），
// 看循环体里有没有出现 <循环变量>.<字段名> 这个选择表达式。

// wireFanoutSite 是快照类型到 agent 报文的一个抄写点。
type wireFanoutSite struct {
	// label 是失败信息里的可读标识。
	label string
	// fields 是被抄写的快照类型的全部导出字段名，反射得到。
	fields []string
	// rangeExpr 是定位循环的 range 表达式源码文本，如 "run.Observation.Pods"。
	rangeExpr string
	// src 是循环变量名。
	src string
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

// preExistingGapExempt 标注一批与本轮改动无关的既有缺口：internal/collect
// 的共享采集逻辑本来就会填上这些字段（pull 模式的采集器与 push 模式的
// agent 走的是同一份 collectPods），但 agent 报文从来没带过它们。
//
// 本轮只关 LoadBalancer 暴露判定与命名端口两类——把这些字段也补上要求
// 先确认平台侧怎么消费（尤其 InMesh/MeshSource/MeshDetail 影响身份可信度
// 判定，ExtraIPs 影响双栈身份解析），那是另一轮的题。写在这里是为了它是
// 一个被记下来的缺口，而不是一次悄悄放过的遗漏。
const preExistingGapExempt = "既有缺口，与本轮改动（LoadBalancer 暴露判定、命名端口）无关：" +
	"internal/collect 的共享采集逻辑本来就会填上这个字段，但 agent 报文从未带过它。" +
	"本轮不补，先记下来。"

func wireFanoutSites() []wireFanoutSite {
	return []wireFanoutSite{
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
				"IPScopeReason":     "同 IPScope：只在归属判不出来时才有意义，而归属本身就不该由 agent 声明。",
				"ExtraIPs":          preExistingGapExempt,
				"InMesh":            preExistingGapExempt,
				"MeshSource":        preExistingGapExempt,
				"MeshDetail":        preExistingGapExempt,
				"ScrapeAnnotations": preExistingGapExempt,
			},
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
			read := fieldsReadInLoop(t, sinkFile, sinkFunc, site.rangeExpr, site.src)
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
