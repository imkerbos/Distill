-- 双栈 Pod 的其余地址（design doc 2026-08-25-existing-policies §6：IPv6/双栈）。
--
-- 本文件是新增的第 27 号迁移，不改动 000001–000026 中任何一条已应用的迁移。
--
-- **为什么必须存**：Kubernetes 的 status.podIPs 是数组，status.podIP 只是它的
-- 第一项。只存主地址的话，双栈 Pod 的第二个地址不进快照、也不进身份区间表 ——
-- 走那个地址的连接解不出主体、判 UNKNOWN，覆盖它的规则于是缺席，下发
-- default-deny 之后那条连接会被拦断。方向危险。
--
-- **主地址仍留在 ip 列**，不并进这一列：现有的每一条按 IP 的查询与展示都指着
-- 它，而单栈集群（绝大多数）这一列恒为 []，形状完全不变。
--
-- 归属判定跟着地址走，因此每一项自带 scope 与 reason：双栈 Pod 的两个地址
-- 可能落在不同的登记网段里，共用一个结论会让其中一个被另一个覆盖。
--
-- 量级：每个双栈 Pod 多一个 JSON 对象（约 60 字节）。单栈集群为空数组。
ALTER TABLE observed_pod
  ADD COLUMN extra_ips JSON NOT NULL DEFAULT (JSON_ARRAY());

-- 历史行显式置成空数组。
--
-- 不写这一句的话 MySQL 给已有行填的是 JSON 的 null 字面量，而不是 []：
-- 读回时 null 与 [] 都会解成"没有额外地址"，行为一样，但那一列的取值从此
-- 有两种形状，而下一个读它的人得先发现这件事。
--
-- **置空是承认那些地址采不回来了，不是断言它们不存在。** 加这一列之前采集的
-- 双栈 Pod，第二个地址当时就没被读出来，数据已经没了。方向是少报地址 ——
-- 走那个地址的连接解不出主体、判 UNKNOWN，保守。下一次采集会补齐。
UPDATE observed_pod SET extra_ips = JSON_ARRAY() WHERE JSON_TYPE(extra_ips) = 'NULL';
