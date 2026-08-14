package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/imkerbos/Distill/internal/registry"
)

// accountTarget 是账号审计行的 target（design doc 2026-08-14 §6）。
//
// 与 git-repo/<id>、platform-setting 同一形状：追溯的人从 target 一眼看出
// 这条记录动的是哪个对象，不必先去前后值里认。
func accountTarget(username string) string {
	return "account/" + username
}

// accountClusterID 是账号审计行的 cluster_id。
//
// 账号不属于任何集群，理由与取值同 settingClusterID —— 那里写了为什么
// 只有空串撞不上真实集群，这里不再重复一遍。
const accountClusterID = ""

// 账号相关的审计动作（design doc 2026-08-14 §6）。
//
// 写成常量而不是散在各方法里的字面量：这几个取值同时是统计口径，
// 拼错一个字母只会让某一类操作从统计里静默消失。
const (
	actionCreateAccount     = "CREATE_ACCOUNT"
	actionUpdateAccountRole = "UPDATE_ACCOUNT_ROLE"
	actionDisableAccount    = "DISABLE_ACCOUNT"
	actionEnableAccount     = "ENABLE_ACCOUNT"
	actionDeleteAccount     = "DELETE_ACCOUNT"
	actionResetPassword     = "RESET_PASSWORD"
	actionChangeOwnPassword = "CHANGE_OWN_PASSWORD"
)

// accountColumns 是账号读路径的列清单。
//
// **没有 password_hash，这是刻意的执行点。** registry.Account 里没有这个
// 字段（见该类型的注释），而这一行是它在存储层对应的那一半：读路径根本
// 不把哈希取出连接，因此"某条返回路径忘了剔除哈希"不是一个需要每次
// review 去发现的疏漏，而是一件表达不出来的事（规范 §19、§20）。
const accountColumns = `username, role, disabled_at, created_at, updated_at`

// passwordChangeRecord 是改密审计行的前后值。
//
// 只记「密码变过」这个事实，不记哈希、更不记明文（design doc §6，规范
// §19、§21、§43）：审计要回答的是"谁在什么时候改了谁的密码"，而哈希落进
// 审计等于把一份口令的离线爆破材料复制进一张为了长期留存、且比账号表
// 更容易被导出查看的表里。
type passwordChangeRecord struct {
	PasswordChanged bool `json:"passwordChanged"`
}

// scanAccount 把一行 platform_account 读进 registry.Account。
//
// 由 Accounts 与 Account 共用：两处的列清单必须一致，抄两份的那一天
// 只会是其中一处新增了列而另一处没有。
func scanAccount(scan func(...any) error) (registry.Account, error) {
	var (
		a          registry.Account
		role       string
		disabledAt sql.NullTime
	)
	if err := scan(&a.Username, &role, &disabledAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return registry.Account{}, err
	}
	a.Role = registry.Role(role)
	// NULL 必须落成 nil，不能落成零值 time.Time：零值是 1970 年的一个真实
	// 时间点，任何「停用了吗」的判断都会把它当成"已停用"。
	if disabledAt.Valid {
		t := disabledAt.Time
		a.DisabledAt = &t
	}
	return a, nil
}

// Accounts 返回全部未删除的账号，按用户名升序。
func (s *Store) Accounts(ctx context.Context) ([]registry.Account, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accountColumns+`
		   FROM platform_account WHERE deleted_at IS NULL ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []registry.Account
	for rows.Next() {
		a, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return out, nil
}

// Account 按用户名查一个未删除的账号。
func (s *Store) Account(ctx context.Context, username string) (registry.Account, bool, error) {
	a, err := scanAccount(s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+`
		   FROM platform_account WHERE username = ? AND deleted_at IS NULL`, username).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.Account{}, false, nil
	}
	if err != nil {
		return registry.Account{}, false, fmt.Errorf("query account: %w", err)
	}
	return a, true, nil
}

// requireAdminRemains 在事务内断言：这次操作做完之后，库里仍然有至少
// 一个启用中的管理员（design doc 2026-08-14 §5）。
//
// **判定必须在事务内、对候选行加锁完成，不能先查后改**（规范 §25）。
// 两个管理员同时把对方降级，先查后改会双双读到"还有两个"、双双通过，
// 平台落地成零个管理员 —— 没有人能把它改回来，恢复手段只剩手改数据库。
// FOR UPDATE 把这段窗口关掉：第二个事务必须等第一个提交后才拿得到锁，
// 而它拿到锁之后这条 SELECT 是当前读，看见的是第一个事务写完的最新状态，
// 因此它会发现自己动的是最后一个管理员。
//
// 只由"会让 username 不再是启用中管理员"的那几条路径调用（停用、软删除、
// 降级）；提权与启用不调用，它们只会让管理员变多。
//
// 全表扫描的加锁读会锁住整张 platform_account：账号是个位数量级的表
// （design doc §10），把并发账号写入串行化的代价可以忽略，而按 role 建
// 索引再加锁反而要多考虑间隙锁的边界。锁的次序在所有守卫路径上一致
// （先管理员集合、再目标行），因此两条并发写路径不会互相死锁。
func requireAdminRemains(ctx context.Context, tx *sql.Tx, username string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT username FROM platform_account
		  WHERE role = ? AND disabled_at IS NULL AND deleted_at IS NULL
		  ORDER BY username FOR UPDATE`, string(registry.RoleAdmin))
	if err != nil {
		return fmt.Errorf("lock enabled admins: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var admins []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return fmt.Errorf("scan enabled admin: %w", err)
		}
		admins = append(admins, u)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate enabled admins: %w", err)
	}
	// 目标不在集合里（它是只读账号，或已经停用/删除）时这次操作动不到
	// 最后一个管理员，放行。
	if len(admins) == 1 && admins[0] == username {
		return fmt.Errorf("%w: %s 是最后一个启用中的管理员", registry.ErrLastAdmin, username)
	}
	return nil
}

// requireLiveAccount 在事务内加锁读一个未删除的账号，不存在时返回 ErrNotFound。
//
// 与 requireLiveRepo 同一个理由：存在性不由 RowsAffected 断言 —— 运行时
// 连接池没开 clientFoundRows，把同一个值原样再写一次时受影响行数是 0，
// 与「行已被并发删除」无法区分。加锁同时让这一行在本事务提交前不能被
// 并发改成已删除。
//
// 调用次序恒在 requireAdminRemains 之后：两把锁的获取次序在每条写路径上
// 一致，两条并发写路径因此不会互相死锁。
func requireLiveAccount(ctx context.Context, tx *sql.Tx, username string) error {
	var alive int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM platform_account WHERE username = ? AND deleted_at IS NULL FOR UPDATE`,
		username).Scan(&alive)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: account %s", ErrNotFound, username)
	}
	if err != nil {
		return fmt.Errorf("lock account: %w", err)
	}
	return nil
}

// CreateAccount 新建一个账号，同事务写审计。
//
// passwordHash 单独传入、不挂在 registry.Account 上：调用方在到达这里
// 之前已经完成明文到哈希的转换，本方法只管落库。审计的 after 值就是那个
// 读模型，因此它在类型上就不可能带出哈希。
func (s *Store) CreateAccount(
	ctx context.Context, actor registry.Actor, a registry.Account, passwordHash string,
) error {
	if err := registry.ValidateAccount(a); err != nil {
		return err
	}
	// 空哈希落库会得到一条谁也登不进去的账号，而它在列表里看起来一切正常。
	// 更要紧的是它给了下一个人一个"哈希为空就跳过校验"的诱惑，那一行会
	// 是一个静默的登录绕过（规范 §28）。这里直接拒绝。
	if passwordHash == "" {
		return registry.NewInvalidError("密码哈希不能为空")
	}
	now := s.now()
	// 写入什么就审计什么：时间戳由存储层生成，调用方给的零值不该出现在
	// 审计里 —— 那会让追溯的人看到一个 1970 年创建的账号。
	created := a
	created.DisabledAt = nil
	created.CreatedAt = now
	created.UpdatedAt = now
	return s.mutate(ctx, actor, accountClusterID, actionCreateAccount, accountTarget(a.Username),
		nil, created, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO platform_account
				   (username, password_hash, role, disabled_at, created_at, updated_at)
				 VALUES (?, ?, ?, NULL, ?, ?)`,
				created.Username, passwordHash, string(created.Role), now, now,
			); err != nil {
				// 软删除留着行，因此一个删掉的用户名不能被重新登记 —— 用户名
				// 不复用是设计的一部分（design doc §3）：复用会让新账号继承旧
				// 账号在审计里的身份。文案说出这件事，否则操作者会以为看花了眼。
				return writeFailure("insert platform account",
					fmt.Sprintf("用户名 %q 已被占用（含已删除的），请换一个", created.Username),
					"", err)
			}
			return nil
		})
}

// UpdateAccountRole 修改一个账号的角色，同事务写审计。
// 账号不存在时返回 ErrNotFound；降级最后一个启用中的管理员时返回 ErrLastAdmin。
func (s *Store) UpdateAccountRole(
	ctx context.Context, actor registry.Actor, username string, role registry.Role,
) error {
	if !role.Valid() {
		// 封闭枚举：自由文本落进库里，权限判定当场失去意义 —— 一个不在
		// 枚举里的角色在 Permits 下什么都做不了，而操作者以为自己改成了
		// 某个角色。
		return registry.NewInvalidError(
			fmt.Sprintf("角色 %q 不在已登记的取值范围内", role))
	}
	before, ok, err := s.Account(ctx, username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: account %s", ErrNotFound, username)
	}
	after := before
	after.Role = role
	after.UpdatedAt = s.now()
	return s.mutate(ctx, actor, accountClusterID, actionUpdateAccountRole, accountTarget(username),
		before, after, func(tx *sql.Tx) error {
			// 只有改成非管理员才可能减少管理员。改成 ADMIN 不必判定，
			// 否则把最后一个管理员的角色原样再设一次都会被拒绝。
			if role != registry.RoleAdmin {
				if err := requireAdminRemains(ctx, tx, username); err != nil {
					return err
				}
			}
			if err := requireLiveAccount(ctx, tx, username); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE platform_account SET role = ?, updated_at = ? WHERE username = ?`,
				string(role), after.UpdatedAt, username,
			); err != nil {
				return fmt.Errorf("update account role: %w", err)
			}
			return nil
		})
}

// DisableAccount 停用一个账号，同事务写审计。
// 账号不存在时返回 ErrNotFound；停用最后一个启用中的管理员时返回 ErrLastAdmin。
func (s *Store) DisableAccount(ctx context.Context, actor registry.Actor, username string) error {
	before, ok, err := s.Account(ctx, username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: account %s", ErrNotFound, username)
	}
	at := s.now()
	after := before
	after.DisabledAt = &at
	after.UpdatedAt = at
	return s.mutate(ctx, actor, accountClusterID, actionDisableAccount, accountTarget(username),
		before, after, func(tx *sql.Tx) error {
			if err := requireAdminRemains(ctx, tx, username); err != nil {
				return err
			}
			if err := requireLiveAccount(ctx, tx, username); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE platform_account SET disabled_at = ?, updated_at = ? WHERE username = ?`,
				at, at, username,
			); err != nil {
				return fmt.Errorf("disable account: %w", err)
			}
			return nil
		})
}

// EnableAccount 重新启用一个账号，同事务写审计。
// 账号不存在时返回 ErrNotFound。
//
// 不做最后管理员判定：启用只会让启用中的管理员变多，不会变少。
func (s *Store) EnableAccount(ctx context.Context, actor registry.Actor, username string) error {
	before, ok, err := s.Account(ctx, username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: account %s", ErrNotFound, username)
	}
	after := before
	after.DisabledAt = nil
	after.UpdatedAt = s.now()
	return s.mutate(ctx, actor, accountClusterID, actionEnableAccount, accountTarget(username),
		before, after, func(tx *sql.Tx) error {
			if err := requireLiveAccount(ctx, tx, username); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE platform_account SET disabled_at = NULL, updated_at = ? WHERE username = ?`,
				after.UpdatedAt, username,
			); err != nil {
				return fmt.Errorf("enable account: %w", err)
			}
			return nil
		})
}

// SoftDeleteAccount 软删除一个账号，同事务写审计。
// 账号不存在时返回 ErrNotFound；删除最后一个启用中的管理员时返回 ErrLastAdmin。
//
// 软删除而非物理删除：审计行按用户名指向这个账号，物理删除之后那些行
// 会指向一个查不到的人；用户名也因此不被复用（design doc §3）。
func (s *Store) SoftDeleteAccount(ctx context.Context, actor registry.Actor, username string) error {
	before, ok, err := s.Account(ctx, username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: account %s", ErrNotFound, username)
	}
	at := s.now()
	return s.mutate(ctx, actor, accountClusterID, actionDeleteAccount, accountTarget(username),
		before, nil, func(tx *sql.Tx) error {
			if err := requireAdminRemains(ctx, tx, username); err != nil {
				return err
			}
			if err := requireLiveAccount(ctx, tx, username); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE platform_account SET deleted_at = ?, updated_at = ? WHERE username = ?`,
				at, at, username,
			); err != nil {
				return fmt.Errorf("soft delete account: %w", err)
			}
			return nil
		})
}

// SetAccountPassword 写入一个账号的新密码哈希，同事务写审计。
// 账号不存在时返回 ErrNotFound。
//
// 管理员重置他人密码与本人改自己的密码走同一条写路径（design doc §6），
// 区别只在审计动作：actor 与目标是同一个人时记 CHANGE_OWN_PASSWORD，否则
// 记 RESET_PASSWORD。这个区分只能从服务端已知的两个值推出来 —— actor 来自
// 会话，用户名来自路由，都不是请求体里可以自称的东西（规范 §7）；让调用方
// 传一个"这是不是自助改密"的标志，等于让被审计的一方决定自己那条审计行
// 怎么写。是否需要先校验当前密码是调用方的职责（§28），不在本方法。
func (s *Store) SetAccountPassword(
	ctx context.Context, actor registry.Actor, username string, passwordHash string,
) error {
	if passwordHash == "" {
		return registry.NewInvalidError("密码哈希不能为空")
	}
	_, ok, err := s.Account(ctx, username)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: account %s", ErrNotFound, username)
	}
	action := actionResetPassword
	if actor.Username == username {
		action = actionChangeOwnPassword
	}
	// 前值为 NULL、后值只有一个布尔：审计记录密码变过，不记录变成了什么，
	// 也不记录变之前是什么（规范 §19、§21、§43）。
	return s.mutate(ctx, actor, accountClusterID, action, accountTarget(username),
		nil, passwordChangeRecord{PasswordChanged: true}, func(tx *sql.Tx) error {
			if err := requireLiveAccount(ctx, tx, username); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE platform_account SET password_hash = ?, updated_at = ? WHERE username = ?`,
				passwordHash, s.now(), username,
			); err != nil {
				// 错误文案里不出现哈希：写失败的错误会一路进日志。
				return fmt.Errorf("set account password: %w", err)
			}
			return nil
		})
}
