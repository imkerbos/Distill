package mysqlregistry

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/config"
)

// 本文件是白盒测试：被测的是迁移源的构造，它发生在拿到数据库连接之前，
// 因此这些用例不需要 MySQL，也不该因为没有 DSN 而 skip —— 这条路径的
// 失效方式是 API 容器起不来，那是一个和数据库无关的故障。

// writeFiles 在临时目录里造出给定的文件，内容即文件名，便于断言读到的是哪一份。
func writeFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("-- "+n), 0o600); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return dir
}

// 编辑器与合并留下的备份文件不得让迁移器构造失败。
//
// 这不是洁癖：API 容器**启动时**就跑迁移，所以这条路径失败等于服务起不来。
// 而 000009_x.up.sql.orig 这种名字恰好能被 source.DefaultParse 解析
// （版本 9、方向 up、扩展名 "sql.orig"），于是它和真文件撞成
// ErrDuplicateMigration —— 一个谁也没打算提交的文件停掉了整个服务。
//
// 断言不止于「构造成功」：还要读出 up 的内容，证明赢的是真文件而不是备份。
// 少了这一条，一个「后者覆盖前者」的实现同样能通过。
func TestMigrationSourceIgnoresEditorLeftovers(t *testing.T) {
	dir := writeFiles(t,
		"000001_ok.up.sql",
		"000001_ok.down.sql",
		"000001_ok.up.sql.orig", // 合并冲突留下的
		"000001_ok.down.sql~",   // 编辑器备份
		"000001_ok.up.sql.rej",  // 打补丁失败留下的
		".DS_Store",             // macOS
		"README.md",             // 说明文档
	)

	src, err := migrationSource(dir)
	if err != nil {
		t.Fatalf("migrationSource() error = %v, want the leftovers to be ignored", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	first, err := src.First()
	if err != nil {
		t.Fatalf("First() error = %v", err)
	}
	if first != 1 {
		t.Errorf("First() = %d, want 1", first)
	}
	if next, err := src.Next(first); err == nil {
		t.Errorf("Next(%d) = %d, want no further version", first, next)
	}

	body, identifier, err := src.ReadUp(first)
	if err != nil {
		t.Fatalf("ReadUp(%d) error = %v", first, err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read up body: %v", err)
	}
	if want := "-- 000001_ok.up.sql"; string(got) != want {
		t.Errorf("ReadUp body = %q, want %q: a leftover won over the real migration", got, want)
	}
	if identifier != "ok" {
		t.Errorf("ReadUp identifier = %q, want %q", identifier, "ok")
	}
}

// 名字不成形的 .sql 文件必须报错，不得被静默跳过。
//
// 与上一条是相反方向，两条缺一不可。golang-migrate 今天对解析不出来的
// 名字一律 continue —— 于是一个把版本号写漏的**真迁移**会悄悄不执行，
// 应用随后跑在一个它以为已经升过级的 schema 上。那比起不来危险得多。
//
// 报错必须点名是哪个文件，否则操作者只知道"迁移目录里有问题"。
func TestMigrationSourceRejectsMalformedSQLFile(t *testing.T) {
	dir := writeFiles(t,
		"000001_ok.up.sql",
		"000001_ok.down.sql",
		"observed_assets.up.sql", // 漏了版本号的真迁移
	)

	_, err := migrationSource(dir)
	if err == nil {
		t.Fatal("migrationSource() = nil error, want a malformed .sql file to be reported")
	}
	if !strings.Contains(err.Error(), "observed_assets.up.sql") {
		t.Errorf("error = %v, want it to name the offending file", err)
	}
}

// migrator 必须先构造迁移源，再去连数据库。
//
// **这一条能证明的仅限于此。** 它不能证明 migrate 用的是过滤后的源 ——
// 那件事要等真的读起迁移来才看得见，见 TestMigrateToleratesEditorLeftovers。
// 这个边界写在这里，是因为第一版处方正是在这里出的错：当时用一个连不上的
// DSN 去断言"错误里没有 duplicate"，而 db.Ping 在源构造之前就失败返回了，
// 两种实现留下的都是同一句 ping mysql —— 一条永远不会红的测试
// （HANDOFF：开处方前先确认它打开的窗口正是 bug 住的地方）。
//
// 改用一个名字不成形的 .sql 文件：它让 migrationSource 自己报错，
// 于是"源在连库之前构造、且它的错误会往上传"成为一个可观测的事实。
func TestMigratorBuildsTheSourceBeforeDialing(t *testing.T) {
	dir := writeFiles(t,
		"000001_ok.up.sql",
		"000001_ok.down.sql",
		"observed_assets.up.sql",
	)

	// DSN 刻意连不上：断言拿到的是目录的错，而不是连接的错。
	_, err := migrator(config.DatabaseConfig{DSN: "root:nobody@tcp(127.0.0.1:1)/nothing"}, dir)
	if err == nil {
		t.Fatal("migrator() = nil error, want the malformed migration file to be reported")
	}
	if !strings.Contains(err.Error(), "observed_assets.up.sql") {
		t.Errorf("migrator() error = %v, want the directory problem to surface before the dial", err)
	}
}

// 迁移器读起真实迁移时，必须能容忍目录里的备份文件。
//
// 这条才是过滤器的调用点测试：只有真的把迁移跑起来，才看得见 migrate
// 拿到的是过滤后的源还是 file://。把 migrator 里的 NewWithInstance 换回
// NewWithDatabaseInstance("file://"+dir, ...)，这条会红在
// ErrDuplicateMigration 上。
//
// 需要 MySQL，因此只在 make test-integration 里真正执行 —— 上面那条
// 不需要 MySQL 的用例挡不住这个方向，两条各管一半。
func TestMigrateToleratesEditorLeftovers(t *testing.T) {
	cfg := config.DatabaseConfig{DSN: internalTestDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}

	// 把真实迁移复制到临时目录，再放一个合并冲突留下的备份进去。
	// 不直接往 migrations/ 里写：那是仓库目录，一次失败的清理就会把
	// 这个文件留给下一个人。
	dir := t.TempDir()
	entries, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var latestUp string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join("../../migrations", e.Name())) // #nosec G304 -- 仓库内固定目录
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// #nosec G304,G703 -- dir 是 t.TempDir()，名字来自同一个仓库内固定目录的条目。
		if err := os.WriteFile(filepath.Join(dir, e.Name()), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
		if strings.HasSuffix(e.Name(), ".up.sql") && e.Name() > latestUp {
			latestUp = e.Name()
		}
	}
	if latestUp == "" {
		t.Fatal("no .up.sql found; this test would pass vacuously")
	}
	if err := os.WriteFile(filepath.Join(dir, latestUp+".orig"), []byte("-- leftover"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	if err := Migrate(cfg, dir); err != nil {
		t.Fatalf("Migrate() error = %v, want a leftover %s.orig to be ignored; "+
			"the API container runs migrations at startup, so this failure is an outage",
			err, latestUp)
	}
}

// 过滤器不得把真迁移一起挡掉。
//
// 这是上面两条的另一半证伪：一个「什么都不放行」的实现能让前两条都通过。
// 直接读仓库里真实的 migrations/ 目录，逐个版本都要能读出 up 与 down。
func TestMigrationSourceLoadsEveryRealMigration(t *testing.T) {
	const dir = "../../migrations"

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	want := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			want++
		}
	}
	if want == 0 {
		t.Fatalf("%s has no .up.sql files; this test would pass vacuously", dir)
	}

	src, err := migrationSource(dir)
	if err != nil {
		t.Fatalf("migrationSource(%s) error = %v", dir, err)
	}
	t.Cleanup(func() { _ = src.Close() })

	got := 0
	version, err := src.First()
	for err == nil {
		got++
		if _, _, upErr := src.ReadUp(version); upErr != nil {
			t.Errorf("ReadUp(%d) error = %v", version, upErr)
		}
		if _, _, downErr := src.ReadDown(version); downErr != nil {
			t.Errorf("ReadDown(%d) error = %v: every migration must be reversible", version, downErr)
		}
		version, err = src.Next(version)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Next() error = %v, want fs.ErrNotExist at the end", err)
	}
	if got != want {
		t.Errorf("loaded %d migrations, want %d: the filter dropped a real one", got, want)
	}
}
