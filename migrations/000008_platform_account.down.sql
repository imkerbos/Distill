-- 没有任何表引用 platform_account，直接 DROP 即可。
--
-- 回滚会一并丢掉后台建过的账号：回滚之后生效的是配置文件里的引导账号，
-- 那正是这次迁移之前的样子（design doc 2026-08-14 §2）。审计行留在
-- audit_log 里，谁建过、停用过、删过哪个账号仍然追得到。
DROP TABLE IF EXISTS platform_account;
