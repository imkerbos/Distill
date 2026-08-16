package mysqlregistry

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// migrationFileShape 是一个迁移文件名必须满足的形状。
//
// 扩展名必须**恰好**是 sql，这是整条规则的重点。golang-migrate 的解析器
// 把扩展名当作任意后缀，于是 000009_x.up.sql.orig 会被解析成"版本 9、
// 方向 up"，与真文件撞成 ErrDuplicateMigration —— 而 API 容器启动时就跑
// 迁移，所以一个合并冲突留下的备份文件足以让服务起不来。
var migrationFileShape = regexp.MustCompile(`^[0-9]+_.+\.(up|down)\.sql$`)

// migrationSource 构造只认迁移文件的源。
//
// 两个方向都要管，缺一条都不安全：
//
//   - 不成形、也不像迁移的文件（备份、补丁残留、.DS_Store、说明文档）
//     一律忽略。让它们停掉服务，是拿一次可用性事故去换一次目录整洁。
//   - 名字不成形的 **.sql** 文件报错并点名。golang-migrate 对这类名字是
//     静默跳过的，于是一个版本号写漏的真迁移会悄悄不执行，应用随后跑在
//     一个它以为已经升过级的 schema 上 —— 那比起不来危险得多。
func migrationSource(dir string) (source.Driver, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	allowed := make(map[string]bool, len(entries))
	var offending []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case migrationFileShape.MatchString(name):
			allowed[name] = true
		case strings.HasSuffix(name, ".sql"):
			offending = append(offending, name)
		}
	}
	if len(offending) > 0 {
		slices.Sort(offending)
		return nil, fmt.Errorf(
			"migrations dir %s holds .sql files that are not named NNN_name.{up,down}.sql: %s; "+
				"a mis-named migration is silently skipped, so it is refused here instead",
			dir, strings.Join(offending, ", "))
	}

	return iofs.New(allowlistFS{fsys: os.DirFS(dir), allowed: allowed}, ".")
}

// allowlistFS 只暴露白名单里的文件。
//
// 过滤放在文件系统这一层而不是构造完再剔除：iofs 在 Init 里就会因为
// 重名而报错，等它构造完再修正已经晚了。
type allowlistFS struct {
	fsys    fs.FS
	allowed map[string]bool
}

// Open 只打开白名单里的文件；目录本身放行，否则 ReadDir 无从开始。
func (a allowlistFS) Open(name string) (fs.File, error) {
	if name != "." && !a.allowed[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return a.fsys.Open(name)
}

// ReadDir 只列出白名单里的条目。
func (a allowlistFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(a.fsys, name)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		if a.allowed[e.Name()] {
			out = append(out, e)
		}
	}
	return out, nil
}
