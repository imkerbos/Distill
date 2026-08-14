-- 没有任何表引用 platform_setting，直接 DROP 即可。
--
-- 回滚会一并丢掉后台改过的设置：回滚之后生效的是配置文件形态，
-- 那正是这次迁移之前的样子。审计行留在 audit_log 里，改过什么仍可追。
DROP TABLE IF EXISTS platform_setting;
