package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// settingTarget 是设置审计行的 target（design doc 2026-08-13 §1.3）。
//
// 不带 id 后缀：设置只有一行，target 里再拼一个恒为 1 的编号，
// 只会让追溯的人以为还存在别的设置对象。
const settingTarget = "platform-setting"

// settingClusterID 是设置审计行的 cluster_id。
//
// 设置不属于任何集群，而 audit_log.cluster_id 是 NOT NULL、且刻意没有外键
// （见 000001_init）。空串是唯一撞不上真实集群的取值 —— ValidateCluster
// 要求集群 ID 非空，因此没有任何集群能占用它；换成 PLATFORM 之类的字面量
// 就有可能与一个真叫这个名字的集群重名，届时它的审计与平台设置的审计
// 会在 idx_cluster_time 上混成一份读不开的记录。
const settingClusterID = ""

// settingAction 是设置变更的审计动作。
//
// host key 是平台连接策略仓库时的信任锚，而它能从后台改（design doc §1.3）——
// 这条审计行是事后唯一能回答「信任锚是谁在什么时候换的」的东西，
// 因此前后值必须完整落库，不做裁剪。
const settingAction = "UPDATE_PLATFORM_SETTING"

// settingRow 是 PlatformSetting 的列形态：Duration 换成列上的整数。
//
// 单独一个类型而不是在 UpdateSetting 里摊开一串 int64：转换会失败
// （见 durationIn），失败要能在写事务开始前就返回。
type settingRow struct {
	sessionTTLSeconds  int64
	readTimeoutMS      int64
	writeTimeoutMS     int64
	shutdownTimeoutMS  int64
	gitVerifyTimeoutMS int64
}

// durationIn 把 Duration 换成以 unit 为单位的整数，除不尽时报错。
//
// 列是整数（design doc §2），而整数除法的截断是静默的：500ms 的会话 TTL
// 落成 0 秒，读回来就是「会话立即过期」，且没有任何地方会说这是保存时
// 丢掉的精度。宁可在操作者按下保存的那一刻就拒绝。
func durationIn(field string, d, unit time.Duration) (int64, error) {
	if d%unit != 0 {
		return 0, registry.NewInvalidError(fmt.Sprintf(
			"%s 必须是 %s 的整数倍，否则落库时会被静默截断", field, unit))
	}
	return int64(d / unit), nil
}

// toSettingRow 把一份设置换算成列值。
func toSettingRow(s registry.PlatformSetting) (settingRow, error) {
	var r settingRow
	var err error
	// 会话 TTL 的列是秒，其余是毫秒（design doc §2 的表定义）。
	if r.sessionTTLSeconds, err = durationIn("sessionTtl", s.SessionTTL, time.Second); err != nil {
		return settingRow{}, err
	}
	if r.readTimeoutMS, err = durationIn("httpReadTimeout", s.HTTPReadTimeout, time.Millisecond); err != nil {
		return settingRow{}, err
	}
	if r.writeTimeoutMS, err = durationIn("httpWriteTimeout", s.HTTPWriteTimeout, time.Millisecond); err != nil {
		return settingRow{}, err
	}
	if r.shutdownTimeoutMS, err = durationIn("httpShutdownTimeout", s.HTTPShutdownTimeout, time.Millisecond); err != nil {
		return settingRow{}, err
	}
	if r.gitVerifyTimeoutMS, err = durationIn("gitVerifyTimeout", s.GitVerifyTimeout, time.Millisecond); err != nil {
		return settingRow{}, err
	}
	return r, nil
}

// Setting 读当前的平台设置。
//
// 每次调用都查库，调用方不得把结果缓存成启动快照（design doc §1.1）：
// 轮 2 已经在这件事上出过一次事故。单行主键命中，代价可忽略。
func (s *Store) Setting(ctx context.Context) (registry.PlatformSetting, error) {
	var (
		out     registry.PlatformSetting
		row     settingRow
		backend string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT session_ttl_seconds, http_read_timeout_ms, http_write_timeout_ms,
		        http_shutdown_timeout_ms, secrets_backend, secrets_project,
		        secrets_prefix, secrets_dir, gitverify_timeout_ms, gitverify_host_keys,
		        git_allowed_destinations
		   FROM platform_setting WHERE id = 1`).
		Scan(&row.sessionTTLSeconds, &row.readTimeoutMS, &row.writeTimeoutMS,
			&row.shutdownTimeoutMS, &backend, &out.SecretsProject, &out.SecretsPrefix,
			&out.SecretsDir, &row.gitVerifyTimeoutMS, &out.GitVerifyHostKeys,
			&out.GitAllowedDestinations)
	if errors.Is(err, sql.ErrNoRows) {
		// 空表不等于「用默认值」：零值超时是关掉超时保护，零值 TTL 是
		// 会话立即过期。迁移种下了这一行，读不到它说明库的状态不对，
		// 让调用方看见这件事，而不是拿一份编出来的设置继续跑。
		return registry.PlatformSetting{}, fmt.Errorf("%w: platform setting", ErrNotFound)
	}
	if err != nil {
		return registry.PlatformSetting{}, fmt.Errorf("query platform setting: %w", err)
	}
	out.SessionTTL = time.Duration(row.sessionTTLSeconds) * time.Second
	out.HTTPReadTimeout = time.Duration(row.readTimeoutMS) * time.Millisecond
	out.HTTPWriteTimeout = time.Duration(row.writeTimeoutMS) * time.Millisecond
	out.HTTPShutdownTimeout = time.Duration(row.shutdownTimeoutMS) * time.Millisecond
	out.GitVerifyTimeout = time.Duration(row.gitVerifyTimeoutMS) * time.Millisecond
	out.SecretsBackend = registry.SecretsBackend(backend)
	return out, nil
}

// UpdateSetting 整体替换平台设置，同事务写审计。
//
// 整体替换而非部分更新：设置页保存的就是完整的一份，而部分更新会让
// 审计的前后值只描述「这次提交带上了哪几个字段」，追溯时读不出全貌。
//
// 前值在事务外读取（同 BindGitRepo）：并发编辑下的最后写入者覆盖是已知
// 的空缺，修法是版本列，不在本文范围（design doc §7）。
func (s *Store) UpdateSetting(
	ctx context.Context, actor registry.Actor, ps registry.PlatformSetting,
) error {
	if err := registry.ValidatePlatformSetting(ps); err != nil {
		return err
	}
	row, err := toSettingRow(ps)
	if err != nil {
		return err
	}
	before, err := s.Setting(ctx)
	if err != nil {
		return err
	}
	// 变更本身的约束在这里判，而不是在边界层：边界层是**一个**调用方，
	// 而「信任锚不得被清空」这件事必须对每一个调用方成立（规范 §34）。
	//
	// 判定用的前值与审计的前值是同一个、同样取自事务外，因此并发编辑下
	// 它与其余字段落在同一条已知空缺里（最后写入者覆盖，design doc §7），
	// 不是这个字段特有的新问题。
	if err := registry.ValidateSettingUpdate(before, ps); err != nil {
		return err
	}
	return s.mutate(ctx, actor, settingClusterID, settingAction, settingTarget,
		before, ps, func(tx *sql.Tx) error {
			// 存在性由事务内的加锁读断言，不由 RowsAffected 断言：运行时
			// 连接池没开 clientFoundRows，把同一份设置原样再保存一次时
			// MySQL 报的受影响行数是 0，与「行不存在」无法区分（同
			// SetGitVerifyResult）。加锁同时让这一行在本事务提交前不能
			// 被并发改写。
			var exists int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM platform_setting WHERE id = 1 FOR UPDATE`).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: platform setting", ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("lock platform setting: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE platform_setting
				    SET session_ttl_seconds = ?, http_read_timeout_ms = ?,
				        http_write_timeout_ms = ?, http_shutdown_timeout_ms = ?,
				        secrets_backend = ?, secrets_project = ?, secrets_prefix = ?,
				        secrets_dir = ?, gitverify_timeout_ms = ?,
				        gitverify_host_keys = ?, git_allowed_destinations = ?,
				        updated_at = ?
				  WHERE id = 1`,
				row.sessionTTLSeconds, row.readTimeoutMS, row.writeTimeoutMS,
				row.shutdownTimeoutMS, string(ps.SecretsBackend), ps.SecretsProject,
				ps.SecretsPrefix, ps.SecretsDir, row.gitVerifyTimeoutMS,
				ps.GitVerifyHostKeys, ps.GitAllowedDestinations, s.now(),
			); err != nil {
				return fmt.Errorf("update platform setting: %w", err)
			}
			return nil
		})
}
