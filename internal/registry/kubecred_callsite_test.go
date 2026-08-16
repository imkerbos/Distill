package registry_test

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

// 相对本包的仓库位置。
const (
	commandsDir = "../../cmd"
	httpAPIDir  = "../../internal/httpapi"
)

// resolverPkgPrefix 覆盖 internal/secrets 与它下面的后端实现
// （gcpsecrets）。前缀而不是精确匹配：后端各自成包，把引用变成凭据的
// 能力跟着任何一个包一起进来。
const resolverPkgPrefix = "github.com/imkerbos/Distill/internal/secrets"

// collectorCommand 是唯一被允许解析 KubeconfigRef 的二进制（spec §1、§3.5）。
const collectorCommand = "distill-collector"

// TestOnlyTheCollectorCanTurnAKubeconfigRefIntoCredentials 守的是
// CLAUDE.md 那条「平台主服务不得持有日常 Kubernetes 策略写权限」。
//
// 进程隔离本身并不构成这条保证：distill-api 已经为了 Git 校验与写回持有
// 一个 secrets.Resolver（cmd/distill-api/main.go 的 newSecretResolver），
// 而 Resolver.Resolve 收的是一个普通 string。把 cluster.KubeconfigRef 传
// 进那同一个 Resolver，今天编译、运行、返回 kubeconfig 都不会有任何阻力。
// 所以这条性质**不是类型系统给出的**，别把它当成已经结构性成立的事。
//
// 这里钉的是能力与引用的共存：cmd/ 下除采集器之外的任何二进制、以及
// internal/httpapi，只要源码里出现 KubeconfigRef，就不得同时 import
// internal/secrets。**存与显示不受影响** —— httpapi 不 import secrets，
// 后续那个把引用显示到控制台上的任务照常绿。变红的恰好是「引用与解析
// 能力被放进了同一个包」这一步，也就是主服务开始持有集群凭据的那一步。
//
// 拦不住的：某个第三方包同时 import secrets 又认识 KubeconfigRef，再由
// httpapi 调它。那种形状要靠 review 看出来，本用例看不见 —— 写在这里，
// 免得下一个人把这条用例的绿当成完整证明。
func TestOnlyTheCollectorCanTurnAKubeconfigRefIntoCredentials(t *testing.T) {
	entries, err := os.ReadDir(commandsDir)
	if err != nil {
		t.Fatalf("read %s: %v", commandsDir, err)
	}

	dirs := map[string]string{"internal/httpapi": httpAPIDir}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == collectorCommand {
			continue
		}
		dirs["cmd/"+e.Name()] = filepath.Join(commandsDir, e.Name())
	}

	for name, dir := range dirs {
		namesRef, importsSecrets := scanForKubeconfigResolution(t, dir)
		if namesRef && importsSecrets {
			t.Errorf("%s names KubeconfigRef and imports %s: "+
				"only %s may turn that reference into a kubeconfig — the platform's main "+
				"service must not hold day-to-day Kubernetes access (CLAUDE.md §3, spec §1)",
				name, resolverPkgPrefix, collectorCommand)
		}
		t.Logf("%s: namesKubeconfigRef=%v importsSecrets=%v", name, namesRef, importsSecrets)
	}
}

// scanForKubeconfigResolution 读一个目录（含子包）的非测试源码，回答两件事：
// 它提到 KubeconfigRef 吗、它 import 得到解析能力吗。
//
// 走 AST 而不是 grep 文本：注释里、字符串里出现一次 KubeconfigRef 不算
// 引用，而这条用例要是被一句注释哄住，它就成了本项目已出现十七次的那种
// 不可能失败的测试。只看 *ast.Ident 就够 —— 选择器 x.KubeconfigRef 的
// Sel 与复合字面量里的键 KubeconfigRef: v 都是 Ident。
func scanForKubeconfigResolution(t *testing.T, dir string) (namesRef, importsSecrets bool) {
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
		for _, spec := range file.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr == nil && strings.HasPrefix(p, resolverPkgPrefix) {
				importsSecrets = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "KubeconfigRef" {
				namesRef = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", dir, err)
	}
	return namesRef, importsSecrets
}
