package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	applog "github.com/imkerbos/Distill/internal/log"
)

// countingAccountant 数记账被发起了几次。
type countingAccountant struct{ calls atomic.Int64 }

func (c *countingAccountant) Once(context.Context) error {
	c.calls.Add(1)
	return nil
}

// 周期为正时，记账在启动时立刻跑一次，之后按周期反复跑。
//
// **启动时那一次是刻意的**：等满一个周期才第一次记账，意味着每次重启都会
// 丢掉一段观测；而重复记同一个窗口由 Accountant 自己的守卫挡住，重启不会
// 把证据算多。
func TestEvidenceAccountingRunsAtStartupAndThenOnTheInterval(t *testing.T) {
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	a := &countingAccountant{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		runBookkeeping(ctx, a, time.Millisecond, logger)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for a.calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("记账只发起了 %d 次，周期没有在走", a.calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("上下文结束后记账循环没有退出")
	}
}

// 周期为 0 表示关掉：一次都不发起，且立刻返回。
func TestEvidenceAccountingIsOffWhenTheIntervalIsZero(t *testing.T) {
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	a := &countingAccountant{}
	done := make(chan struct{})
	go func() {
		runBookkeeping(context.Background(), a, 0, logger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("周期为 0 却没有立刻返回；关掉记账变成了一个空转的循环")
	}
	if got := a.calls.Load(); got != 0 {
		t.Errorf("周期为 0 却发起了 %d 次记账", got)
	}
}

// **装配层必须真的把这个循环起起来。**
//
// 上面两条直接调 runBookkeeping，绕过了"run() 到底有没有起它"这一环 ——
// 与 TestAssemblyHandsTheDispatchingReaderToTheRouter 是同一个形态：守卫被验过，
// 没有东西证明调用方仍然在调它。摘掉 run() 里那一行，上面两条照样全绿，而
// 推送式接入的证据会重新冻在 windows=1。
func TestAssemblyStartsEvidenceAccounting(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	run := funcDecl(file, "run")
	if run == nil {
		t.Fatal("main.go declares no run(); the assembly moved and this test now proves nothing")
	}

	found := false
	ast.Inspect(run, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "runBookkeeping" {
			found = true
		}
		return !found
	})
	if !found {
		t.Fatal("run() 没有起证据记账循环；推送式接入的每条规则会永远停在 windows=1")
	}
}

// **两件记账都必须被注册。**
//
// 上一条只证明循环起来了，不证明它有事可做：删掉 agreement 那个 Task，
// 循环照跑、上面那条照绿，而一致率趋势会重新变成一条永远空着的曲线 ——
// 而空趋势读起来是"这个集群还没对过账"，不是"这条接入方式对不了账"。
func TestAssemblyRegistersBothBookkeepingTasks(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	run := funcDecl(file, "run")
	if run == nil {
		t.Fatal("main.go declares no run(); the assembly moved and this test now proves nothing")
	}

	// 找出 run() 里构造的每一个 bookkeeping.Task 的 Name。
	names := map[string]bool{}
	ast.Inspect(run, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Task" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "bookkeeping" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Name" {
				continue
			}
			if v, ok := kv.Value.(*ast.BasicLit); ok {
				names[strings.Trim(v.Value, `"`)] = true
			}
		}
		return true
	})

	for _, want := range []string{"evidence", "agreement"} {
		if !names[want] {
			t.Errorf("run() 没有注册 %q 记账；它那一侧的数据会永远停在原地，拿到的 = %v",
				want, names)
		}
	}
}
