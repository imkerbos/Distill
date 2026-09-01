-- 回滚后写回退回复用 gitverify_timeout_ms，也就是这次变更之前的行为。
ALTER TABLE platform_setting
  DROP COLUMN gitwrite_timeout_ms;
