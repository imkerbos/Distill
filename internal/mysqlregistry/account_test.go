package mysqlregistry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
)

// testHash 是一个 bcrypt 形状的假哈希，只在本文件里当作「不该出现在别处
// 的那串字符」使用。
//
// 不用真实口令跑一次 bcrypt：这一层不做哈希，它只负责把调用方给的串落库
// 与不落到别处，而一个字面量在断言失败时能被一眼认出来。
const (
	testHash    = "$2a$10$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testNewHash = "$2a$10$BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

// newAccountStore 返回一个 platform_account 也被清空的 Store。
//
// newTestStore 的清理清单是集群那一侧的表，账号表不在其中：账号跨全部集群
// 存在，硬塞进那份清单会让每个集群测试也去动一张与它无关的表。清理放在
// 这里，账号测试之间的隔离因此不依赖别处的清单有没有被跟着改。
func newAccountStore(t *testing.T) (*mysqlregistry.Store, *sql.DB) {
	t.Helper()
	s, db := newTestStore(t)
	if _, err := db.Exec(`DELETE FROM platform_account`); err != nil {
		t.Fatalf("clean platform_account: %v", err)
	}
	return s, db
}

// mustCreateAccount 建一个账号，失败即终止 —— 后面的断言都以它存在为前提。
func mustCreateAccount(
	t *testing.T, s *mysqlregistry.Store, actor registry.Actor,
	username string, role registry.Role,
) registry.Account {
	t.Helper()
	a := registry.Account{Username: username, Role: role}
	if err := s.CreateAccount(context.Background(), actor, a, testHash); err != nil {
		t.Fatalf("CreateAccount(%s) error = %v", username, err)
	}
	return a
}

// enabledAdmins 返回当前启用中的管理员用户名。
//
// 直接查库而不是走 Accounts()：这些断言要守的是**库里的事实**，
// 用同一层的读方法去证明同一层的写方法，两边一起错时断言照样绿。
func enabledAdmins(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT username FROM platform_account
		  WHERE role = 'ADMIN' AND disabled_at IS NULL AND deleted_at IS NULL
		  ORDER BY username`)
	if err != nil {
		t.Fatalf("query enabled admins: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan enabled admin: %v", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate enabled admins: %v", err)
	}
	return out
}

// auditCount 数某个动作留下了多少条审计行。
func auditCount(t *testing.T, db *sql.DB, action string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatalf("count %s audit rows: %v", action, err)
	}
	return n
}

// 账号登记之后读得回来，并带着自己的审计动作与 target。
//
// 这一条在的意义是让下面几条「必须拒绝」的用例不能靠一份恒为拒绝的实现
// 全部通过。
func TestCreateAndReadBackAccount(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}

	in := mustCreateAccount(t, s, actor, "alice", registry.RoleAdmin)

	got, ok, err := s.Account(ctx, in.Username)
	if err != nil || !ok {
		t.Fatalf("Account() = %+v, %v, %v", got, ok, err)
	}
	if got.Username != in.Username || got.Role != registry.RoleAdmin {
		t.Errorf("round-tripped account = %+v, want %s/%s", got, in.Username, registry.RoleAdmin)
	}
	// 新账号是启用的：DisabledAt 落成零值 time.Time 而不是 nil，会让
	// 「这个账号停用了吗」在 1970 年那个真实时刻上答成"是"。
	if got.DisabledAt != nil {
		t.Errorf("DisabledAt = %v, want nil for a freshly created account", got.DisabledAt)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps = %v/%v, want them filled by the store", got.CreatedAt, got.UpdatedAt)
	}

	list, err := s.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	if len(list) != 1 || list[0].Username != in.Username {
		t.Errorf("Accounts() = %+v, want exactly the one created account", list)
	}

	var target, clusterID string
	if err := db.QueryRow(
		`SELECT target, cluster_id FROM audit_log WHERE action = 'CREATE_ACCOUNT'`,
	).Scan(&target, &clusterID); err != nil {
		t.Fatalf("query CREATE_ACCOUNT audit row: %v", err)
	}
	if target != "account/"+in.Username {
		t.Errorf("CREATE_ACCOUNT target = %q, want %q", target, "account/"+in.Username)
	}
	// 账号不属于任何集群：审计行的 cluster_id 必须是那个撞不上真实集群的
	// 空串，否则它会与某个集群的审计混在 idx_cluster_time 上读不开。
	if clusterID != "" {
		t.Errorf("CREATE_ACCOUNT cluster_id = %q, want empty —— 账号不属于任何集群", clusterID)
	}

	// 用户名不复用：软删除的行仍占着主键，重登记必须被拒绝而不是让新账号
	// 继承旧账号在审计里的身份（design doc 2026-08-14 §3）。
	if err := s.CreateAccount(ctx, actor,
		registry.Account{Username: in.Username, Role: registry.RoleViewer}, testHash,
	); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("CreateAccount() with a taken username err = %v, want ErrInvalid", err)
	}
}

// 停用最后一个启用中的管理员，执行完就没有人能再管理平台
// （design doc 2026-08-14 §5）。
func TestCannotDisableTheLastEnabledAdmin(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}

	mustCreateAccount(t, s, actor, "admin-a", registry.RoleAdmin)
	mustCreateAccount(t, s, actor, "viewer-1", registry.RoleViewer)

	if err := s.DisableAccount(ctx, actor, "admin-a"); !errors.Is(err, registry.ErrLastAdmin) {
		t.Fatalf("DisableAccount() on the last enabled admin err = %v, want ErrLastAdmin", err)
	}
	// 拒绝之后账号必须原封不动。少了这条，一个「先停用再返回 ErrLastAdmin」
	// 的实现照样让上面那行绿。
	if got := enabledAdmins(t, db); len(got) != 1 || got[0] != "admin-a" {
		t.Errorf("enabled admins after a refused disable = %v, want [admin-a]", got)
	}
	// 被拒绝的操作不得留下审计行：审计里的一条 DISABLE_ACCOUNT 会说一件
	// 没发生过的事。
	if n := auditCount(t, db, "DISABLE_ACCOUNT"); n != 0 {
		t.Errorf("DISABLE_ACCOUNT audit rows after a refused disable = %d, want 0", n)
	}

	// 只读账号不在保护范围内：拒绝的是「最后一个管理员」，不是「停用」本身。
	if err := s.DisableAccount(ctx, actor, "viewer-1"); err != nil {
		t.Fatalf("DisableAccount() on a viewer error = %v", err)
	}

	// 第二个管理员到位之后停得掉。
	mustCreateAccount(t, s, actor, "admin-b", registry.RoleAdmin)
	if err := s.DisableAccount(ctx, actor, "admin-a"); err != nil {
		t.Fatalf("DisableAccount() with a second admin present error = %v", err)
	}
	if got := enabledAdmins(t, db); len(got) != 1 || got[0] != "admin-b" {
		t.Fatalf("enabled admins = %v, want [admin-b]", got)
	}
	// 停用中的管理员不算数：admin-a 还在库里，但它已经管不了平台，
	// 拿它当"还有第二个管理员"就等于允许把最后一个也关掉。
	if err := s.DisableAccount(ctx, actor, "admin-b"); !errors.Is(err, registry.ErrLastAdmin) {
		t.Errorf("DisableAccount() with only a disabled admin left err = %v, want ErrLastAdmin", err)
	}

	// 启用不受判定影响：它只会让管理员变多。
	if err := s.EnableAccount(ctx, actor, "admin-a"); err != nil {
		t.Fatalf("EnableAccount() error = %v", err)
	}
	if got := enabledAdmins(t, db); len(got) != 2 {
		t.Errorf("enabled admins after re-enabling = %v, want both", got)
	}

	if err := s.DisableAccount(ctx, actor, "nobody"); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("DisableAccount() on a missing account err = %v, want ErrNotFound", err)
	}
}

// 降级最后一个启用中的管理员，与停用它是同一件事的另一条路径。
func TestCannotDemoteTheLastEnabledAdmin(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}

	mustCreateAccount(t, s, actor, "admin-a", registry.RoleAdmin)
	mustCreateAccount(t, s, actor, "viewer-1", registry.RoleViewer)

	if err := s.UpdateAccountRole(ctx, actor, "admin-a", registry.RoleViewer); !errors.Is(err, registry.ErrLastAdmin) {
		t.Fatalf("UpdateAccountRole() demoting the last enabled admin err = %v, want ErrLastAdmin", err)
	}
	if got := enabledAdmins(t, db); len(got) != 1 || got[0] != "admin-a" {
		t.Errorf("enabled admins after a refused demotion = %v, want [admin-a]", got)
	}
	if n := auditCount(t, db, "UPDATE_ACCOUNT_ROLE"); n != 0 {
		t.Errorf("UPDATE_ACCOUNT_ROLE audit rows after a refused demotion = %d, want 0", n)
	}

	// 把最后一个管理员的角色原样再设一次不该被拒绝：这次写入不会让
	// 管理员变少，而一个"凡是碰到最后一个管理员就拒绝"的实现会挡下它。
	if err := s.UpdateAccountRole(ctx, actor, "admin-a", registry.RoleAdmin); err != nil {
		t.Errorf("UpdateAccountRole() setting the last admin's role to ADMIN again error = %v", err)
	}

	// 提权不受判定影响，提完之后降级就通得过。
	if err := s.UpdateAccountRole(ctx, actor, "viewer-1", registry.RoleAdmin); err != nil {
		t.Fatalf("UpdateAccountRole() promoting a viewer error = %v", err)
	}
	if err := s.UpdateAccountRole(ctx, actor, "admin-a", registry.RoleViewer); err != nil {
		t.Fatalf("UpdateAccountRole() with a second admin present error = %v", err)
	}
	if got := enabledAdmins(t, db); len(got) != 1 || got[0] != "viewer-1" {
		t.Fatalf("enabled admins = %v, want [viewer-1]", got)
	}
	if err := s.UpdateAccountRole(ctx, actor, "viewer-1", registry.RoleViewer); !errors.Is(err, registry.ErrLastAdmin) {
		t.Errorf("UpdateAccountRole() demoting the remaining admin err = %v, want ErrLastAdmin", err)
	}

	// 封闭枚举：一个不在枚举里的角色在 Permits 下什么都做不了，
	// 而操作者以为自己改成了某个角色。
	if err := s.UpdateAccountRole(ctx, actor, "admin-a", registry.Role("SUPERADMIN")); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("UpdateAccountRole() with an unregistered role err = %v, want ErrInvalid", err)
	}
	if err := s.UpdateAccountRole(ctx, actor, "nobody", registry.RoleViewer); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("UpdateAccountRole() on a missing account err = %v, want ErrNotFound", err)
	}
}

// 软删除最后一个启用中的管理员，与停用、降级它是同一件事的第三条路径。
func TestCannotSoftDeleteTheLastEnabledAdmin(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}

	mustCreateAccount(t, s, actor, "admin-a", registry.RoleAdmin)

	if err := s.SoftDeleteAccount(ctx, actor, "admin-a"); !errors.Is(err, registry.ErrLastAdmin) {
		t.Fatalf("SoftDeleteAccount() on the last enabled admin err = %v, want ErrLastAdmin", err)
	}
	if _, ok, err := s.Account(ctx, "admin-a"); err != nil || !ok {
		t.Errorf("Account() after a refused delete = %v, %v; want it still live", ok, err)
	}
	if n := auditCount(t, db, "DELETE_ACCOUNT"); n != 0 {
		t.Errorf("DELETE_ACCOUNT audit rows after a refused delete = %d, want 0", n)
	}

	mustCreateAccount(t, s, actor, "admin-b", registry.RoleAdmin)
	if err := s.SoftDeleteAccount(ctx, actor, "admin-a"); err != nil {
		t.Fatalf("SoftDeleteAccount() with a second admin present error = %v", err)
	}
	if _, ok, err := s.Account(ctx, "admin-a"); err != nil || ok {
		t.Errorf("Account() after delete = %v, %v; want it gone", ok, err)
	}
	// 删掉的管理员不算数，否则最后一个也删得掉。
	if err := s.SoftDeleteAccount(ctx, actor, "admin-b"); !errors.Is(err, registry.ErrLastAdmin) {
		t.Errorf("SoftDeleteAccount() on the remaining admin err = %v, want ErrLastAdmin", err)
	}
	// 软删除的行仍占着主键，因此对它的后续操作是「不存在」而不是「成功」。
	if err := s.DisableAccount(ctx, actor, "admin-a"); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("DisableAccount() on a soft-deleted account err = %v, want ErrNotFound", err)
	}
}

// 两个管理员同时把对方降级：先查后改会双双通过，平台落地成零个管理员，
// 没有人能把它改回来（design doc §5，规范 §25）。
//
// 这条用例真的制造并发，而不是把两次调用先后跑一遍 —— 后者对「先查后改」
// 的实现同样是绿的，因为第二次调用查到的已经是第一次写完的状态。
// 造并发的办法是让测试自己持有一把锁，把两个降级都堵在同一个点上：
//
//   - 测试开一个事务，对全部启用中的管理员行做 SELECT ... FOR UPDATE；
//   - 两个降级并发发起，各自撞上这把锁而停住；
//   - 等到确实有两个事务处于 LOCK WAIT（不是 sleep 一个猜出来的时长）
//     才释放，此时两边都已经越过了各自的"判定"那一步；
//   - 释放之后两边继续跑完。
//
// 两种实现在这里分岔，而这正是本用例要能分辨的事：
//
//   - 判定在事务内、对候选行加锁：两个降级都被堵在**判定之前**，释放后
//     一个先拿到锁、降级成功并提交，另一个拿到锁时读到的是当前状态
//     （FOR UPDATE 是当前读），只剩一个管理员，于是返回 ErrLastAdmin。
//   - 判定在事务外（先查后改）：两个降级的判定发生在锁释放**之前**，
//     双双读到"还有两个管理员"而通过，堵住的是它们的 UPDATE；释放之后
//     两条 UPDATE 都落地，库里一个启用中的管理员都不剩，本用例转红。
func TestConcurrentMutualDemotionLeavesOneAdmin(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}

	mustCreateAccount(t, s, actor, "admin-a", registry.RoleAdmin)
	mustCreateAccount(t, s, actor, "admin-b", registry.RoleAdmin)

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	// 出错路径上也要放锁，否则后续测试会跟着一起卡住。
	defer func() { _ = blocker.Rollback() }()
	lockEnabledAdminRows(t, blocker)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, victim := range []string{"admin-a", "admin-b"} {
		wg.Add(1)
		go func(username string) {
			defer wg.Done()
			results <- s.UpdateAccountRole(ctx, actor, username, registry.RoleViewer)
		}(victim)
	}

	// 等两个降级都真的停在锁上再放行。少了这一步，两次调用可能一前一后
	// 跑完，而那不是并发 —— 那正是这条用例要避免的形状。
	waitForRowLockWaiters(t, db, 2)
	if err := blocker.Rollback(); err != nil {
		t.Fatalf("Rollback() of the blocking tx error = %v", err)
	}

	wg.Wait()
	close(results)
	var succeeded, refused int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, registry.ErrLastAdmin):
			refused++
		default:
			t.Fatalf("concurrent UpdateAccountRole() err = %v, want nil or ErrLastAdmin", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Errorf("concurrent mutual demotion: %d succeeded, %d refused; want exactly one of each",
			succeeded, refused)
	}
	// 这一行才是真正要守的东西：无论哪一边先跑完，平台都必须还有人能管。
	if got := enabledAdmins(t, db); len(got) != 1 {
		t.Fatalf("enabled admins after a concurrent mutual demotion = %v, want exactly one —— "+
			"两个管理员同时把对方降级把平台锁死了", got)
	}
	// 被拒绝的那一次不得留下审计行。
	if n := auditCount(t, db, "UPDATE_ACCOUNT_ROLE"); n != 1 {
		t.Errorf("UPDATE_ACCOUNT_ROLE audit rows = %d, want exactly 1 (the demotion that went through)", n)
	}
}

// 换一个大小写不能绕过「最后一个启用中的管理员」的保护（design doc §5）。
//
// 库与 Go 对「同一个用户名」的判定不是同一件事：platform_account 的排序规则
// 是 utf8mb4_0900_ai_ci，`WHERE username = ?` 认得出 "ALICE" 就是 "alice"
// 那一行，Go 的 == 认不出。守卫拿两边直接比，就会判定为"动的不是最后那一个
// 管理员"而放行，紧接着的 UPDATE 走 SQL 比较，动的恰恰就是它。上面那三条
// 用例（停用/降级/删除）全部只用同一个大小写调用，一条都拦不住这个形状。
//
// 断言分两层：返回 ErrLastAdmin，以及**库里仍然有那一个启用中的管理员** ——
// 后者才是真正要守的东西。没有配置引导账号的部署一旦落到零个启用中的管理员，
// 就只能改数据库救。
func TestUsernameCaseDoesNotWalkPastTheLastAdminGuard(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}

	mustCreateAccount(t, s, actor, "alice", registry.RoleAdmin)

	// 这条用例的前提是库把 "ALICE" 与 "alice" 当成同一行。前提不成立时
	// 下面的期望本身就变了（那时这三次调用应当是 ErrNotFound），与其让断言
	// 报一个看不懂的错，不如在这里把话说清楚。
	var matched int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM platform_account WHERE username = 'ALICE'`).Scan(&matched); err != nil {
		t.Fatalf("count rows matching 'ALICE': %v", err)
	}
	if matched != 1 {
		t.Fatalf("'ALICE' 匹配到 %d 行，本用例的前提是 1 —— platform_account 的排序规则"+
			"不再是大小写不敏感的那一种，这条用例要跟着重写", matched)
	}

	for _, tc := range []struct {
		name   string
		action string
		call   func() error
	}{
		{"disable", "DISABLE_ACCOUNT", func() error { return s.DisableAccount(ctx, actor, "ALICE") }},
		{"demote", "UPDATE_ACCOUNT_ROLE", func() error {
			return s.UpdateAccountRole(ctx, actor, "ALICE", registry.RoleViewer)
		}},
		{"delete", "DELETE_ACCOUNT", func() error { return s.SoftDeleteAccount(ctx, actor, "ALICE") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, registry.ErrLastAdmin) {
				t.Errorf("%s of the last enabled admin spelled 'ALICE' err = %v, want ErrLastAdmin",
					tc.name, err)
			}
			if got := enabledAdmins(t, db); len(got) != 1 || got[0] != "alice" {
				t.Fatalf("enabled admins after a case-varied %s = %v, want [alice] —— "+
					"改一下大小写就把平台落地成零个启用中的管理员了", tc.name, got)
			}
			if n := auditCount(t, db, tc.action); n != 0 {
				t.Errorf("%s audit rows after a refused %s = %d, want 0", tc.action, tc.name, n)
			}
		})
	}
}

// 改密的审计动作按**库认的那一份用户名**判，不按调用方打的那一份。
//
// actor 来自会话（登录时打的写法），目标来自路由，两者可能只差大小写，而库
// 把它们当成同一行。按调用方给的写法比，一次自助改密会被记成
// RESET_PASSWORD ——「管理员重置了别人的密码」，审计从此答错了「谁对谁做了
// 什么」（design doc §6、§7，规范 §43）。
func TestPasswordChangeAuditFollowsTheStoredUsername(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()

	mustCreateAccount(t, s, registry.Actor{Username: "bootstrap"}, "carol", registry.RoleAdmin)

	// carol 用 "CAROL" 这个写法登进来，改的是自己的密码。
	if err := s.SetAccountPassword(ctx, registry.Actor{Username: "CAROL"}, "carol", testNewHash); err != nil {
		t.Fatalf("SetAccountPassword() error = %v", err)
	}
	if n := auditCount(t, db, "CHANGE_OWN_PASSWORD"); n != 1 {
		t.Errorf("CHANGE_OWN_PASSWORD audit rows = %d, want 1 —— 这是一次自助改密", n)
	}
	if n := auditCount(t, db, "RESET_PASSWORD"); n != 0 {
		t.Errorf("RESET_PASSWORD audit rows = %d, want 0 —— "+
			"一次自助改密被记成了「管理员重置了别人的密码」", n)
	}

	// 审计的 target 也用库里那一份写法，否则同一个账号会在 audit_log 里
	// 留下两种写法，聚合不到一起。
	var target string
	if err := db.QueryRow(
		`SELECT target FROM audit_log WHERE action = 'CHANGE_OWN_PASSWORD'`).Scan(&target); err != nil {
		t.Fatalf("query CHANGE_OWN_PASSWORD audit row: %v", err)
	}
	if target != "account/carol" {
		t.Errorf("CHANGE_OWN_PASSWORD target = %q, want account/carol", target)
	}

	// 反方向：真的重置别人的密码仍然记 RESET_PASSWORD，别让上面那条被一份
	// 「恒为 CHANGE_OWN_PASSWORD」的实现蒙混过去。
	mustCreateAccount(t, s, registry.Actor{Username: "carol"}, "dave", registry.RoleViewer)
	if err := s.SetAccountPassword(ctx, registry.Actor{Username: "CAROL"}, "dave", testNewHash); err != nil {
		t.Fatalf("SetAccountPassword() on another account error = %v", err)
	}
	if n := auditCount(t, db, "RESET_PASSWORD"); n != 1 {
		t.Errorf("RESET_PASSWORD audit rows = %d, want 1 —— 管理员重置他人密码", n)
	}
}

// 事务外的存在性预检与事务内的写入之间有一段窗口，别的事务可以在其中把这一行
// 软删除掉。requireLiveAccount 的加锁读把这段窗口关掉 —— 没有它，写入会落进
// 一条已经删除的行，还留下一条说这件事发生过的审计（规范 §25、§43）。
//
// **五个调用点逐个走一遍，而不是只测函数本身。** 守卫写对了不等于调用方还在
// 调它：把某一处的调用删掉，只覆盖别处的用例照样绿，而那正是这个平台反复出现
// 的形状。这条用例的每个子用例只盯一个调用点，删掉哪一个就红哪一个。
//
// 并发是真的造出来的，办法与 TestConcurrentMutualDemotionLeavesOneAdmin 同源：
// 测试自己开一个事务把目标行软删除但不提交，被测的写入因此堵在那一行的锁上；
// 等到 MySQL 自己报告确实有事务在等这把锁（不是 sleep 一个猜出来的时长）再
// 提交，此时那次写入已经越过了事务外的预检。两种实现在这里分岔：
//
//   - 有 requireLiveAccount：拿到锁之后那次 SELECT ... FOR UPDATE 是当前读，
//     看见 deleted_at 已经落上，返回 ErrNotFound，什么都没写。
//   - 没有 requireLiveAccount：各条 UPDATE 的谓词里都没有 deleted_at，锁一
//     放开就把改动写进那条已删除的行并提交，两条断言同时转红。
func TestConcurrentSoftDeleteStopsEveryAccountWrite(t *testing.T) {
	const victim = "viewer-1"
	actor := registry.Actor{Username: "admin-a"}

	for _, tc := range []struct {
		name string
		// action 是这条路径成功时会留下的审计动作。它必须一条都没有 ——
		// 一条留下来的审计行会说一件没发生过的事。
		action string
		// disabledFirst 让目标先被停用，启用那条路径才有东西可启用。
		disabledFirst bool
		call          func(ctx context.Context, s *mysqlregistry.Store) error
	}{
		{"update-role", "UPDATE_ACCOUNT_ROLE", false,
			func(ctx context.Context, s *mysqlregistry.Store) error {
				return s.UpdateAccountRole(ctx, actor, victim, registry.RoleAdmin)
			}},
		{"disable", "DISABLE_ACCOUNT", false,
			func(ctx context.Context, s *mysqlregistry.Store) error {
				return s.DisableAccount(ctx, actor, victim)
			}},
		{"enable", "ENABLE_ACCOUNT", true,
			func(ctx context.Context, s *mysqlregistry.Store) error {
				return s.EnableAccount(ctx, actor, victim)
			}},
		{"soft-delete", "DELETE_ACCOUNT", false,
			func(ctx context.Context, s *mysqlregistry.Store) error {
				return s.SoftDeleteAccount(ctx, actor, victim)
			}},
		{"set-password", "RESET_PASSWORD", false,
			func(ctx context.Context, s *mysqlregistry.Store) error {
				return s.SetAccountPassword(ctx, actor, victim, testNewHash)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newAccountStore(t)
			ctx := context.Background()
			// 留一个启用中的管理员，下面这些操作才不会撞上最后管理员判定 ——
			// 那条守卫先返回，本用例就测不到 requireLiveAccount 了。
			mustCreateAccount(t, s, registry.Actor{Username: "bootstrap"}, "admin-a", registry.RoleAdmin)
			mustCreateAccount(t, s, actor, victim, registry.RoleViewer)
			if tc.disabledFirst {
				if err := s.DisableAccount(ctx, actor, victim); err != nil {
					t.Fatalf("DisableAccount() setup error = %v", err)
				}
			}

			deleter, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("BeginTx() error = %v", err)
			}
			// 出错路径上也要放锁，否则后续测试会跟着一起卡住。
			defer func() { _ = deleter.Rollback() }()
			at := time.Now().UTC()
			if _, err := deleter.ExecContext(ctx,
				`UPDATE platform_account SET deleted_at = ?, updated_at = ? WHERE username = ?`,
				at, at, victim); err != nil {
				t.Fatalf("soft delete %s in the blocking tx: %v", victim, err)
			}

			done := make(chan error, 1)
			go func() { done <- tc.call(ctx, s) }()

			// 等那次写入真的停在目标行的锁上再提交删除。少了这一步，删除可能
			// 在事务外的预检之前就提交完，那时预检自己就会返回 ErrNotFound ——
			// 用例照样绿，却与 requireLiveAccount 在不在毫无关系。
			waitForRowLockWaiters(t, db, 1)
			if err := deleter.Commit(); err != nil {
				t.Fatalf("Commit() of the deleting tx error = %v", err)
			}

			if err := <-done; !errors.Is(err, mysqlregistry.ErrNotFound) {
				t.Errorf("%s racing a soft delete err = %v, want ErrNotFound", tc.name, err)
			}
			if n := auditCount(t, db, tc.action); n != 0 {
				t.Errorf("%s audit rows = %d, want 0 —— 这次写入没有发生", tc.action, n)
			}
			// 已删除的行上不得留下新哈希：那条行不再是一个身份，而一份能验过
			// 新口令的哈希会在它被恢复的那一天成为一条谁也不记得的登录路径。
			var stored string
			if err := db.QueryRow(
				`SELECT password_hash FROM platform_account WHERE username = ?`,
				victim).Scan(&stored); err != nil {
				t.Fatalf("query password_hash: %v", err)
			}
			if stored != testHash {
				t.Errorf("password_hash of the soft-deleted account changed —— " +
					"新哈希被写进了一条已删除的行")
			}
		})
	}
}

// lockEnabledAdminRows 在给定事务里锁住全部启用中的管理员行。
//
// 谓词与实现里那条判定一致：锁的是同一批行，两条并发写路径因此一定撞上。
func lockEnabledAdminRows(t *testing.T, tx *sql.Tx) {
	t.Helper()
	rows, err := tx.Query(
		`SELECT username FROM platform_account
		  WHERE role = 'ADMIN' AND disabled_at IS NULL AND deleted_at IS NULL
		  ORDER BY username FOR UPDATE`)
	if err != nil {
		t.Fatalf("lock enabled admin rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var n int
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan locked admin: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate locked admins: %v", err)
	}
	if n != 2 {
		t.Fatalf("locked %d enabled admin rows, want 2 —— 这条用例的前提是两个管理员都在", n)
	}
}

// waitForRowLockWaiters 等到至少 want 个事务正在等 platform_account 上的行锁。
//
// 用 MySQL 自己报告的锁等待关系而不是 sleep 一个猜出来的时长：sleep 太短
// 会让两个降级实际上一前一后跑完，而那种"并发"对先查后改的实现同样是
// 绿的 —— 一条分辨不出两种实现的并发用例，比没有更糟，因为它看起来像
// 已经守住了。等不到就直接失败，不带着一个没成立的前提往下跑。
//
// 判据取自 performance_schema.data_lock_waits，不是
// information_schema.innodb_trx.trx_state = 'LOCK WAIT'：本机这套 MySQL 8.4
// 上，两个事务确实卡在行锁上等着（data_lock_waits 有它们的等待关系），
// trx_state 却一直是 RUNNING，照 trx_state 判会永远等不到而误报"没有并发"。
// data_lock_waits 描述的是"谁在等谁的哪把锁"这件事本身，与状态字段怎么
// 刷新无关。按库名与表名收窄，别处的锁等待不会被算进来。
func waitForRowLockWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
			   FROM performance_schema.data_lock_waits w
			   JOIN performance_schema.data_locks l
			     ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
			  WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = 'platform_account'`,
		).Scan(&n); err != nil {
			t.Fatalf("query data_lock_waits: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d transactions were waiting on platform_account row locks within "+
				"the deadline, want %d —— 没等到真正的并发，这条用例无法分辨事务内判定与先查后改",
				n, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// realHash 生成一个真的 bcrypt 哈希。
//
// 与 testHash 那个字面量分工不同：字面量用来断言"这串字符不该出现在别处"，
// 而这里要断言的是"读回来的哈希确实是刚写进去的那一份"，唯一能证明它的
// 办法是拿它去验一次密码 —— PasswordHash 不给取出字节的出口，比较字符串
// 这条路本来就不存在。
func realHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	return string(h)
}

// 库里的账号必须有一条读得回自己密码哈希的路径，否则平台在第一次装好之后
// 立刻不可用：引导账号建出第一个管理员，引导闸门随即关上，而那个管理员
// 没有任何一条能登录的路（design doc 2026-08-14 §2）。
//
// 顺带钉死这条路径的谓词：停用与软删除的账号读不到哈希，因此连会话都
// 换不到（与 RoleOf 对停用的处置一致）。
func TestAccountPasswordHashReadsBackTheStoredHash(t *testing.T) {
	s, _ := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}
	const password = "correct-horse-battery"

	if err := s.CreateAccount(ctx, actor,
		registry.Account{Username: "alice", Role: registry.RoleAdmin}, realHash(t, password),
	); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	h, ok, err := s.AccountPasswordHash(ctx, "alice")
	if err != nil || !ok {
		t.Fatalf("AccountPasswordHash() = (_, %v, %v), want the stored hash", ok, err)
	}
	if !h.Matches(password) {
		t.Error("读回来的哈希验不过刚写进去的那个密码 —— 库里的账号登不进来")
	}
	// 反方向：一份恒为 true 的 Matches，或者读到了别人的哈希，都要被认出来。
	if h.Matches("not-the-password") {
		t.Error("读回来的哈希验过了一个错误密码")
	}

	// 改完密码之后读到的是新的那一份，不是一份陈旧的副本。
	const newPassword = "another-correct-horse"
	if err := s.SetAccountPassword(ctx, actor, "alice", realHash(t, newPassword)); err != nil {
		t.Fatalf("SetAccountPassword() error = %v", err)
	}
	h, ok, err = s.AccountPasswordHash(ctx, "alice")
	if err != nil || !ok {
		t.Fatalf("AccountPasswordHash() after a password change = (_, %v, %v)", ok, err)
	}
	if !h.Matches(newPassword) || h.Matches(password) {
		t.Error("改密之后读到的仍是旧哈希 —— 旧口令还能登录，新口令登不进来")
	}

	if _, ok, err := s.AccountPasswordHash(ctx, "nobody"); err != nil || ok {
		t.Errorf("AccountPasswordHash() for a missing account = (%v, %v), want (false, nil)", ok, err)
	}

	// 停用的账号读不到哈希。需要第二个管理员，否则停用会被最后管理员判定拦下。
	mustCreateAccount(t, s, actor, "admin-b", registry.RoleAdmin)
	if err := s.DisableAccount(ctx, actor, "alice"); err != nil {
		t.Fatalf("DisableAccount() error = %v", err)
	}
	if _, ok, err := s.AccountPasswordHash(ctx, "alice"); err != nil || ok {
		t.Errorf("AccountPasswordHash() for a disabled account = (%v, %v), want (false, nil) —— "+
			"停用的账号不该还能换到一张会话", ok, err)
	}
	// 重新启用之后又读得到：挡下的是"停用"这个状态，不是这个账号。
	if err := s.EnableAccount(ctx, actor, "alice"); err != nil {
		t.Fatalf("EnableAccount() error = %v", err)
	}
	if _, ok, err := s.AccountPasswordHash(ctx, "alice"); err != nil || !ok {
		t.Errorf("AccountPasswordHash() after re-enabling = (%v, %v), want (true, nil)", ok, err)
	}

	// 软删除的账号同样读不到：那条行还占着主键，但它不再是一个身份。
	if err := s.SoftDeleteAccount(ctx, actor, "alice"); err != nil {
		t.Fatalf("SoftDeleteAccount() error = %v", err)
	}
	if _, ok, err := s.AccountPasswordHash(ctx, "alice"); err != nil || ok {
		t.Errorf("AccountPasswordHash() for a soft-deleted account = (%v, %v), want (false, nil)", ok, err)
	}
}

// 引导账号登录必须写审计（design doc 2026-08-14 §2，规范 §43）。
//
// 这不是记账：引导口重新可用，意味着平台此刻一个启用中的管理员都不剩。
// 那是一次事故，而事故必须在事后可见 —— 少了这条审计行，"谁在什么时候
// 从逃生口进来的"永远答不出来。
func TestRecordBootstrapLoginWritesAnAuditRow(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()

	if err := s.RecordBootstrapLogin(ctx, registry.Actor{Username: "demo"}); err != nil {
		t.Fatalf("RecordBootstrapLogin() error = %v", err)
	}

	if n := auditCount(t, db, "BOOTSTRAP_LOGIN"); n != 1 {
		t.Fatalf("BOOTSTRAP_LOGIN audit rows = %d, want exactly 1", n)
	}
	var actorCol, target, clusterID string
	var before, after sql.NullString
	if err := db.QueryRow(
		`SELECT actor, target, cluster_id, before_val, after_val
		   FROM audit_log WHERE action = 'BOOTSTRAP_LOGIN'`,
	).Scan(&actorCol, &target, &clusterID, &before, &after); err != nil {
		t.Fatalf("query BOOTSTRAP_LOGIN audit row: %v", err)
	}
	if actorCol != "demo" {
		t.Errorf("actor = %q, want demo —— 审计要答得出「谁」（design doc §7）", actorCol)
	}
	if target != "account/demo" {
		t.Errorf("target = %q, want account/demo", target)
	}
	// 引导登录与账号一样不属于任何集群：空串是唯一撞不上真实集群的取值，
	// 换成别的字面量会与一个真叫这个名字的集群在 idx_cluster_time 上混成
	// 一份读不开的记录（见 settingClusterID）。
	if clusterID != "" {
		t.Errorf("cluster_id = %q, want empty —— 引导登录不属于任何集群", clusterID)
	}
	if before.Valid || after.Valid {
		t.Errorf("before/after = %v/%v, want both NULL —— 这次登录没有改变任何状态",
			before, after)
	}

	// 它不建账号：引导账号只在配置文件里，一条把它写进 platform_account 的
	// 实现会让引导闸门自己关上自己 —— 那个"管理员"没有人建过、也没有人
	// 能把它删掉。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM platform_account`).Scan(&n); err != nil {
		t.Fatalf("count platform_account: %v", err)
	}
	if n != 0 {
		t.Errorf("platform_account rows = %d, want 0 —— 引导登录不得往账号表里写东西", n)
	}
}

// 密码哈希不得出现在读模型、审计行或错误里（design doc §3、§6，
// 规范 §19、§21、§43）。
//
// 三个方向一起断言：
//
//   - 读模型：类型上就没有这个字段，序列化整条记录也带不出它；
//   - 审计：CREATE_ACCOUNT 与改密的前后值只说"密码变过"，不说变成了什么；
//   - 反过来的一条 —— 哈希确实落进了 password_hash 列。少了它，一个根本
//     没保存哈希的实现也能让上面两条绿，而那时"没泄漏"只是因为没有东西
//     可泄漏。
func TestAccountReadsCarryNoHash(t *testing.T) {
	s, db := newAccountStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "bootstrap"}

	mustCreateAccount(t, s, actor, "alice", registry.RoleAdmin)

	// 读模型的类型上不该有任何与密码有关的字段：这一条守的是「将来有人
	// 为了省一次查询把哈希挂上来」，那之后每一条返回账号的路径都会默默
	// 把它带出去。
	rt := reflect.TypeOf(registry.Account{})
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(name, "hash") || strings.Contains(name, "password") {
			t.Errorf("registry.Account has field %q; 读模型不得携带密码字段",
				rt.Field(i).Name)
		}
	}

	got, ok, err := s.Account(ctx, "alice")
	if err != nil || !ok {
		t.Fatalf("Account() = %+v, %v, %v", got, ok, err)
	}
	list, err := s.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	for _, v := range []any{got, list} {
		blob, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal read model: %v", err)
		}
		if strings.Contains(string(blob), testHash) {
			t.Errorf("read model %s carries the password hash", blob)
		}
	}

	// 改密：管理员重置他人密码与本人改自己的密码走同一条写路径，
	// 区别只在审计动作。
	if err := s.SetAccountPassword(ctx, actor, "alice", testNewHash); err != nil {
		t.Fatalf("SetAccountPassword() error = %v", err)
	}
	if n := auditCount(t, db, "RESET_PASSWORD"); n != 1 {
		t.Errorf("RESET_PASSWORD audit rows = %d, want 1 —— 管理员重置他人密码", n)
	}
	if err := s.SetAccountPassword(ctx, registry.Actor{Username: "alice"}, "alice", testHash); err != nil {
		t.Fatalf("SetAccountPassword() by the account itself error = %v", err)
	}
	if n := auditCount(t, db, "CHANGE_OWN_PASSWORD"); n != 1 {
		t.Errorf("CHANGE_OWN_PASSWORD audit rows = %d, want 1 —— 本人改自己的密码", n)
	}

	// 审计只记「密码变过」这个事实。哈希落进审计等于把一份离线爆破材料
	// 复制进一张为了长期留存的表里。
	_, after := auditPayload(t, db, "CHANGE_OWN_PASSWORD")
	if changed, _ := after["passwordChanged"].(bool); !changed {
		t.Errorf("CHANGE_OWN_PASSWORD after_val = %v, want it to state that the password changed", after)
	}

	// 一条也不放过：扫全表，任何审计行的前后值都不得含任一个哈希。
	rows, err := db.Query(`SELECT action, COALESCE(before_val, ''), COALESCE(after_val, '') FROM audit_log`)
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var action, before, afterVal string
		if err := rows.Scan(&action, &before, &afterVal); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		for _, h := range []string{testHash, testNewHash} {
			if strings.Contains(before, h) || strings.Contains(afterVal, h) {
				t.Errorf("audit row %s carries the password hash", action)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}

	// 反方向：哈希确实被保存了，只是没有被读出来过。
	var stored string
	if err := db.QueryRow(
		`SELECT password_hash FROM platform_account WHERE username = 'alice'`).Scan(&stored); err != nil {
		t.Fatalf("query password_hash: %v", err)
	}
	if stored != testHash {
		t.Errorf("stored password_hash = %q, want the latest hash written", stored)
	}

	if err := s.SetAccountPassword(ctx, actor, "nobody", testHash); !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("SetAccountPassword() on a missing account err = %v, want ErrNotFound", err)
	}
}
