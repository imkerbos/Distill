-- 操作者声明的、这个集群的 CNI **真的会执行**的第二策略平面
-- （design doc 2026-08-25-existing-policies §2.2）。
--
-- 本文件是新增的第 30 号迁移，不改动 000001–000029 中任何一条已应用的迁移。
--
-- **与 other_planes 分开，回答的是两个问题**：那一列是探测结果
-- （"集群里有没有这类对象"），这一列是事实声明（"它们是不是活的"）。
-- 探测回答不了后者 —— 实测（2026-08-26）原生 Calico v3.30.4 执行
-- AdminNetworkPolicy，而 Cilium 1.19.5 完全不实现它，那种集群上的 ANP
-- 对象是死的。
--
-- **默认空数组 = 平台不按任何第二平面的语义求值**，照旧走"探测到就整片
-- 降级"的保守路线。这是唯一安全的默认值：解释一个并不生效的平面，会让
-- 平台以为某条连接被 Deny 拦了、于是不为它生成放行规则，下发之后真的被
-- 拦断；而漏解释一个真在执行的平面只是继续保守。
--
-- **声明必须带理由**（NOT NULL，空串即未声明），与 no_node_agents_reason、
-- business_cycle_reason、managed_system_namespaces_reason 同一形状。
ALTER TABLE cluster
  ADD COLUMN enforced_planes JSON NOT NULL DEFAULT (JSON_ARRAY()),
  ADD COLUMN enforced_planes_reason VARCHAR(512) NOT NULL DEFAULT '';

-- 历史行显式置成空数组，理由同 000027/000028：不写的话已有行拿到的是
-- JSON 的 null 字面量，那一列从此有两种形状。
UPDATE cluster SET enforced_planes = JSON_ARRAY()
 WHERE JSON_TYPE(enforced_planes) = 'NULL';
