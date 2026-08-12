-- 绑定校验结论与其发生时间；两者独立于绑定能否保存（design doc §3.2）。
ALTER TABLE cluster_git_binding
  ADD COLUMN verified_at   DATETIME(6) NULL,
  ADD COLUMN verify_result VARCHAR(32) NOT NULL DEFAULT 'NOT_VERIFIED';
