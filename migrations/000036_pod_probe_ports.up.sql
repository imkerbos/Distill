-- Pod 的 kubelet 探针端口（readiness / liveness / startup）。
--
-- kubelet 从节点地址发起探测，podSelector 永远选不中它。default-deny 之后
-- 没有对应放行，探测就失败，Pod 被判不健康后杀掉重启——这是最先断、
-- 后果最重的一类（design doc 2026-09-01）。
--
-- 命名端口在采集侧就对着这个 Pod 解析成了数字：NetworkPolicy 的端口名
-- 由 CNI 对着**被选中的 Pod** 解析，而这条基线的对端是节点网段，
-- 名字在那里没有解析依据。
--
-- 允许 NULL 而不是 DEFAULT '[]'，理由同 000032：迁移之前采集的行确实
-- 没有采过这一列，而空数组的含义是「采过，这个 Pod 没有探针」。
-- 后者是 KUBELET_PROBE 基线的 NotApplicable 一档，前者不是——把老行填成
-- '[]' 会让一批从没采过探针的 Pod 冒充「确认没有探针」，而那正是
-- 这条基线最不该出错的方向。
ALTER TABLE observed_pod
  ADD COLUMN probe_ports JSON NULL;
