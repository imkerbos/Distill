-- 回滚节点 agent 登记与「不适用」声明。
--
-- 丢掉的是**登记与一次人工判断**，不是观测：hostNetwork DaemonSet 的观测留在
-- observed_pod 里不受影响。
--
-- 回滚之后 NODE_AGENT 退回「缺失」—— 包括那些曾被声明为不适用的集群。
-- 那是对的失败方向：说不出这个集群不需要，就照旧报缺失。
--
-- 不可恢复的是「谁在什么时候声明的不适用」，而那一条在 audit_log 里。
ALTER TABLE cluster DROP COLUMN no_node_agents_reason;
DROP TABLE cluster_node_agent;
