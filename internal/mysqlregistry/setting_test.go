package mysqlregistry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
)

// newSettingStore 在一次完整的 down + up 之后返回 Store。
//
// 设置是单行表，测试之间没有「换一个 id」这条隔离路线：上一个测试保存过
// 的值就是下一个测试的初始值，而种子行的断言必须看到迁移刚种下的样子。
// 重跑一遍迁移是这里唯一能给出干净起点的做法 —— 顺带也让每个设置测试
// 都真的跑过一次 down 脚本。
func newSettingStore(t *testing.T) (*mysqlregistry.Store, *sql.DB) {
	t.Helper()
	cfg := config.DatabaseConfig{DSN: testDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}
	if err := mysqlregistry.Rollback(cfg, "../../migrations"); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	return newTestStore(t)
}

// seededSetting 是迁移种下的那一份，取值与 configs/demo.yaml 当前生效值一致。
func seededSetting() registry.PlatformSetting {
	return registry.PlatformSetting{
		SessionTTL:          8 * time.Hour,
		HTTPReadTimeout:     10 * time.Second,
		HTTPWriteTimeout:    20 * time.Second,
		HTTPShutdownTimeout: 15 * time.Second,
		SecretsBackend:      registry.SecretsBackendNone,
		GitVerifyTimeout:    10 * time.Second,
	}
}

// latestAudit 取某个动作最近一条审计行的 cluster_id、actor、target 与前后值。
//
// 按 id 倒序取而不是随便取一条：设置会被改多次，而「前值是上一次的值」
// 这个断言只有落在确定的那一行上才成立。
func latestAudit(t *testing.T, db *sql.DB, action string) (
	clusterID, actor, target string, before, after map[string]any,
) {
	t.Helper()
	var beforeVal, afterVal sql.NullString
	if err := db.QueryRow(
		`SELECT cluster_id, actor, target, before_val, after_val
		   FROM audit_log WHERE action = ? ORDER BY id DESC LIMIT 1`, action,
	).Scan(&clusterID, &actor, &target, &beforeVal, &afterVal); err != nil {
		t.Fatalf("query latest audit row for %s: %v", action, err)
	}
	decode := func(v sql.NullString) map[string]any {
		if !v.Valid {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(v.String), &m); err != nil {
			t.Fatalf("decode %s payload %q: %v", action, v.String, err)
		}
		return m
	}
	return clusterID, actor, target, decode(beforeVal), decode(afterVal)
}

// 升级后设置表不能是空的：第一次读就没有配置可用，而这些字段的零值
// 不是「用默认」，而是关掉超时保护、会话立即过期。
func TestMigrationSeedsTheSettingRowWithTheConfigFileValues(t *testing.T) {
	s, db := newSettingStore(t)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM platform_setting`).Scan(&n); err != nil {
		t.Fatalf("count platform_setting: %v", err)
	}
	if n != 1 {
		t.Fatalf("platform_setting rows after migrate = %d, want exactly 1", n)
	}

	got, err := s.Setting(context.Background())
	if err != nil {
		t.Fatalf("Setting() error = %v", err)
	}
	if want := seededSetting(); !reflect.DeepEqual(got, want) {
		t.Errorf("seeded setting =\n%+v\nwant configs/demo.yaml 的现行取值\n%+v\n"+
			"—— 种子与配置文件不一致，升级就是一次没人宣布的行为变更", got, want)
	}
	// 种下的值必须自己就是合法的，否则第一次打开设置页保存就会被拒。
	if err := registry.ValidatePlatformSetting(got); err != nil {
		t.Errorf("ValidatePlatformSetting(seeded) error = %v, want nil", err)
	}
}

// 单行由数据库保证，不靠代码自觉：多出第二行时「当前设置是哪一行」
// 没有答案，而并发插入正是写出第二行的那条路径。
func TestSecondSettingRowIsRejectedByTheDatabase(t *testing.T) {
	_, db := newSettingStore(t)

	// 绕过 Store 直接插：这条约束要挡的就是绕过写路径的插入，
	// 经由 UpdateSetting 去插永远插不出第二行，也就证明不了任何事。
	_, err := db.Exec(
		`INSERT INTO platform_setting
		   (id, session_ttl_seconds, http_read_timeout_ms, http_write_timeout_ms,
		    http_shutdown_timeout_ms, secrets_backend, secrets_project, secrets_prefix,
		    secrets_dir, gitverify_timeout_ms, gitverify_host_keys, updated_at)
		 VALUES (2, 3600, 5000, 5000, 5000, 'NONE', '', '', '', 5000, '', UTC_TIMESTAMP(6))`)
	if err == nil {
		t.Error("INSERT of a second platform_setting row succeeded, " +
			"want the CHECK constraint to reject it —— 两行设置时「当前设置是哪一行」没有答案")
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM platform_setting`).Scan(&n); err != nil {
		t.Fatalf("count platform_setting: %v", err)
	}
	if n != 1 {
		t.Errorf("platform_setting rows = %d, want 1", n)
	}
}

// 每次设置变更都必须留下前后值完整的审计行。host key 是平台连接策略仓库
// 的信任锚，而它能从后台改（design doc §1.3）—— 这条审计是事后唯一能回答
// 「信任锚是谁在什么时候换的」的东西。
func TestUpdateSettingWritesAuditWithBeforeAndAfter(t *testing.T) {
	s, db := newSettingStore(t)
	ctx := context.Background()
	// actor 来自会话，不来自请求体；审计行必须记下的是这个人。
	actor := registry.Actor{Username: "settings-admin"}

	// 先落一份非平凡的前值：从种子行的空 host key 直接改，「前值」是空串，
	// 那与「前值这一列根本没写」看起来一模一样。
	first := seededSetting()
	first.GitVerifyHostKeys = "gitlab.example.com ssh-ed25519 AAAAOLDKEY"
	first.SessionTTL = 4 * time.Hour
	if err := s.UpdateSetting(ctx, actor, first); err != nil {
		t.Fatalf("first UpdateSetting() error = %v", err)
	}

	second := first
	second.GitVerifyHostKeys = "gitlab.example.com ssh-ed25519 AAAANEWKEY"
	second.SecretsBackend = registry.SecretsBackendDir
	second.SecretsDir = "/run/secrets/distill"
	if err := s.UpdateSetting(ctx, actor, second); err != nil {
		t.Fatalf("second UpdateSetting() error = %v", err)
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = ?`, "UPDATE_PLATFORM_SETTING",
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	// 恰好两条：一次变更被记两次与两次真实变更，在复盘时无法区分。
	if n != 2 {
		t.Fatalf("UPDATE_PLATFORM_SETTING audit rows = %d, want exactly 2 (每次变更一条)", n)
	}

	clusterID, who, target, before, after := latestAudit(t, db, "UPDATE_PLATFORM_SETTING")
	if who != actor.Username {
		t.Errorf("audit actor = %q, want %q", who, actor.Username)
	}
	if target != "platform-setting" {
		t.Errorf("audit target = %q, want %q", target, "platform-setting")
	}
	// 设置不属于任何集群。空串撞不上真实集群（集群 ID 校验为非空），
	// 换成 PLATFORM 之类的字面量就可能与一个真叫这个名字的集群重名。
	if clusterID != "" {
		t.Errorf("audit cluster_id = %q, want the empty string —— 设置不属于任何集群", clusterID)
	}

	wantBefore := auditJSON(t, first)
	if !reflect.DeepEqual(before, wantBefore) {
		t.Errorf("audit before_val =\n%v\nwant 上一次保存的那份设置\n%v\n"+
			"—— 前值不对时，「信任锚从什么换成了什么」就答不出来", before, wantBefore)
	}
	wantAfter := auditJSON(t, second)
	if !reflect.DeepEqual(after, wantAfter) {
		t.Errorf("audit after_val =\n%v\nwant\n%v", after, wantAfter)
	}
	// host key 原文必须整条落在审计里，不做裁剪或指纹化：指纹只够回答
	// 「变了没有」，答不出「换成了谁」。
	if got := before["gitVerifyHostKeys"]; got != first.GitVerifyHostKeys {
		t.Errorf("audit before_val.gitVerifyHostKeys = %v, want %q", got, first.GitVerifyHostKeys)
	}
	if got := after["gitVerifyHostKeys"]; got != second.GitVerifyHostKeys {
		t.Errorf("audit after_val.gitVerifyHostKeys = %v, want %q", got, second.GitVerifyHostKeys)
	}
}

// auditJSON 把一份设置按审计行的序列化方式摊成 map，供逐字段比较。
func auditJSON(t *testing.T, s registry.PlatformSetting) map[string]any {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal setting: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal setting: %v", err)
	}
	return m
}

// 列是整数、字段是 Duration，换算错了不会有任何症状 —— 直到某个超时
// 变成原来的一千倍或千分之一。
func TestUpdateSettingRoundTripsThroughTheIntegerColumns(t *testing.T) {
	s, _ := newSettingStore(t)
	ctx := context.Background()

	want := registry.PlatformSetting{
		SessionTTL:          90 * time.Minute,
		HTTPReadTimeout:     11 * time.Second,
		HTTPWriteTimeout:    22 * time.Second,
		HTTPShutdownTimeout: 1500 * time.Millisecond,
		SecretsBackend:      registry.SecretsBackendSecretManager,
		SecretsProject:      "distill-prod",
		SecretsPrefix:       "distill-git-",
		GitVerifyTimeout:    7500 * time.Millisecond,
		GitVerifyHostKeys:   "gitlab.example.com ssh-ed25519 AAAAKEY",
	}
	if err := s.UpdateSetting(ctx, registry.Actor{Username: "admin"}, want); err != nil {
		t.Fatalf("UpdateSetting() error = %v", err)
	}
	got, err := s.Setting(ctx)
	if err != nil {
		t.Fatalf("Setting() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Setting() =\n%+v\nwant\n%+v", got, want)
	}
}

// 校验不合法的设置不得落库：一份 backend 与字段互相矛盾的设置存进去，
// 之后每次读都要面对一个说不清哪个后端在生效的状态。
func TestUpdateSettingRejectsInvalidSettingAndLeavesTheRowAlone(t *testing.T) {
	s, db := newSettingStore(t)
	ctx := context.Background()

	bad := seededSetting()
	bad.SecretsBackend = registry.SecretsBackendDir // dir 却为空
	err := s.UpdateSetting(ctx, registry.Actor{Username: "admin"}, bad)
	if err == nil {
		t.Fatal("UpdateSetting() with an invalid setting succeeded, want it rejected")
	}
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("UpdateSetting() error = %v, want it to wrap registry.ErrInvalid", err)
	}

	got, err := s.Setting(ctx)
	if err != nil {
		t.Fatalf("Setting() error = %v", err)
	}
	if want := seededSetting(); !reflect.DeepEqual(got, want) {
		t.Errorf("setting after a rejected update = %+v, want it untouched at %+v", got, want)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = ?`, "UPDATE_PLATFORM_SETTING",
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 0 {
		t.Errorf("audit rows after a rejected update = %d, want 0 —— 审计记下了一件没发生的事", n)
	}
}

// 秒/毫秒列装不下更细的精度，而整数除法的截断是静默的：500ms 的会话 TTL
// 落成 0 秒之后，读回来就是「会话立即过期」。
func TestUpdateSettingRejectsDurationsThatWouldBeTruncated(t *testing.T) {
	s, _ := newSettingStore(t)
	ctx := context.Background()

	sub := seededSetting()
	sub.SessionTTL = 1500 * time.Millisecond
	if err := s.UpdateSetting(ctx, registry.Actor{Username: "admin"}, sub); err == nil {
		t.Error("UpdateSetting() with a sub-second sessionTtl succeeded, " +
			"want it rejected —— 秒列会把 1.5s 静默截成 1s")
	}

	micro := seededSetting()
	micro.GitVerifyTimeout = 10*time.Second + 500*time.Microsecond
	if err := s.UpdateSetting(ctx, registry.Actor{Username: "admin"}, micro); err == nil {
		t.Error("UpdateSetting() with a sub-millisecond gitVerifyTimeout succeeded, want it rejected")
	}

	got, err := s.Setting(ctx)
	if err != nil {
		t.Fatalf("Setting() error = %v", err)
	}
	if want := seededSetting(); !reflect.DeepEqual(got, want) {
		t.Errorf("setting after rejected updates = %+v, want it untouched at %+v", got, want)
	}
}

// 装着信任锚的那一行，不接受一次把它清空的写入 —— 判定必须落在这条写路径上。
//
// httpapi 那边的同名用例走的是内存替身，只能证明边界层会把拒绝翻译成一条
// 业务失败；证明不了**这一层**真的会拒。而这一层才是所有调用方的必经之处
// （规范 §34：浏览器不是安全边界，一个 handler 也不是唯一的调用方）。
//
// 一并断言写路径没有被这条判定误伤：换成另一份 host key 必须照常成功，
// 否则「不许清空」就变成了「不许修改」，信任锚轮换会因此做不了。
func TestUpdateSettingRefusesToClearTheHostKeys(t *testing.T) {
	s, db := newSettingStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "settings-admin"}

	anchored := seededSetting()
	anchored.GitVerifyHostKeys = "gitlab.example.com ssh-ed25519 AAAAANCHOR"
	if err := s.UpdateSetting(ctx, actor, anchored); err != nil {
		t.Fatalf("UpdateSetting() error = %v", err)
	}

	cleared := anchored
	cleared.GitVerifyHostKeys = ""
	cleared.SessionTTL = time.Hour
	err := s.UpdateSetting(ctx, actor, cleared)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("UpdateSetting() clearing the host keys = %v, want registry.ErrInvalid: "+
			"一次 PUT 就能抹掉平台的 SSH 信任锚，而原文再也读不回来", err)
	}

	got, err := s.Setting(ctx)
	if err != nil {
		t.Fatalf("Setting() error = %v", err)
	}
	if !reflect.DeepEqual(got, anchored) {
		t.Errorf("setting after a rejected clear = %+v, want it untouched at %+v", got, anchored)
	}
	// 被拒的那一次不得留下审计行：一条记着「信任锚被换掉了」的审计，
	// 会让复盘的人去找一次从未发生的变更。
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = ?`, "UPDATE_PLATFORM_SETTING",
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Errorf("audit rows = %d, want 1 —— 只有那次成功的写入该留下记录", n)
	}

	rotated := anchored
	rotated.GitVerifyHostKeys = "gitlab.example.com ssh-ed25519 AAAANEWKEY"
	if err := s.UpdateSetting(ctx, actor, rotated); err != nil {
		t.Fatalf("UpdateSetting() rotating the host keys = %v, want it accepted: "+
			"「不许清空」不得变成「不许修改」", err)
	}
}
