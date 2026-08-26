-- 由平台管理的系统命名空间（默认为空 = 平台不碰）。
--
-- 本文件是新增的第 28 号迁移，不改动 000001–000027 中任何一条已应用的迁移。
--
-- **为什么默认不碰**：候选集是给每个 workload 装上 default-deny、再把观测到的
-- 连接一条条放回去。而观测窗口证明不了完整时，学出来的规则默认不启用 ——
-- 于是 kube-dns 会拿到一份"只放行 Baseline"的 default-deny ingress，
-- 全集群的 DNS 解析随之中断。
--
-- 这不是假设。真集群实测（2026-08-26）：kube-system/kube-dns 的候选里，
-- 各 namespace 到 UDP/53 的规则全部 enabled=false，dry-run 报出 14 条 DNS
-- 会被拦断。dry-run 确实报了 —— 但 DNS 断与一条业务连接断在计数里是平等的
-- 21 条，屏幕上没有任何东西说"这 14 条会让整个集群失去 DNS"。
--
-- **纳入必须带理由**（NOT NULL，空串即未声明）：这是一次会改变爆炸半径的
-- 决定，而没有理由的决定在事后复盘时与"手滑填上去的"分不开 ——
-- 与 no_node_agents_reason、business_cycle_reason 同一形状。
ALTER TABLE cluster
  ADD COLUMN managed_system_namespaces JSON NOT NULL DEFAULT (JSON_ARRAY()),
  ADD COLUMN managed_system_namespaces_reason VARCHAR(512) NOT NULL DEFAULT '';

-- 历史行显式置成空数组，理由同 000027：不写的话已有行拿到的是 JSON 的
-- null 字面量，那一列从此有两种形状。空数组在这里的含义是明确的 ——
-- 没有人声明过要平台管系统命名空间，那就是默认的"不碰"。
UPDATE cluster SET managed_system_namespaces = JSON_ARRAY()
 WHERE JSON_TYPE(managed_system_namespaces) = 'NULL';
