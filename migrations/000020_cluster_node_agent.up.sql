-- 节点级 agent 登记与「不适用」声明
-- （design doc 2026-08-18-node-agent-applicability）。
--
-- 本文件是新增的第 20 号迁移，不改动 000001–000019 中任何一条已应用的迁移。
--
-- **为什么登记而不是推导**：平台看得见集群里有哪些 hostNetwork DaemonSet，
-- 但看不见它们往哪连 —— agent 连不连工作负载、连哪个端口，写在它自己的配置里。
-- 推断错的方向是危险的那一侧：把一个真的需要放行的 agent 判成"不需要"，
-- 下发之后监控静默中断，而在事故发生时才显现。
--
-- 主键含 (namespace, app)：一个集群可以有多个节点 agent（日志一个、指标一个
-- 是常态）。同一个 (namespace, app) 只能有一条 —— 两条不同端口的登记会生成
-- 两条同名策略，而后者不报错，只是其中一条永远选不中任何 Pod。
CREATE TABLE cluster_node_agent (
  cluster_id   VARCHAR(64)  NOT NULL,
  namespace    VARCHAR(63)  NOT NULL,
  app          VARCHAR(253) NOT NULL,
  host_network TINYINT(1)   NOT NULL,
  target_port  INT          NOT NULL,
  PRIMARY KEY (cluster_id, namespace, app)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 显式声明「本集群没有需要放行的节点 agent」的理由。
--
-- **存理由而不是一个布尔**：这是一次有后果的判断 —— 判错的方向是监控在下发
-- 之后静默中断。事后要答得出「当初凭什么说不需要」，一个 true 答不出来。
--
-- 空串表示没有声明过，与「声明了、理由是空」不需要区分：一次没有理由的声明
-- 在写入侧就会被拒（registry.ValidateCluster）。
ALTER TABLE cluster
  ADD COLUMN no_node_agents_reason VARCHAR(512) NOT NULL DEFAULT '' AFTER ccnp_present;
