-- 这个集群看全一轮流量需要多久，以及凭什么这么定（design doc 2026-08-25 §5）。
--
-- 本文件是新增的第 23 号迁移，不改动 000001–000022 中任何一条已应用的迁移。
--
-- **为什么要有这两列**：候选策略只学观测窗口内见到的流量。月结批处理、
-- 季度对账、只在故障时走的灾备链路，不在窗口里就不会有规则 —— 而 dry-run
-- 也看不出来，因为它只能评估自己见过的连接。平台观测不出「多久算一轮」，
-- 那要靠知道业务的人说出来。
--
-- **默认 0 表示还没有人回答过这个问题**，写回门禁据此拒绝出计划。默认放行
-- 会让这道门禁在最需要它的集群上不存在 —— 那些恰恰是没人想过这个问题的集群。
--
-- 存秒数而不是 MySQL 的时间间隔类型：Go 侧是 time.Duration，两边都以整数
-- 秒表达，转换只有一处；用 INTERVAL 会让"这一列到底是什么单位"多一个答案。
ALTER TABLE cluster
  ADD COLUMN business_cycle_seconds INT UNSIGNED NOT NULL DEFAULT 0 AFTER other_planes,
  ADD COLUMN business_cycle_reason  VARCHAR(512) NOT NULL DEFAULT '' AFTER business_cycle_seconds;
