-- observed_admin_policy 保留 ANP 与 BANP 的 YAML 原文。
--
-- 与 observed_network_policy 同形，但**不共用一张表**：这一族是集群级的
-- （没有 namespace）、带优先级、且求值次序与标准 NetworkPolicy 完全不同
-- （ANP 在前、BANP 在后）。混进同一张表要靠一个 plane 字段区分，而那个字段
-- 一旦漏判，一条兜底规则就会被当成前置规则解释 —— 方向恰好相反。
--
-- 本轮**只存不解释**：落库让"这个集群上有哪些 ANP、当时长什么样"成为可以
-- 回看的事实，求值仍然照旧整片降级。
--
-- 主键含 cluster_id（CLAUDE.md §4）与 observed_at：同一条策略在每一次采集
-- 里都留一行，历史查询才能按 (cluster_id, name, timestamp) join 到当时那一份，
-- 而不是拿今天的策略解释上周的流量。
-- kind 进主键：ANP 与 BANP 是两类对象，同名不冲突。
CREATE TABLE observed_admin_policy (
  cluster_id     VARCHAR(64)  NOT NULL,
  policy_kind    VARCHAR(32)  NOT NULL,
  name           VARCHAR(253) NOT NULL,
  observed_at    DATETIME(6)  NOT NULL,
  run_id         VARCHAR(64)  NOT NULL,
  uid            VARCHAR(36)  NOT NULL,
  -- priority 与 priority_known 分开：0 是合法且最高的 ANP 优先级，
  -- 拿 0 表示"没读到"会把一条读不懂的策略排到所有策略之前。
  priority       INT          NOT NULL,
  priority_known TINYINT(1)   NOT NULL,
  manifest       MEDIUMTEXT   NOT NULL,
  PRIMARY KEY (cluster_id, policy_kind, name, observed_at),
  KEY idx_admin_policy_run (cluster_id, run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
