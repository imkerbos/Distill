-- 回滚 collection_run.error_reason。
--
-- 回滚会丢掉「这一轮为什么根本没开始」这个信息，且丢掉之后那些行会变成
-- 一批没有任何资源计数、也没有任何失败记录的 FAILED 运行 —— 读起来像
-- 一次采到零个资源的成功运行。这是这次回滚的已知代价，不是遗漏：
-- 要恢复只能重新采一次。
ALTER TABLE collection_run DROP COLUMN error_reason;
