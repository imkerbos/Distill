-- 回滚身份区间表。
--
-- 顺序与 up 相反：先撤索引，再删表。
--
-- 丢掉的是推导产物，不是事实来源 —— observed_pod 原样还在，重新迁上来之后
-- 按 spec §6 逐次运行重推即可。这是这张表"由观测推导、不由采集器直写"
-- 带来的一个直接好处：它可以被丢弃并重建，而快照不能。
ALTER TABLE observed_pod
  DROP KEY idx_pod_run;

DROP TABLE pod_identity_interval;
