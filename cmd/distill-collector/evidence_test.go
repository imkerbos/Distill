package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// **拉取式采集必须在每一轮里记一次证据账。**
//
// 记账逻辑本身在 internal/evidence 里，那里有测试；这一条守的是**调用方仍然
// 在调它**。摘掉 collectAndIngest 里那一行，internal/evidence 的测试照样全绿，
// 而拉取式部署的每条规则会永远停在 windows=1 —— 与推送式那一侧同一个形态
// （见 cmd/distill-api 的 TestAssemblyStartsEvidenceAccounting）。
//
// 这条路径此前一直没有任何测试；这里补上，是因为"两种接入形态都要记账"
// 正是本轮要立住的性质，而只守住一侧等于没守。
func TestCollectAndIngestRecordsRuleEvidence(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "ingest.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ingest.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "collectAndIngest" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("ingest.go declares no collectAndIngest(); the call site moved and this test now proves nothing")
	}

	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recordEvidenceOnce" {
			found = true
		}
		return !found
	})
	if !found {
		t.Fatal("collectAndIngest 没有记证据账；拉取式部署的每条规则会永远停在 windows=1")
	}
}

// 记账必须走 internal/evidence 那一份实现，不得在这里另写一遍。
//
// 两份实现会让同一条规则在拉取式与推送式下累积出不同的证据，而操作者无从
// 知道该信哪一个 —— 而两者都答得出、都不报错。
func TestCollectorEvidenceDelegatesToTheSharedRecorder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "evidence.go", nil, 0)
	if err != nil {
		t.Fatalf("parse evidence.go: %v", err)
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "bookkeeping" && sel.Sel.Name == "NewRecorder" {
			found = true
		}
		return !found
	})
	if !found {
		t.Fatal("采集器自己实现了一份记账；两份实现会各自累积出不同的证据")
	}
}
