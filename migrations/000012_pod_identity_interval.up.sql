-- 身份时序映射（design doc 2026-08-17 §2）：把 append-only 的 Pod 观测收敛成
-- 可按时刻查询的区间，回答「在时刻 T，这个 IP 是谁」。
--
-- 这张表由**已落库的快照推导**产生，不由采集器直接写。采集器看见的是
-- "这一刻有什么"，区间是"从哪一刻到哪一刻"—— 后者要看两次采集之间的差异，
-- 而采集器按设计是无状态的单次运行。让它维护区间，等于要求它信任上一次
-- 运行的结论，而那次运行可能是 PARTIAL。
--
-- 行数与「Pod 生命周期次数」同阶，不与采集次数同阶（spec §7）：一个稳定的
-- Pod 无论被采集多少次，只有一行。这正是引入区间的收益 —— observed_pod 随
-- 采集次数线性增长，这张表不会。保留期与 observed_pod 一致，到期一起清理，
-- 且必须先清快照后清区间，否则会出现有区间无来源的行。
--
-- 主键首列是 cluster_id（CLAUDE.md §4）：不同集群的 Pod CIDR 可能重叠，
-- 缺了它会解到另一个集群的 Pod 上且不报错。
--
-- 主键末列是 pod_uid（spec §2）：一个节点上的多个 hostNetwork Pod 共用节点
-- 地址 —— CNI agent、日志采集、node exporter 都是这个形状，几乎每个真实集群
-- 在同一时刻都有若干个主体指向同一个地址。少了 pod_uid 这张表放不下这个
-- 事实：要么静默丢掉其中一个，要么整次推导失败。让它把重叠原样存下来，
-- 解析器按 spec §4 对这一刻返回 AMBIGUOUS —— 那正是它本来就被设计要说的话。

-- valid_to 用 NULL 表示"至今未关闭"，不用哨兵时刻。
--
-- 一个"很远的未来"在按时刻查询时会被当成一段真实的覆盖，于是"还没结束"
-- 与"结束于 9999 年"变成同一个值。这里的 NULL 恰好是它在 SQL 里的本义：
-- 上界还不知道。这与 000011 拒绝给 error_reason 用 NULL 不矛盾 —— 那一列的
-- "没有原因"是一个确定的事实，这一列的"没有上界"不是。
--
-- closed_reason 是封闭枚举，不用自由文本（CLAUDE.md §3）：关闭动作是
-- "这个 Pod 不在了"这一结论的落点，事后要能按原因聚合追溯。空串表示尚未
-- 关闭；新增取值要同步改 identity.ClosedReason 与统计口径。
--
-- first_run_id 与 last_run_id 都留，且都不可省：推导不写审计行（spec §8），
-- 可追溯性全靠这两列。尤其是关闭 —— 一个区间被关掉意味着平台认定这个
-- 工作负载下线了，事后必须能追到那个结论是哪一次采集下的、那次采集可不可信。
--
-- 字段长度与 observed_pod 逐列对齐：那里是这张表唯一的数据来源，
-- 两边不一致会让一个合法的 Pod 名在推导时被静默截断。
CREATE TABLE pod_identity_interval (
  cluster_id    VARCHAR(64)  NOT NULL,
  pod_ip        VARCHAR(45)  NOT NULL,
  valid_from    DATETIME(6)  NOT NULL,
  valid_to      DATETIME(6)  NULL,      -- NULL = 至今未关闭
  namespace     VARCHAR(63)  NOT NULL,
  pod_name      VARCHAR(253) NOT NULL,
  pod_uid       VARCHAR(36)  NOT NULL,
  workload_kind VARCHAR(64)  NOT NULL,
  workload_name VARCHAR(253) NOT NULL,
  host_network  TINYINT(1)   NOT NULL,
  in_mesh       TINYINT(1)   NOT NULL,
  first_run_id  VARCHAR(64)  NOT NULL,
  last_run_id   VARCHAR(64)  NOT NULL,
  closed_reason VARCHAR(32)  NOT NULL DEFAULT '',  -- '' | ABSENT_IN_LATER_RUN
  PRIMARY KEY (cluster_id, pod_ip, valid_from, pod_uid),
  -- 推导每次只取本集群还开着的区间，条数与在跑的 Pod 同阶。没有这个索引
  -- 它会随历史线性退化成全表扫描，而那正是 spec §6 拒绝全表重算的理由。
  KEY idx_interval_open (cluster_id, valid_to)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 推导按 (cluster_id, observed_at) 取一次运行的全部 Pod 观测。
--
-- observed_pod 现有的两个索引都不服务这条查询：主键是
-- (cluster_id, namespace, name, observed_at)，idx_pod_ip 是 (cluster_id, ip,
-- observed_at)，两者都得先定住第二列。缺这个索引，每推导一次就要扫完这个
-- 集群的全部 Pod 观测历史 —— 成本随采集次数无上限增长，而推导本身
-- 按 spec §10 应当与采集次数同阶。
ALTER TABLE observed_pod
  ADD KEY idx_pod_run (cluster_id, observed_at);
