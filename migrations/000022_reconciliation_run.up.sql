-- 一次对账的结果（design doc 2026-08-25 §3）。
--
-- 本文件是新增的第 22 号迁移，不改动 000001–000021 中任何一条已应用的迁移。
--
-- **为什么要落库**：一致率"此刻算一次"回答不了最要紧的那个问题 ——
-- 它在变好还是变坏。一次 97% 无所谓，从 100% 掉到 97% 才是信号。
--
-- **主键含 cluster_id**（CLAUDE.md §4）：不同集群的 workload 重名是常态，
-- 缺了它两个集群的分歧会 join 到一起且不报错。
CREATE TABLE reconciliation_run (
  cluster_id        VARCHAR(64)  NOT NULL,
  run_id            CHAR(32)     NOT NULL,
  -- 被对账的观测窗口。与 flow_ingest 的窗口对齐，因此可以按窗口回看。
  window_from       DATETIME(6)  NOT NULL,
  window_to         DATETIME(6)  NOT NULL,
  computed_at       DATETIME(6)  NOT NULL,
  -- 来源报不报判定。为 0 时下面的计数全在 source_silent 里，一致率不成立 ——
  -- 存下来是为了让"那段时间根本对不了账"与"那段时间一致率很低"分得开。
  source_reports    TINYINT(1)   NOT NULL,
  agree             INT UNSIGNED NOT NULL,
  source_silent     INT UNSIGNED NOT NULL,
  platform_unknown  INT UNSIGNED NOT NULL,
  over_permissive   INT UNSIGNED NOT NULL,
  under_permissive  INT UNSIGNED NOT NULL,
  PRIMARY KEY (cluster_id, run_id),
  KEY idx_recon_window (cluster_id, window_from)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 按主体的明细。
--
-- **只存聚合，不存每一条分歧的原文**（design doc §3.4）：分歧量随流量增长，
-- 全存会让这张表成为账单上最大的一项，而回答"哪个 workload 有问题"只需要
-- 计数。要看具体是哪几条连接，去 /flows 按窗口查 —— 那份数据本来就在。
CREATE TABLE reconciliation_subject (
  cluster_id        VARCHAR(64)  NOT NULL,
  run_id            CHAR(32)     NOT NULL,
  namespace         VARCHAR(253) NOT NULL,
  -- 空串表示这些 Pod 一个 workload 归属标签都没有 —— 那本身是要被修的东西，
  -- 因此它是一个有意义的取值，不是缺失。
  workload          VARCHAR(253) NOT NULL,
  agree             INT UNSIGNED NOT NULL,
  over_permissive   INT UNSIGNED NOT NULL,
  under_permissive  INT UNSIGNED NOT NULL,
  PRIMARY KEY (cluster_id, run_id, namespace, workload),
  CONSTRAINT fk_recon_subject_run FOREIGN KEY (cluster_id, run_id)
    REFERENCES reconciliation_run (cluster_id, run_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
