-- 回滚采集层轮 1 的资产快照表（design doc 2026-08-16 §4）。
--
-- 回滚会丢掉全部已采资产。这是可接受的：本轮的采集结果不进任何结论
-- 链路（design doc §5.2），拓扑、dry-run、候选策略、导出、写回全部仍走
-- fixture，因此丢掉这些行不会改变平台给出的任何判断。
-- 重新采一次即可恢复 —— 采集是幂等的全量拉取，不是增量流水。
--
-- 顺序按外键反向：先删引用方，再删被引用方。
DROP TABLE IF EXISTS observed_gateway;
DROP TABLE IF EXISTS observed_network_policy;
DROP TABLE IF EXISTS observed_endpoints;
DROP TABLE IF EXISTS observed_service;
DROP TABLE IF EXISTS observed_node;
DROP TABLE IF EXISTS observed_pod;
DROP TABLE IF EXISTS observed_namespace;
DROP TABLE IF EXISTS collection_warning;
DROP TABLE IF EXISTS collection_run_failure;
DROP TABLE IF EXISTS collection_run_resource;
DROP TABLE IF EXISTS collection_run;
