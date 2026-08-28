package httpapi_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/imkerbos/Distill/internal/store"
)

// store.PolicyPreview 到"离开平台的那两段文字"——导出文件的注释头与写回的
// 提交信息——是又一处手抄的边界：判定的产物是一个结构体，而呈现出来的是
// 另一份人写的行。给 PolicyPreview 加一个字段，这两处不会有任何反应。
//
// 这个边界比 agent 报文那几处更要命，因为它的下游不是另一段代码，是人的
// 判断：导出的 YAML 会脱离平台独自存在，隔几天被人应用到生产集群上，而
// 那时他手上只有这段注释头。平台知道自己哪里没看全（窗口不完整、基线推
// 不出、主体挂不上、规则被放宽），却在唯一会被读到的那段文字里一个字都
// 不说——读者读到的是四个数字和一句让人安心的收尾。
//
// 判据反射得来，不是手抄的清单：加字段的人被迫走到豁免表前写清楚为什么
// 这一处不带。
type previewFanoutSite struct {
	// label 是失败信息里的可读标识。
	label string
	// fields 是被抄写类型的全部导出字段名，反射得到。
	fields []string
	// file/fns 定位抄写点。一个抄写点可以横跨同一文件里的几个函数
	// （注释头把缺口那几行拆给了 renderPolicyCaveats），字段读取取并集。
	file string
	fns  []string
	// extraFile/extraFns 是同一个抄写点在另一个文件里的续篇。
	extraFile string
	extraFns  []string
	// rootExpr 是该类型在函数体里的根表达式，如 "pv"。
	rootExpr string
	// exempt 是故意不带的字段，值是理由。
	exempt map[string]string
}

// previewHeaderExempt 是注释头故意不带的字段。每一条的判据是同一个问题：
// 拿着这份 YAML、隔几天、手上没有平台的那个人，少了它会不会做错决定。
var previewHeaderExempt = map[string]string{
	"Candidates": "文件正文就是它——注释头下面逐份渲染成 YAML 的正是这批候选策略。" +
		"在注释头里再数一遍是同一件事说两次，而两处一旦不一致，读者无从判断信哪个。",
	"Prediction": "候选集单独跑的那一套计数，回答的是「把旧策略也清理掉」这个另外的问题。" +
		"注释头统一取 OverriddenPredictionWithExisting——并入集群既有策略、且应用过人工决定的那一套，" +
		"因为那才是应用这份文件之后的实际状态。两套数字都印会让读者拿错那一套（design doc §3.4）。",
	"PredictionWithExisting": "默认推荐的那一套计数。这份文件渲染的是 Overridden 那一份候选集，" +
		"配上默认推荐的数字就是轮 3 那条缺陷：文字与内容各说各的。",
	"Overridden": "本函数经 OverriddenPredictionWithExisting() 读它——注释头的四类计数正是从这里来的。" +
		"反射扫的是 pv.<字段> 的形状，方法调用扫不出来，因此写在这里。",
	"Overrides": "人工决定本身已经烘进 Overridden 那份候选集，也就是文件正文。" +
		"读者要判断的是「应用这份文件会怎样」，不是「谁在什么时候按了哪个按钮」——后者是平台上的审计，不是这份文件的职责。",
	"Evidence": "逐条规则的观测计数（观察了几个窗口、多少次）。它回答的是「这条规则该不该确认」，" +
		"而到了导出这一步，该确认的已经确认过了；文件里的每一条都是有人拍过板的。" +
		"把它印进注释头只会让这段文字长到没人读——而这几行的全部价值在于会被读完。",
}

// commitMessageExempt 在注释头那份之上多豁免一条：提交信息拿的是 URL 上的
// 集群 ID，不是预览里的那个。
var commitMessageExempt = func() map[string]string {
	out := map[string]string{
		"Cluster": "提交信息用的是 URL 上、且刚被鉴权过的那个集群 ID（clusterID 形参），" +
			"不是预览结构体里的那个。两者本该相等，而在一份要落进仓库历史的文字上，" +
			"取被鉴权过的那一个是刻意的：预览是查出来的数据，clusterID 是这次请求被批准操作的对象。",
	}
	for k, v := range previewHeaderExempt {
		out[k] = v
	}
	return out
}()

func previewFanoutSites() []previewFanoutSite {
	return []previewFanoutSite{
		{
			label:  "export_handler.go:renderPolicyDocs",
			fields: exportedFields(store.PolicyPreview{}),
			file:   "export_handler.go",
			fns:    []string{"renderPolicyDocs"},
			// 缺口那几行拆在 policy_caveats.go 里，与写回的提交信息共用；
			// 它读的 pv.* 与这里读的合起来才是"注释头带上了什么"。
			extraFile: "policy_caveats.go",
			extraFns:  []string{"renderPolicyCaveats", "renderPolicyBasis"},
			rootExpr:  "pv",
			exempt:    previewHeaderExempt,
		},
		{
			label:  "writeback_handler.go:writebackCommitMessage",
			fields: exportedFields(store.PolicyPreview{}),
			file:   "writeback_handler.go",
			fns:    []string{"writebackCommitMessage"},
			// 与注释头共用同一个缺口渲染器——两处读者做的是同一个决定，
			// 说的话必须一样。
			extraFile: "policy_caveats.go",
			extraFns:  []string{"renderPolicyCaveats", "renderPolicyBasis"},
			rootExpr:  "pv",
			exempt:    commitMessageExempt,
		},
	}
}

func TestPolicyPreviewReachesTheReaderOfTheExportedFile(t *testing.T) {
	for _, site := range previewFanoutSites() {
		read := map[string]bool{}
		for _, fn := range site.fns {
			for f := range fieldsReadUnderRoot(t, site.file, fn, site.rootExpr) {
				read[f] = true
			}
		}
		for _, fn := range site.extraFns {
			for f := range fieldsReadUnderRoot(t, site.extraFile, fn, site.rootExpr) {
				read[f] = true
			}
		}
		for _, f := range site.fields {
			if read[f] {
				if why, ok := site.exempt[f]; ok {
					t.Errorf("%s: %s 既被豁免又确实带上了——豁免表过期了，删掉这一条。理由写的是：%s",
						site.label, f, why)
				}
				continue
			}
			if _, ok := site.exempt[f]; ok {
				continue
			}
			t.Errorf("%s: %s.%s 没有出现在这段文字里，豁免表里也没有它。\n"+
				"    这份文件会脱离平台独自存在、隔几天被应用到集群上，读者手上只有这段注释头。\n"+
				"    要么把它渲染出去，要么写进 exempt 并说明为什么读者不需要知道。",
				site.label, site.rootExpr, f)
		}
	}
}

// fieldsReadUnderRoot 扫整个函数体，收集所有 <rootExpr>.X 形状的字段读取。
//
// 与 fieldsReadInLoop 的区别是不锚在某个 for-range 上：PolicyPreview 是聚合
// 类型，字段直接写成 pv.X 散落在函数体各处。
func fieldsReadUnderRoot(t *testing.T, file, fn, rootExpr string) map[string]bool {
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
		t.Fatalf("%s 里找不到函数 %s —— 抄写点被移动或改名了，previewFanoutSites 要跟着改", file, fn)
	}

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
		t.Fatalf("%s 的 %s 一次都没读过 %s.* —— 这条用例已经什么都不在守了", file, fn, rootExpr)
	}
	return read
}
