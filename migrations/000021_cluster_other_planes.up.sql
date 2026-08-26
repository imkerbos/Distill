-- 集群里除标准 NetworkPolicy 之外还有没有别的策略平面（design doc 2026-08-25 §2）。
--
-- 本文件是新增的第 21 号迁移，不改动 000001–000020 中任何一条已应用的迁移。
-- golang-migrate 只按版本号判断某个库跑过哪一版：改一个已被记录的版本，那个库
-- 永远不会重跑修正后的文件，于是它与从零建起来的测试库静默分叉，而分叉本身
-- 不报任何错。
--
-- **为什么要有这一列**：在它之前只有 ccnp_present 这个布尔，零值 0 的含义是
-- 「不存在其它平面」。于是「没人去勾」与「确认不存在」在数据里长得一模一样，
-- 而平台对前者给出的是满置信度判定。一个装了 CiliumNetworkPolicy 或
-- AdminNetworkPolicy 的集群会因此拿到一份看起来可信、实际可能相反的报告 ——
-- ANP 的优先级高于 NetworkPolicy，能整体覆盖判定结论。
--
-- **默认值是 UNKNOWN，不是 NONE。** 与 000014 把 data_source 默认成 COLLECTED
-- 是同一条纪律：默认值要朝「难看但可见」的方向掉。判错成 UNKNOWN 的症状是
-- 判定被标成 DEGRADED —— 难看，且立刻可见；判错成 NONE 的症状是一份看不出
-- 问题的假报告。
--
-- 长度取 VARCHAR(16)：三个取值最长 7 个字符，与 data_source 一列同宽。
-- 不用 ENUM —— 本库其余封闭枚举一律存字符串、由 Go 侧的 Valid() 把关。
ALTER TABLE cluster
  ADD COLUMN other_planes VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN' AFTER ccnp_present;

-- 已经登记了「存在 Cilium 策略」的集群直接回填成 PRESENT：那是一句操作者
-- 做过的判断，不该因为换了字段就丢掉。其余一律留在 UNKNOWN —— 它们从来
-- 没有被查过，而不是被确认过没有。
UPDATE cluster SET other_planes = 'PRESENT' WHERE ccnp_present = 1;
