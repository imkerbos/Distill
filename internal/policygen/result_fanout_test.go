package policygen_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/policygen"
)

// Result 的每一个字段都会被四处代码抄一遍，抄漏一个字段不报错、不失败、
// 也不影响编译 —— 输出里那一栏变成 null，而它自己的文档说 null 的意思是
// 「没人算过这一栏」。
//
// **这个 bug 在本分支上发生了五次。** 前四次各自被局部修好（Apply 一次、
// namespace 折叠一次、fixture 预览一次、字段文档一次），而每一次修的都是
// 刚刚失败的那一处 —— 于是第五处（生产预览漏 ExposureWidenings）一路活到
// 最后一次评审：全生产集群上 spec §4 根本没上线。修一处修不掉这个类别，
// 这份用例才修得掉。
//
// 判据是**反射出来的**，不是手抄的清单：给 Result 加一个字段，这里立刻多
// 一项要求，加字段的人被迫走到下面那张豁免表前，写清楚为什么这一处不带。
// 手抄一份字段名清单会连同这个性质一起失效。
//
// 判「带没带」看的是源码：四个抄写点里有两个抄进的是另一个类型
// （store.PolicyPreview，字段名不一一对应，Policies 抄成 Candidates），
// 用值比对没法统一表达。看函数体里有没有读过 <源变量>.<字段名>，四处
// 是同一条判据。

// resultFanoutSite 是一个 Result 的抄写点。
type resultFanoutSite struct {
	// file 是该抄写点所在文件，相对本包目录。
	file string
	// fn 是承载抄写的函数名。
	fn string
	// src 是函数里持有那份 Result 的变量名。判据是"函数体里读过
	// src.<字段>"——变量改名会让这条用例红，那正是要的：抄写点被重写时
	// 必须有人重新确认这张表。
	src string
	// exempt 是**故意不带**的字段，值是理由。空表示这一处必须全带。
	exempt map[string]string
}

func resultFanoutSites() []resultFanoutSite {
	return []resultFanoutSite{{
		file: "granularity.go",
		fn:   "AtNamespaceGranularity",
		src:  "r",
		exempt: map[string]string{
			"UnattachedImports": "折叠前就没带过来，是一个既有缺口，不是本轮的决定。" +
				"补它要先决定 namespace 粒度下这一栏该怎么读（导入挂的是 workload），" +
				"那是另一轮的题。写在这里是为了它是一个被记下来的缺口，而不是一次遗漏。",
			"ExcludedNamespaces": "同上：既有缺口。这一栏讲的是「整片 namespace 没进候选集」，" +
				"而折叠的输入本来就只含进了候选集的那些 namespace，语义要重新定义。",
		},
	}, {
		file: "override.go",
		fn:   "Apply",
		src:  "base",
	}, {
		file: "../collectstore/policy.go",
		fn:   "PolicyPreviewAtGranularity",
		src:  "gen",
	}, {
		file: "../store/policy.go",
		fn:   "PolicyPreviewAtGranularity",
		src:  "gen",
	}}
}

func TestEveryResultFieldReachesEveryConsumer(t *testing.T) {
	fields := resultFieldNames()
	if len(fields) == 0 {
		t.Fatal("policygen.Result 没有字段？反射失败，这条用例什么都没在守")
	}
	for _, site := range resultFanoutSites() {
		t.Run(site.file+":"+site.fn, func(t *testing.T) {
			read := fieldsReadFrom(t, site)
			for _, f := range fields {
				if read[f] {
					if reason, ok := site.exempt[f]; ok {
						t.Errorf("%s 已经带上了 %s，但豁免表里还写着「%s」——"+
							"过期的豁免会掩护下一次遗漏", site.fn, f, reason)
					}
					continue
				}
				if reason, ok := site.exempt[f]; ok {
					if strings.TrimSpace(reason) == "" {
						t.Errorf("%s 豁免了 %s 却没写理由", site.fn, f)
					}
					continue
				}
				t.Errorf("%s（%s）没有带上 Result.%s。\n"+
					"要么把它抄过去，要么在 resultFanoutSites 的 exempt 里写下为什么不带。\n"+
					"漏掉它不会编译失败，也不会有任何症状——输出里那一栏是 null，"+
					"而 null 的含义是「没人算过」。", site.fn, site.file, f)
			}
			for f := range site.exempt {
				if !slicesContains(fields, f) {
					t.Errorf("豁免表里的 %s 不是 Result 的字段了，删掉这一条", f)
				}
			}
		})
	}
}

// resultFieldNames 反射出 Result 的全部导出字段名。
func resultFieldNames() []string {
	rt := reflect.TypeOf(policygen.Result{})
	out := make([]string, 0, rt.NumField())
	for i := range rt.NumField() {
		if f := rt.Field(i); f.IsExported() {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// fieldsReadFrom 解析抄写点所在的函数，收集它读过的 src.<字段名>。
func fieldsReadFrom(t *testing.T, site resultFanoutSite) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), site.file, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", site.file, err)
	}
	var body *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Name.Name == site.fn && fn.Body != nil {
			body = fn.Body
			return false
		}
		return true
	})
	if body == nil {
		t.Fatalf("%s 里找不到函数 %s —— 抄写点被移动或改名了，"+
			"resultFanoutSites 要跟着改", site.file, site.fn)
	}
	read := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == site.src {
			read[sel.Sel.Name] = true
		}
		return true
	})
	if len(read) == 0 {
		t.Fatalf("%s 的函数体里一次都没读过 %s.* —— 持有 Result 的变量多半改名了，"+
			"这条用例已经什么都不在守了", site.fn, site.src)
	}
	return read
}

func slicesContains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
