-- 让一次「还没开始采集就失败」的运行留下痕迹（design doc 2026-08-16 §4.2）。
--
-- 迁移 000009 建 collection_run 时漏了 spec §4.2 写明的 error_reason 一列。
-- 后果不是少一个字段：采集器在读集群**之前**就失败退出时（凭据解析不出来、
-- apiserver 地址被出站守卫拒绝、只读自证没通过），根本没有一行
-- collection_run 被写下 —— 于是界面显示「这个集群还没有过任何一次资产
-- 采集」，与一个刚注册、采集器压根没被拉起来过的集群一模一样。
-- 操作者会去等一次永远不会成功的采集。
--
-- 封闭枚举，不用自由文本（CLAUDE.md §3）。取值与 snapshot.RunErrorReason
-- 一一对应；新增原因要同步改那个枚举与可见面的白名单。
--
-- 空串表示这一轮真的读到了集群，成败看 status —— 不用 NULL：这一列的
-- 「没有原因」是一个确定的事实，而 NULL 在 SQL 里既表示「没有」也表示
-- 「不知道」，用它会让「采集正常开始」和「这行是旧数据、当时没记」
-- 变成同一个值。DEFAULT '' 让 000009 之前的历史行落进前者，这是对的：
-- 那些行确实都是采集真的跑起来之后写的。
ALTER TABLE collection_run
  ADD COLUMN error_reason VARCHAR(32) NOT NULL DEFAULT '' AFTER status;
