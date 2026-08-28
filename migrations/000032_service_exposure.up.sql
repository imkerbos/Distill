-- Service 的对外暴露事实：入口地址与允许来源网段。
--
-- 入口地址是判定「这个入口面向公网还是只在 VPC 内」的依据，取代读云厂商
-- 注解那条路（design doc 2026-08-28 §2）。两列都是 JSON 数组：一个 Service
-- 可以有多个入口地址（双栈、多可用区）。
--
-- 允许 NULL 而不是给 DEFAULT '[]'：迁移之前采集的那些行确实**没有采过**
-- 这两个字段，而空数组的含义是「采过，是空的」。两者混在一起，推导层
-- 会把一批老快照当成「这个 LB 没有入口地址」而报缺口。
ALTER TABLE observed_service
  ADD COLUMN lb_ingress_ips   JSON NULL,
  ADD COLUMN lb_source_ranges JSON NULL;

-- Pod 的命名容器端口，供命名端口求值使用。
-- 同样允许 NULL，理由同上。
ALTER TABLE observed_pod
  ADD COLUMN named_ports JSON NULL;
