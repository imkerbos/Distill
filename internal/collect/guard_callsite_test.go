package collect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// commandsDir 是仓库里所有二进制的装配目录，相对本包。
const commandsDir = "../../cmd"

// collectImportPath 是本包的导入路径。
const collectImportPath = "github.com/imkerbos/Distill/internal/collect"

// TestEveryCommandThatCollectsProvesItIsReadOnly 守的是"那一天不会忘"。
//
// AssertReadOnly 今天没有生产调用点。调用点属于 cmd/distill-collector 的
// 装配（spec §3.3：采集器**启动时**自证），而那个二进制还没写；塞进
// Collect 会让每轮采集多发 15 次 SSAR，且把一次性的启动闸门变成每轮的
// 常规开销 —— 那是一次设计决定，不该由一次修缺陷顺手做掉。
//
// 于是这里钉的是绑定关系而不是覆盖：cmd/ 下任何引用了本包的二进制，
// 都必须同时调用 AssertReadOnly。今天 cmd/ 里没有这样的二进制，
// 本用例是绿的 —— **绿不代表只读性已经被运行时验证**，它现在仍然只是
// 一条注释。这个缺口记在 docs/TODO.md 的「还没做」里，是采集器的合并
// 阻塞项。第一个装配出采集器的人不需要读过这份注释：漏掉自证，
// 这条用例当场变红。
func TestEveryCommandThatCollectsProvesItIsReadOnly(t *testing.T) {
	entries, err := os.ReadDir(commandsDir)
	if err != nil {
		t.Fatalf("read %s: %v", commandsDir, err)
	}

	collectors := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		importsCollect, callsAssert := scanCommand(t, filepath.Join(commandsDir, e.Name()))
		if !importsCollect {
			continue
		}
		collectors++
		if !callsAssert {
			t.Errorf("cmd/%s uses %s but never calls AssertReadOnly: "+
				"a collector must prove it holds no policy write access before it reads a cluster (spec §3.3)",
				e.Name(), collectImportPath)
		}
	}
	t.Logf("commands importing %s: %d", collectImportPath, collectors)
}

// scanCommand 读一个命令目录（含子包）的非测试源码，回答两件事：
// 它引用本包吗、它调用 AssertReadOnly 吗。
//
// 走 AST 而不是 grep 文本：注释里、字符串里出现一次 AssertReadOnly
// 不算调用点，而这条用例要是被一句注释哄住，它就成了本项目已出现
// 十七次的那种"不可能失败的测试"。
func scanCommand(t *testing.T, dir string) (importsCollect, callsAssert bool) {
	t.Helper()

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}

		// 本包可能被起了别名，按 import 声明取它在这个文件里的名字。
		local := ""
		for _, spec := range file.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil || p != collectImportPath {
				continue
			}
			importsCollect = true
			local = "collect"
			if spec.Name != nil {
				local = spec.Name.Name
			}
		}
		if local == "" {
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AssertReadOnly" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == local {
				callsAssert = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", dir, err)
	}
	return importsCollect, callsAssert
}
