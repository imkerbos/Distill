-- 运行期配置从配置文件迁进库里（design doc 2026-08-13 §1、§2）。
--
-- 单行表、一列一项，不是 key/value：key/value 让「有哪些设置」变成运行期
-- 才知道的事，拼错的键静默变成一个新设置。一列一项把这件事推到迁移期。
--
-- 单行由 CHECK 保证，不靠代码自觉：多出第二行时「当前设置是哪一行」
-- 没有答案，而并发插入正是写出第二行的那条路径。
CREATE TABLE platform_setting (
  id                       TINYINT      NOT NULL DEFAULT 1,
  session_ttl_seconds      INT          NOT NULL,
  http_read_timeout_ms     INT          NOT NULL,
  http_write_timeout_ms    INT          NOT NULL,
  http_shutdown_timeout_ms INT          NOT NULL,
  secrets_backend          VARCHAR(32)  NOT NULL,   -- NONE | DIR | SECRET_MANAGER
  secrets_project          VARCHAR(128) NOT NULL DEFAULT '',
  secrets_prefix           VARCHAR(128) NOT NULL DEFAULT '',
  secrets_dir              VARCHAR(512) NOT NULL DEFAULT '',
  gitverify_timeout_ms     INT          NOT NULL,
  gitverify_host_keys      TEXT         NOT NULL,
  updated_at               DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  CONSTRAINT ck_platform_setting_single_row CHECK (id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 种子行：取值与 configs/demo.yaml 当前生效值一致，升级前后行为不变。
--
-- 表不能留空：设置按需读取，第一次读就没有配置可用，而这些字段的零值
-- 不是「用默认」而是「关掉超时保护、会话立即过期」。
--
-- gitverify 段在 demo 里整段注释着，生效值来自 config.applyDefaults 的
-- 10s；host keys 为空同样是 demo 的现状 —— 不解析凭据也就不做 Git 校验。
INSERT INTO platform_setting
  (id, session_ttl_seconds, http_read_timeout_ms, http_write_timeout_ms,
   http_shutdown_timeout_ms, secrets_backend, secrets_project, secrets_prefix,
   secrets_dir, gitverify_timeout_ms, gitverify_host_keys, updated_at)
VALUES
  (1, 28800, 10000, 20000, 15000, 'NONE', '', '', '', 10000, '', UTC_TIMESTAMP(6));
