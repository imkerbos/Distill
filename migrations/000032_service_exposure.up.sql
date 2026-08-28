-- Service 的对外暴露事实：入口地址与允许来源网段。
--
-- 入口地址是判定「这个入口面向公网还是只在 VPC 内」的依据，取代读云厂商
-- 注解那条路（design doc 2026-08-28 §2）。两列都是 JSON 数组：一个 Service
-- 可以有多个入口地址（双栈、多可用区）。
--
-- 允许 NULL 而不是给 DEFAULT '[]'：迁移之前采集的那些行确实**没有采过**
-- 这两个字段，而空数组的含义是「采过，是空的」。
--
-- **今天这两种情形的下游结论相同**：推导层判的是 len(...) == 0，两者都报
-- 成缺口，没有任何消费方分得开它们。留住这个区分是为了它**不可逆** ——
-- 迁移时把老行填成 '[]' 是一次销毁，事后再也回答不了「这一行到底采没采过」；
-- 而反过来，将来要区分（比如把老快照的缺口标成 DEGRADED 而不是缺失）
-- 随时做得到。列的语义要能承载还没写的那个判断，不能只承载今天这个。
ALTER TABLE observed_service
  ADD COLUMN lb_ingress_ips   JSON NULL,
  ADD COLUMN lb_source_ranges JSON NULL;

-- Pod 的命名容器端口，供命名端口求值使用。
-- 同样允许 NULL，理由同上：区分不可逆，今天的下游结论则不区分。
ALTER TABLE observed_pod
  ADD COLUMN named_ports JSON NULL;
