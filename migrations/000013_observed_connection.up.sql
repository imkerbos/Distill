-- 流量事实层（design doc 2026-08-17 §4 / §6）：存一个时间窗内**观测到的连接**，
-- 以及那次观测漏了多少。
--
-- 本文件是新增的第 13 号迁移，不改动 000009–000012 中任何一条已应用的迁移。
-- golang-migrate 只按版本号判断某个库有没有跑过某一版：改一个已被记录的版本，
-- 那个库永远不会重跑修正后的文件，于是它与从零建起来的测试库静默分叉，而分叉
-- 本身不报任何错（见 docs/superpowers/HANDOFF.md 里 000012 的教训）。

-- flow_ingest_run 是一次流量摄入的运行记录。
--
-- **与 collection_run 分开一张表**（spec §6）。同构，但不合并：资产采集失败
-- 多半是 RBAC，流量摄入失败多半是 relay 或配额，两者的处置人不同。合成一张表
-- 之后，"这个集群最近怎么老是失败"这个问题的排查方向会先糊掉一轮 —— 而排查
-- 方向糊掉的代价在这个平台上不是慢，是有人去改一份根本没错的 RBAC。
--
-- status 是封闭枚举。PARTIAL 必须与 OK 区分，理由与 collection_run 逐字相同：
-- 一次丢了半个窗口的摄入若报成 OK，下游会把一份缺了连接的流量当作完整事实，
-- 于是覆盖那些连接的规则被判"无流量、可收紧"（spec §2 的单向错误）。
--
-- **这张表里没有 completeness 一列，这是刻意的。** 完整度不是一个可以被写下的
-- 事实，而是证据的函数（internal/flow 里它连 setter 都没有）。落一列就等于给了
-- 一个可以填 COMPLETE 而没人给过依据的位置。这里只存证据本身 —— 请求窗口、
-- 实际覆盖窗口、采样率、丢弃数 —— 读取时交回 flow 包重算。
--
-- covered_start / covered_end 用 NULL 表示"来源说不出自己实际覆盖了多少"。
-- 这与 000011 拒绝给 error_reason 用 NULL 不矛盾：那一列的"没有原因"是一个
-- 确定的事实，而这两列的"说不出"本来就是不知道，NULL 恰是它在 SQL 里的本义。
-- 用请求窗口填进去才是危险的那个方向 —— 那等于替来源宣称它跑满了整段时间。
--
-- sample_rate 同理：NULL 是"问不出来"，**不得写 1.0**（spec §5）。1.0 是一句
-- "一条没漏"的断言，而取不到采样率的时候没有人说过这句话。
-- 类型取 DOUBLE 而非 DECIMAL：float64 存进 DOUBLE 再读回来逐位相同，而一个
-- 在往返中变了值的采样率会把完整度从 COMPLETE 翻成 DEGRADED（或者反过来）。
--
-- dropped 用 NULL 区分"来源不报丢弃"与"来源报告丢了 0 条"。后者是证据，
-- 前者是没有证据 —— 只有后者才配参与 COMPLETE 的判定。
CREATE TABLE flow_ingest_run (
  cluster_id    VARCHAR(64)     NOT NULL,
  run_id        VARCHAR(64)     NOT NULL,
  source_kind   VARCHAR(32)     NOT NULL,   -- HUBBLE
  window_start  DATETIME(6)     NOT NULL,
  window_end    DATETIME(6)     NOT NULL,
  covered_start DATETIME(6)     NULL,       -- NULL = 来源说不出实际覆盖
  covered_end   DATETIME(6)     NULL,
  started_at    DATETIME(6)     NOT NULL,
  finished_at   DATETIME(6)     NOT NULL,
  status        VARCHAR(16)     NOT NULL,   -- OK | PARTIAL | FAILED
  error_reason  VARCHAR(32)     NOT NULL DEFAULT '',
  sample_rate   DOUBLE          NULL,       -- NULL = 未知，绝不是 1.0
  dropped       BIGINT UNSIGNED NULL,       -- NULL = 来源不报
  PRIMARY KEY (cluster_id, run_id),
  -- 按窗口检索：查一段时间的流量一律带 cluster_id 与窗口范围，不做全表扫描
  -- （spec §7）。首列是 cluster_id，理由见下。
  KEY idx_ingest_window (cluster_id, window_start),
  CONSTRAINT fk_ingest_run_cluster FOREIGN KEY (cluster_id) REFERENCES cluster(cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- observed_connection 是一个时间窗内观测到的连接，**不是报文**（spec §4）。
--
-- 按连接而非按报文：判定引擎的输入单位就是连接，按报文存体量是连接数的几个
-- 数量级倍，而多出来的信息判定层一条也用不上 —— CLAUDE.md §5 说的"失控方向
-- 是账单，不是性能"。
--
-- 量级（CLAUDE.md §5 / spec §7）：单集群单窗口 ≈ 活跃 Pod 数 × 平均对端数。
-- 按 500 Pod、平均 8 个对端计，1 小时窗口约 4000 行；按小时窗口留 30 天，
-- 单集群约 288 万行。这是本平台第一条随流量而非随操作者动作增长的存储路径，
-- 保留期与窗口粒度按 spec §7 应当可配 —— **本轮尚未实现**，眼下只能靠改代码
-- 调整，这一点记在这里以免被当成已经做完的事。
--
-- 主键首列是 cluster_id（CLAUDE.md §4）：不同集群 Pod CIDR 可能重叠，缺了它
-- 一条 10.4.0.9 的连接会 join 到另一个集群的 Pod 上，且不报错。
-- 次列是 window_start：历史查询的 join 键必须是 (cluster_id, ..., timestamp)，
-- 禁止用当前状态解释历史数据。这个顺序也让按窗口范围查询天然裁剪。
-- 末列 seq 只负责唯一性：同一个窗口里同一对端点、同一协议端口的连接可以被
-- 不同来源各看见一次，用业务列凑主键会让第二条静默顶掉第一条。
--
-- window_start / window_end 与所属运行重复存一份，是为了让窗口范围查询不必先
-- join 运行表 —— 288 万行上的一次 join 换一个 8 字节的冗余，值得。
--
-- verdict_observed 空串表示**来源根本没报判定**，与 ALLOWED 是两件事
-- （spec §4）。把没报当放行，会让一批实际被网络拒掉的连接变成"正常业务流量"
-- 的证据，进而生成一条允许规则 —— 把现网的拒绝变成允许。用 '' 而非 NULL，
-- 与 000012 的 closed_reason 一致：读取一律走 flow.Connection.Verdict 的第二个
-- 返回值，SQL 层不需要再多一种"不知道"。
--
-- src/dst_identity_confidence **逐条落库，不是一个全局值**（spec §4）。一个窗口
-- 里既有解析成功的连接也有解析不了的，汇总成一次运行一个数字会让"90% 可信"
-- 掩盖掉那 10% 恰好落在关键路径上的连接。
-- 两端各一列而不是一条连接一列：一条连接的两端各自解析，源解出来了、目的没
-- 解出来是常态。合成一列必须挑一个说，而"挑一个"就是同一个汇总动作换个尺度
-- 再做一遍。spec §4 的表草图只写了一列，见该节 2026-08-17 补记。
--
-- source_kind 与 sample_rate 逐条落到连接上（spec §4）：完整度元数据是按来源
-- 给的，"这条连接是谁看见的"决定了它该按哪一份元数据解释。同一个窗口以后会
-- 有多个来源同时供数（F 轮的 VPC flow logs），那时按行解释才对得上。
CREATE TABLE observed_connection (
  cluster_id               VARCHAR(64)  NOT NULL,
  window_start             DATETIME(6)  NOT NULL,
  window_end               DATETIME(6)  NOT NULL,
  ingest_run_id            VARCHAR(64)  NOT NULL,
  seq                      INT          NOT NULL,
  src_ip                   VARCHAR(45)  NOT NULL,
  src_kind                 VARCHAR(64)  NOT NULL,
  src_namespace            VARCHAR(63)  NOT NULL,
  src_workload             VARCHAR(253) NOT NULL,
  src_identity_confidence  VARCHAR(16)  NOT NULL,  -- RESOLVED|AMBIGUOUS|NOT_COVERED|NO_DATA
  dst_ip                   VARCHAR(45)  NOT NULL,
  dst_kind                 VARCHAR(64)  NOT NULL,
  dst_namespace            VARCHAR(63)  NOT NULL,
  dst_workload             VARCHAR(253) NOT NULL,
  dst_identity_confidence  VARCHAR(16)  NOT NULL,  -- RESOLVED|AMBIGUOUS|NOT_COVERED|NO_DATA
  protocol                 VARCHAR(8)   NOT NULL,  -- TCP | UDP | SCTP
  port                     INT          NOT NULL,
  observed_count           BIGINT       NOT NULL,
  verdict_observed         VARCHAR(16)  NOT NULL DEFAULT '',  -- '' | ALLOWED | DENIED
  source_kind              VARCHAR(32)  NOT NULL,
  sample_rate              DOUBLE       NULL,      -- NULL = 未知，绝不是 1.0
  PRIMARY KEY (cluster_id, window_start, ingest_run_id, seq),
  CONSTRAINT fk_connection_ingest_run FOREIGN KEY (cluster_id, ingest_run_id)
    REFERENCES flow_ingest_run(cluster_id, run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
