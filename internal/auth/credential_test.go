package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/registry"
)

func hashOf(t *testing.T, plain string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return string(h)
}

// fakeAccounts 是 auth.AccountSource 的内存实现。
//
// 只实现读：本包要证明的是「角色现读」，而不是账号表怎么写的。测试用
// 直接改这里的记录来模拟另一条路径（管理员在界面上）刚刚改完权限。
type fakeAccounts struct {
	accounts map[string]registry.Account
	// hashes 与 accounts 分开存，正如库里读模型与 password_hash 分成两条
	// 读路径：把哈希挂进 registry.Account 是这一层刻意表达不出来的事。
	hashes map[string]string
	err    error
}

func newFakeAccounts(list ...registry.Account) *fakeAccounts {
	m := make(map[string]registry.Account, len(list))
	for _, a := range list {
		m[a.Username] = a
	}
	return &fakeAccounts{accounts: m, hashes: map[string]string{}}
}

func (f *fakeAccounts) Accounts(_ context.Context) ([]registry.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]registry.Account, 0, len(f.accounts))
	for _, a := range f.accounts {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeAccounts) Account(_ context.Context, username string) (registry.Account, bool, error) {
	if f.err != nil {
		return registry.Account{}, false, f.err
	}
	a, ok := f.accounts[username]
	return a, ok, nil
}

// AccountPasswordHash 复刻存储层的谓词：不存在、已停用、已软删除一律
// 返回 false。照抄这条谓词是本假实现存在的意义 —— 少抄一条，
// 「停用的账号还能登录」在这里就永远测不出来。
func (f *fakeAccounts) AccountPasswordHash(
	_ context.Context, username string,
) (registry.PasswordHash, bool, error) {
	if f.err != nil {
		return registry.PasswordHash{}, false, f.err
	}
	a, ok := f.accounts[username]
	if !ok || a.DisabledAt != nil {
		return registry.PasswordHash{}, false, nil
	}
	h, ok := f.hashes[username]
	if !ok {
		return registry.PasswordHash{}, false, nil
	}
	return registry.NewPasswordHash(h), true, nil
}

// setPassword 给一个账号配上哈希，模拟它在库里真的有一条口令。
func (f *fakeAccounts) setPassword(username, hash string) {
	f.hashes[username] = hash
}

// disable 把一个账号标成停用；软删除则直接从表里移除，因为 Account 与
// Accounts 都只返回未删除的行。
func (f *fakeAccounts) disable(username string) {
	a := f.accounts[username]
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	a.DisabledAt = &at
	f.accounts[username] = a
}

func (f *fakeAccounts) softDelete(username string) {
	delete(f.accounts, username)
}

func (f *fakeAccounts) setRole(username string, role registry.Role) {
	a := f.accounts[username]
	a.Role = role
	f.accounts[username] = a
}

func enabledAccount(username string, role registry.Role) registry.Account {
	return registry.Account{Username: username, Role: role}
}

// bootstrapUser 是测试里配置文件那一份引导账号。
func bootstrapUser(t *testing.T) config.User {
	t.Helper()
	return config.User{Username: "demo", PasswordHash: hashOf(t, "bootstrap-secret")}
}

// 降权必须立刻生效，而不是等会话过期。会话只带用户名，角色在判定时现读。
//
// 断言用的是一张**签发在降权之前**的会话：这正是事故形状 —— 会话还在
// 手里，权限已经被收走了（design doc 2026-08-14 §4）。
func TestDemotionTakesEffectOnTheNextRequest(t *testing.T) {
	ctx := context.Background()
	accounts := newFakeAccounts(
		enabledAccount("ops", registry.RoleAdmin),
		enabledAccount("root", registry.RoleAdmin),
	)
	v := auth.NewVerifier(bootstrapUser(t), accounts)

	sessions := auth.NewSessionStore(8*time.Hour, nil)
	sess, err := sessions.Create("ops")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	role, ok, err := v.RoleOf(ctx, sess.Username)
	if err != nil || !ok {
		t.Fatalf("RoleOf before demotion = (%q, %v, %v), want an admin", role, ok, err)
	}
	if role != registry.RoleAdmin {
		t.Fatalf("role before demotion = %q, want %q", role, registry.RoleAdmin)
	}

	accounts.setRole("ops", registry.RoleViewer)

	// 同一张会话，下一次判定必须已经是只读。
	role, ok, err = v.RoleOf(ctx, sess.Username)
	if err != nil || !ok {
		t.Fatalf("RoleOf after demotion = (%q, %v, %v), want a viewer", role, ok, err)
	}
	if role != registry.RoleViewer {
		t.Errorf("role after demotion = %q, want %q —— 降权必须在下一次请求就生效，"+
			"而不是等这张会话过期", role, registry.RoleViewer)
	}
}

// 停用与软删除都必须让该账号手里的会话立即失去权限，而且这件事要由
// 「现读账号记录」得出，不能依赖每条改权限的路径记得去撤销会话。
func TestDisablingAnAccountInvalidatesItsLiveSession(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		revoke func(f *fakeAccounts)
	}{
		{"disabled", func(f *fakeAccounts) { f.disable("ops") }},
		{"soft deleted", func(f *fakeAccounts) { f.softDelete("ops") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := newFakeAccounts(
				enabledAccount("ops", registry.RoleAdmin),
				enabledAccount("root", registry.RoleAdmin),
			)
			v := auth.NewVerifier(bootstrapUser(t), accounts)

			sessions := auth.NewSessionStore(8*time.Hour, nil)
			sess, err := sessions.Create("ops")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, ok, err := v.RoleOf(ctx, sess.Username); !ok || err != nil {
				t.Fatalf("RoleOf before %s = (%v, %v), want a usable role", tc.name, ok, err)
			}

			tc.revoke(accounts)

			// 会话本身还在存储里没过期 —— 失效必须来自账号记录，
			// 而不是来自「有人记得把它删掉」。
			if _, live := sessions.Get(sess.ID); !live {
				t.Fatal("the session must still be in the store: this test is about the " +
					"account record deciding, not about someone having revoked it")
			}
			role, ok, err := v.RoleOf(ctx, sess.Username)
			if err != nil {
				t.Fatalf("RoleOf after %s: %v", tc.name, err)
			}
			if ok {
				t.Errorf("RoleOf after %s = (%q, true), want refused —— "+
					"停用或删除之后那张会话必须立刻什么都做不了", tc.name, role)
			}
			if role.Valid() {
				t.Errorf("RoleOf after %s returned a usable role %q", tc.name, role)
			}
		})
	}
}

// 引导账号只在库里没有启用中的管理员时可登录：第一个库内管理员建出来之后，
// 它自己失效，不需要谁记得去关掉它（design doc 2026-08-14 §2）。
func TestBootstrapLoginRefusedOnceAnEnabledAdminExists(t *testing.T) {
	ctx := context.Background()
	accounts := newFakeAccounts()
	v := auth.NewVerifier(bootstrapUser(t), accounts)

	ok, err := v.Verify(ctx, "demo", "bootstrap-secret")
	if err != nil {
		t.Fatalf("Verify on an empty account table: %v", err)
	}
	if !ok {
		t.Fatal("the bootstrap account must be able to log in while the database holds " +
			"no enabled admin — otherwise the platform can never be set up")
	}

	accounts.accounts["root"] = enabledAccount("root", registry.RoleAdmin)

	ok, err = v.Verify(ctx, "demo", "bootstrap-secret")
	if err != nil {
		t.Fatalf("Verify once an admin exists: %v", err)
	}
	if ok {
		t.Error("the bootstrap account must stop working once an enabled admin exists " +
			"in the database — it is an escape hatch, not a standing credential")
	}

	// 同一道闸门必须也管住它已经签发的会话：引导账号的角色同样是现读的。
	role, ok, err := v.RoleOf(ctx, "demo")
	if err != nil {
		t.Fatalf("RoleOf for the bootstrap account: %v", err)
	}
	if ok {
		t.Errorf("RoleOf for the bootstrap account = %q once an admin exists, want refused",
			role)
	}
}

// 全部管理员被停用或删除之后，引导口必须自己回来：否则平台被永久锁死，
// 只能改数据库救（design doc 2026-08-14 §2）。
func TestBootstrapLoginReturnsWhenNoEnabledAdminRemains(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		remove func(f *fakeAccounts)
	}{
		{"the last admin is disabled", func(f *fakeAccounts) { f.disable("root") }},
		{"the last admin is soft deleted", func(f *fakeAccounts) { f.softDelete("root") }},
		{"the last admin is demoted", func(f *fakeAccounts) { f.setRole("root", registry.RoleViewer) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := newFakeAccounts(
				enabledAccount("root", registry.RoleAdmin),
				enabledAccount("reader", registry.RoleViewer),
			)
			v := auth.NewVerifier(bootstrapUser(t), accounts)

			ok, err := v.Verify(ctx, "demo", "bootstrap-secret")
			if err != nil {
				t.Fatalf("Verify while an admin exists: %v", err)
			}
			if ok {
				t.Fatal("precondition failed: the bootstrap account must be refused " +
					"while an enabled admin exists")
			}

			tc.remove(accounts)

			ok, err = v.Verify(ctx, "demo", "bootstrap-secret")
			if err != nil {
				t.Fatalf("Verify after %s: %v", tc.name, err)
			}
			if !ok {
				t.Errorf("the bootstrap account is still refused after %s — "+
					"the platform is locked out with no way back", tc.name)
			}
			role, ok, err := v.RoleOf(ctx, "demo")
			if err != nil {
				t.Fatalf("RoleOf after %s: %v", tc.name, err)
			}
			if !ok || role != registry.RoleAdmin {
				t.Errorf("RoleOf for the bootstrap account after %s = (%q, %v), want (%q, true)",
					tc.name, role, ok, registry.RoleAdmin)
			}
		})
	}
}

// 引导闸门只看**启用中的管理员**：只读账号再多也救不回一个没人能管理的平台。
func TestBootstrapGateIgnoresViewersAndDisabledAdmins(t *testing.T) {
	ctx := context.Background()
	disabledAdmin := enabledAccount("root", registry.RoleAdmin)
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	disabledAdmin.DisabledAt = &at

	accounts := newFakeAccounts(disabledAdmin, enabledAccount("reader", registry.RoleViewer))
	v := auth.NewVerifier(bootstrapUser(t), accounts)

	ok, err := v.Verify(ctx, "demo", "bootstrap-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("a disabled admin and a viewer leave nobody able to manage the platform: " +
			"the bootstrap account must still be usable")
	}
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	v := auth.NewVerifier(bootstrapUser(t), newFakeAccounts())

	if ok, err := v.Verify(ctx, "demo", "bootstrap-secret"); err != nil || !ok {
		t.Errorf("correct credentials must verify: ok = %v, err = %v", ok, err)
	}
	if ok, err := v.Verify(ctx, "demo", "wrong"); err != nil || ok {
		t.Errorf("wrong password must not verify: ok = %v, err = %v", ok, err)
	}
	if ok, err := v.Verify(ctx, "ghost", "bootstrap-secret"); err != nil || ok {
		t.Errorf("unknown user must not verify: ok = %v, err = %v", ok, err)
	}
}

// 未知用户与错误密码都必须走完哈希比对，否则响应耗时会泄露账号是否存在。
func TestVerifyUnknownUserStillHashes(t *testing.T) {
	ctx := context.Background()
	v := auth.NewVerifier(bootstrapUser(t), newFakeAccounts())

	// 行为断言：两种失败的返回值逐字段相同，调用方无法区分。
	ghostOK, ghostErr := v.Verify(ctx, "ghost", "x")
	wrongOK, wrongErr := v.Verify(ctx, "demo", "x")
	if ghostOK != wrongOK || !errors.Is(ghostErr, wrongErr) {
		t.Error("unknown-user and wrong-password must be indistinguishable to the caller")
	}
}

func TestVerifyRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	v := auth.NewVerifier(bootstrapUser(t), newFakeAccounts())
	if ok, err := v.Verify(ctx, "", ""); err != nil || ok {
		t.Errorf("empty credentials must not verify: ok = %v, err = %v", ok, err)
	}
}

// 没有配置引导账号时不存在引导身份：空用户名不得成为一条登录，更不得
// 解析出一个角色。这是一次装配失误应有的下场（规范 §49）。
func TestVerifierWithoutABootstrapAccountRefusesTheEmptyUsername(t *testing.T) {
	ctx := context.Background()
	v := auth.NewVerifier(config.User{}, newFakeAccounts())

	if ok, err := v.Verify(ctx, "", ""); err != nil || ok {
		t.Errorf("an unconfigured bootstrap account must not verify: ok = %v, err = %v", ok, err)
	}
	role, ok, err := v.RoleOf(ctx, "")
	if err != nil {
		t.Fatalf("RoleOf(\"\"): %v", err)
	}
	if ok {
		t.Errorf("RoleOf(\"\") = %q, want refused — an unconfigured bootstrap account "+
			"must not resolve to an administrator", role)
	}
}

// 读不到账号表时一律拒绝，且把错误交给调用方。
//
// 「读失败」不是「没有限制」：退一步放行会让一次数据库抖动变成一次
// 全平台的越权（规范 §49）。
func TestAccountReadFailureRefuses(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("account table unavailable")
	accounts := newFakeAccounts(enabledAccount("ops", registry.RoleAdmin))
	accounts.err = boom
	v := auth.NewVerifier(bootstrapUser(t), accounts)

	ok, err := v.Verify(ctx, "demo", "bootstrap-secret")
	if ok {
		t.Error("Verify must refuse when the account table cannot be read")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Verify err = %v, want it to carry %v", err, boom)
	}

	role, ok, err := v.RoleOf(ctx, "ops")
	if ok || role.Valid() {
		t.Errorf("RoleOf = (%q, %v), want refused when the account table cannot be read", role, ok)
	}
	if !errors.Is(err, boom) {
		t.Errorf("RoleOf err = %v, want it to carry %v", err, boom)
	}
}

// 库里的角色不在封闭枚举内时一律拒绝：迁移、手工 SQL 都绕得开写入时的
// 校验，而给一个认不出的角色兜底等于凭空发一次授权。
func TestRoleOfRefusesAnUnregisteredRole(t *testing.T) {
	ctx := context.Background()
	accounts := newFakeAccounts(
		enabledAccount("ops", registry.Role("SUPERADMIN")),
		enabledAccount("root", registry.RoleAdmin),
	)
	v := auth.NewVerifier(bootstrapUser(t), accounts)

	role, ok, err := v.RoleOf(ctx, "ops")
	if err != nil {
		t.Fatalf("RoleOf: %v", err)
	}
	if ok || role.Valid() {
		t.Errorf("RoleOf = (%q, %v), want refused for a role outside the enum", role, ok)
	}
}

// 库里的账号必须能登录。
//
// **这条线决定平台在第一次装好之后还能不能用**：引导账号建出第一个管理员
// 之后引导闸门就关上（design doc 2026-08-14 §2），若库内账号没有一条能拿到
// 自己哈希的路径，那个刚建出来的管理员登不进来，平台从此不可用。
func TestDatabaseAccountCanSignIn(t *testing.T) {
	ctx := context.Background()
	const password = "correct-horse-battery"
	accounts := newFakeAccounts(enabledAccount("ops", registry.RoleAdmin))
	accounts.setPassword("ops", hashOf(t, password))
	v := auth.NewVerifier(bootstrapUser(t), accounts)

	ok, err := v.Verify(ctx, "ops", password)
	if err != nil {
		t.Fatalf("Verify for a database account: %v", err)
	}
	if !ok {
		t.Fatal("a database account cannot sign in — once the bootstrap gate closes, " +
			"nobody can get into the platform at all")
	}

	// 错误密码必须被拒绝：一份恒为 true 的实现同样能让上面那行绿。
	if ok, err := v.Verify(ctx, "ops", "not-the-password"); err != nil || ok {
		t.Errorf("Verify with a wrong password = (%v, %v), want refused", ok, err)
	}

	// 引导闸门此刻是关着的（库里有一个启用中的管理员），库内账号照样登得
	// 进来：两条身份互不牵连。
	if ok, err := v.Verify(ctx, "demo", "bootstrap-secret"); err != nil || ok {
		t.Errorf("bootstrap Verify while an enabled admin exists = (%v, %v), want refused", ok, err)
	}
}

// 停用与软删除的账号一律登不进来。
//
// 停用期间与「没有这个账号」等价，这条处置 RoleOf 早就定了；把它同样落在
// 认证这一层，被停用的账号连会话都换不到，而不是换到一张什么都做不了的
// 会话（design doc 2026-08-14 §4）。
func TestDisabledOrSoftDeletedAccountCannotSignIn(t *testing.T) {
	ctx := context.Background()
	const password = "correct-horse-battery"

	for _, tc := range []struct {
		name   string
		revoke func(f *fakeAccounts)
	}{
		{"disabled", func(f *fakeAccounts) { f.disable("ops") }},
		{"soft deleted", func(f *fakeAccounts) { f.softDelete("ops") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := newFakeAccounts(
				enabledAccount("ops", registry.RoleAdmin),
				enabledAccount("root", registry.RoleAdmin),
			)
			accounts.setPassword("ops", hashOf(t, password))
			v := auth.NewVerifier(bootstrapUser(t), accounts)

			// 前提：撤销之前它确实登得进来，否则下面那条断言什么也没证明。
			if ok, err := v.Verify(ctx, "ops", password); err != nil || !ok {
				t.Fatalf("precondition: Verify before %s = (%v, %v), want accepted", tc.name, ok, err)
			}

			tc.revoke(accounts)

			ok, err := v.Verify(ctx, "ops", password)
			if err != nil {
				t.Fatalf("Verify after %s: %v", tc.name, err)
			}
			if ok {
				t.Errorf("Verify after %s = true, want refused —— "+
					"停用或删除的账号不该还能换到一张会话", tc.name)
			}
		})
	}
}

// 未知账号那条分支必须与「账号存在但密码错了」花掉同一次 bcrypt 计算。
//
// TestVerifyUnknownUserStillHashes 只断言两种失败的**返回值**认不出来，而
// 返回值相同的实现里有一份是「未知就立刻 return」——它泄漏的是耗时，不是
// 返回值，那条用例对它是绿的。这一条量的就是耗时。
//
// 哈希用 bcrypt.DefaultCost 而不是 hashOf 的 MinCost：MinCost 的一次比对是
// 几十微秒，与「直接返回」在调度噪声里分不开；DefaultCost 是几十毫秒，
// 一次短路会掉到它的千分之一以下。判据因此不依赖一个猜出来的绝对时长，
// 取三次里最快的一次也是同一个理由——噪声只会让耗时变长，取最小值就把
// 它挤掉了。dummyHash 本身也是 cost 10，两条分支的工作量因此可比。
func TestVerifyDoesNotShortCircuitForUnknownAccounts(t *testing.T) {
	ctx := context.Background()
	realCost, err := bcrypt.GenerateFromPassword([]byte("correct-horse-battery"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash at the default cost: %v", err)
	}
	accounts := newFakeAccounts(enabledAccount("ops", registry.RoleAdmin))
	accounts.setPassword("ops", string(realCost))
	v := auth.NewVerifier(bootstrapUser(t), accounts)

	fastest := func(username string) time.Duration {
		t.Helper()
		var best time.Duration
		for i := range 3 {
			start := time.Now()
			ok, err := v.Verify(ctx, username, "not-the-password")
			if err != nil || ok {
				t.Fatalf("Verify(%q) = (%v, %v), want a plain refusal", username, ok, err)
			}
			if d := time.Since(start); i == 0 || d < best {
				best = d
			}
		}
		return best
	}

	known := fastest("ops")
	unknown := fastest("ghost")
	// 四分之一是给调度噪声留的余量，不是给「少算一次哈希」留的：短路的
	// 实现会掉到千分之一量级，这条线拦得住它而不会被抖动误伤。
	if unknown*4 < known {
		t.Errorf("unknown account took %v but a wrong password took %v —— "+
			"未知账号那条分支没有走完哈希比对，登录接口成了一个用户名探针",
			unknown, known)
	}
}

// 库内账号的用户名不得与配置里的引导账号撞名（大小写不敏感）。
//
// 撞名会同时造成两件事：撞名的库内账号永远登不进来（Verify 先看引导
// 分支），以及引导闸门开着时它会被解析成 ADMIN 而不管库里写的是什么角色
// （RoleOf 同样先看引导分支）。后者是一次静默提权。
//
// 大小写不敏感是因为 platform_account 的排序规则是 utf8mb4_0900_ai_ci：
// 库把 "demo" 与 "DEMO" 当成同一个主键，而 isBootstrap 用的是 Go 的 ==。
func TestValidateNewUsernameRefusesTheBootstrapName(t *testing.T) {
	v := auth.NewVerifier(bootstrapUser(t), newFakeAccounts())

	for _, name := range []string{"demo", "DEMO", "Demo", "deMO"} {
		if err := v.ValidateNewUsername(name); !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("ValidateNewUsername(%q) = %v, want ErrInvalid —— "+
				"一个用户名不能同时是两套凭证", name, err)
		}
	}

	// 不撞名的用户名必须放行，否则一份恒为拒绝的实现也能让上面几行绿。
	// 空串在这里也放行：「用户名不能为空」是 ValidateAccount 的判定，
	// 本函数只回答撞不撞名这一个问题。
	for _, name := range []string{"demo2", "ademo", "de mo", "ops", ""} {
		if err := v.ValidateNewUsername(name); err != nil {
			t.Errorf("ValidateNewUsername(%q) = %v, want nil", name, err)
		}
	}

	// 没有配置引导账号时不存在引导身份，也就不存在撞名 —— 此时用空用户名
	// 去撞一个空的 bootstrapUser 必须放行，而不是拒绝一个与它无关的输入。
	none := auth.NewVerifier(config.User{}, newFakeAccounts())
	for _, name := range []string{"", "demo"} {
		if err := none.ValidateNewUsername(name); err != nil {
			t.Errorf("ValidateNewUsername(%q) with no bootstrap account = %v, want nil", name, err)
		}
	}
}

// 库里没有的账号解析不出角色：一张指向已不存在账号的会话什么都做不了。
func TestRoleOfRefusesAnUnknownAccount(t *testing.T) {
	ctx := context.Background()
	v := auth.NewVerifier(bootstrapUser(t), newFakeAccounts(
		enabledAccount("root", registry.RoleAdmin),
	))

	role, ok, err := v.RoleOf(ctx, "ghost")
	if err != nil {
		t.Fatalf("RoleOf: %v", err)
	}
	if ok || role.Valid() {
		t.Errorf("RoleOf = (%q, %v), want refused for an account that is not in the table",
			role, ok)
	}
}
