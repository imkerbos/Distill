package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/store"
)

// 装配层交给 httpapi.Deps.Reader 的必须是分派器，而不是某一个 Reader。
//
// 这条测试存在的理由：本轮之前，`run()` 里那一行是整个分支唯一的接线，而
// **它没有任何测试**。把它改回 `newFixtureReader(reg)` 是一次编辑，输出是
// 全部测试仍然全绿，而本轮的全部性质当场失效（branch review I1）。这与
// falsification #3 逮到的是同一个形态 —— 守卫被验过，没有东西证明调用方
// 仍然在调它 —— 只是这次落在装配线本身。
//
// **读的是 main.go 的语法树，不是一张手写清单。** 前两次这个形态的漏网都出在
// 手写表格上（一张漏了一个方法，一张断言了错误的行为），所以这里不写"应当
// 出现的构造器名单"，而是从源码里**取出** Deps.Reader 实际被赋的那个表达式，
// 再问它是不是 newReader 的返回值。改名、换构造器、直接内联另一个 Reader ——
// 三种改法都会让它红，而它自己不需要跟着任何东西更新。
//
// 与它配套的是 newReader 的返回类型：那是具体类型 *dispatchReader，因此
// "newReader 仍然被调用"加上"newReader 只可能交出分派器"合起来才封住这一行。
// 单有语法树那条，newReader 内部可以被换掉；单有返回类型那条，run() 可以
// 绕开 newReader。两条各自独立。
func TestAssemblyHandsTheDispatchingReaderToTheRouter(t *testing.T) {
	const wantCtor = "newReader"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	run := funcDecl(file, "run")
	if run == nil {
		t.Fatal("main.go declares no run(); the assembly moved and this test now proves nothing")
	}

	value, pos := depsFieldValue(t, fset, run, "Reader")
	// 值是一个标识符时回溯到它在 run() 里的赋值：`reader := ...` 之后
	// `Reader: reader` 是今天的写法，而直接写 `Reader: newReader(db, reg)`
	// 也应当通过 —— 判据是"这个值从哪来"，不是源码长什么样。
	if id, ok := value.(*ast.Ident); ok {
		v, p := assignedValue(run, id.Name)
		if v == nil {
			t.Fatalf("%s: Deps.Reader is %s, which run() never assigns; cannot tell what the "+
				"router is being handed", fset.Position(pos), id.Name)
		}
		value, pos = v, p
	}

	call, ok := value.(*ast.CallExpr)
	if !ok {
		t.Fatalf("%s: Deps.Reader is not built by a call; want %s(...)",
			fset.Position(pos), wantCtor)
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != wantCtor {
		t.Fatalf("%s: Deps.Reader is built by %s, want %s — the router must be handed the "+
			"reader that dispatches on each cluster's registered data source. Handing it a "+
			"single reader silently undoes this whole change (branch review I1)",
			fset.Position(pos), exprName(call.Fun), wantCtor)
	}
}

// funcDecl 取出文件里某个顶层函数。
func funcDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// depsFieldValue 在函数体里找出 httpapi.Deps 复合字面量的某个字段的值。
//
// 找不到就 Fatal，不是返回零值：一条"什么都没找到所以没什么可断言"的测试
// 恰好是这个项目已经出过二十二次的那种测不出来的测试。装配被重构成别的形状
// 时，正确的表现是这里红并说明原因。
func depsFieldValue(
	t *testing.T, fset *token.FileSet, fn *ast.FuncDecl, field string,
) (ast.Expr, token.Pos) {
	t.Helper()
	var value ast.Expr
	var pos token.Pos
	var found int
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isSelector(lit.Type, "httpapi", "Deps") {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
				found++
				value, pos = kv.Value, kv.Value.Pos()
			}
		}
		return true
	})
	switch found {
	case 1:
		return value, pos
	case 0:
		t.Fatalf("%s: run() builds no httpapi.Deps with a %s field; the assembly moved and "+
			"this test now proves nothing", fset.Position(fn.Pos()), field)
	default:
		t.Fatalf("%s: run() builds %d httpapi.Deps literals carrying %s; this test can only "+
			"speak for one of them", fset.Position(fn.Pos()), found, field)
	}
	return nil, token.NoPos
}

// assignedValue 找出函数体里 `name := expr` 或 `name = expr` 的右手边。
//
// 只认单值赋值：`a, b := f()` 这种形状下"这个变量从哪来"没有一个能断言的
// 表达式，回 nil 让调用方报错，而不是猜。
func assignedValue(fn *ast.FuncDecl, name string) (ast.Expr, token.Pos) {
	var value ast.Expr
	var pos token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == name {
			value, pos = as.Rhs[0], as.Rhs[0].Pos()
		}
		return true
	})
	return value, pos
}

// isSelector 判断表达式是不是 pkg.Name 这种形状。
func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// exprName 给失败信息取一个可读的名字。
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	default:
		return "an expression of another shape"
	}
}

// newReader 交出来的那个 Reader 必须真的按登记来源分派，而不只是类型对。
//
// 与语法树那条互补：那条管"run() 仍然在用 newReader"，这条管"newReader 交出来
// 的东西行为正确"。少了这条，`newReader` 可以退化成 `newDispatchReader(reg, nil)` ——
// 类型仍然是 *dispatchReader，语法树那条照过，而每个 COLLECTED 集群从此
// 拿不到采集侧 Reader。
//
// db 传 nil：collectstore.New 只把它存起来，构造时不发起任何连接，而这条用例
// 在 readerOf 就得到答案，走不到取数。
func TestNewReaderResolvesACollectedClusterToTheCollectedReader(t *testing.T) {
	src := mixedFleet()
	r := newReader(nil, src)

	got, err := r.readerOf(t.Context(), fixtureBackedID)
	if err != nil {
		t.Fatalf("newReader(...).readerOf(%s declared COLLECTED) error = %v; the assembly "+
			"must hand the dispatcher a usable collected reader", fixtureBackedID, err)
	}
	if _, ok := got.(*collectstore.Reader); !ok {
		t.Errorf("newReader(...).readerOf(%s declared COLLECTED) = %T, want *collectstore.Reader",
			fixtureBackedID, got)
	}

	// 对照组：FIXTURE 集群仍然走合成数据集。少了它，一个无条件交出采集侧
	// Reader 的 newReader 也能让上面通过 —— 而那是把演示环境接到 MySQL 上。
	got, err = r.readerOf(t.Context(), fixtureBackedPeer)
	if err != nil {
		t.Fatalf("newReader(...).readerOf(%s declared FIXTURE) error = %v", fixtureBackedPeer, err)
	}
	if _, ok := got.(*store.FixtureReader); !ok {
		t.Errorf("newReader(...).readerOf(%s declared FIXTURE) = %T, want *store.FixtureReader",
			fixtureBackedPeer, got)
	}
}
