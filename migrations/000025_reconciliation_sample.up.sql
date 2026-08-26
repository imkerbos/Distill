-- 分歧样本：让"payment/api 20%"这个数字能被点开
-- （design doc 2026-08-25-trust-engineering §3.4）。
--
-- 本文件是新增的第 25 号迁移，不改动 000001–000024 中任何一条已应用的迁移。
--
-- **为什么必须有这张表**：一致率门禁按分歧率拦人，而一个只有比率的界面
-- 给不出下一步 —— 操作者要看的是哪几条连接对不上，才能判断平台漏了什么
-- （多半是它不解释的另一个策略平面）。没有样本，那道门只能拦住人，
-- 不能告诉他怎么办。
--
-- **只存两类分歧，且每 (主体, 类别) 至多 N 条**：AGREE 量最大且没有下钻
-- 价值，SOURCE_SILENT 与 PLATFORM_UNKNOWN 不是"我们算错了"。全存会让这张表
-- 按流量规模增长，而这个平台的失控方向是账单（CLAUDE.md §5）。
--
-- 量级估算：每次对账每主体每类至多 5 条，两类共 10 条。1000 个 workload 的
-- 集群单次上限 1 万行；15 分钟一轮、保留 30 天则约 2900 万行上限 —— 而这是
-- **全部主体都在持续分歧**的极端值，正常集群分歧主体是个位数，实际两三个
-- 数量级以下。真到了上限，要解决的是那个集群的分歧本身，不是这张表。
--
-- **主键含 cluster_id**（CLAUDE.md §4）：不同集群 Pod CIDR 可能重叠，
-- 缺了它两个集群的样本会混在一起，且不报错。
CREATE TABLE reconciliation_sample (
  cluster_id    VARCHAR(64)  NOT NULL,
  run_id        CHAR(32)     NOT NULL,
  namespace     VARCHAR(253) NOT NULL,
  workload      VARCHAR(253) NOT NULL,
  -- 分类，封闭枚举（internal/reconcile.Class）。只会出现两类分歧。
  class         VARCHAR(32)  NOT NULL,
  -- seq 是同一 (主体, 类别) 下的序号，让主键不必依赖连接内容 ——
  -- 同一条连接在一个窗口里可能出现多次，用五元组做键会把它们折叠掉，
  -- 而"同一个端口反复出现"恰恰是最要紧的那个信号。
  seq           TINYINT UNSIGNED NOT NULL,
  -- 两端与端口：渲染证据要的全部内容。
  --
  -- 存 IP 而不是身份：身份是按时刻解析出来的，而样本是给人看"当时那条连接
  -- 长什么样"。身份要下钻时再按 (cluster_id, ip, timestamp) 去查。
  src_ip        VARCHAR(45)  NOT NULL,
  dst_ip        VARCHAR(45)  NOT NULL,
  protocol      VARCHAR(8)   NOT NULL,
  dst_port      INT          NOT NULL,
  -- 连接发生的时刻。**不是记录写入的时刻**：下钻要按它去对齐历史快照，
  -- 用写入时刻会把人带到错误的那一份 Pod 名册上。
  occurred_at   DATETIME(6)  NOT NULL,
  PRIMARY KEY (cluster_id, run_id, namespace, workload, class, seq),
  CONSTRAINT fk_recon_sample_run FOREIGN KEY (cluster_id, run_id)
    REFERENCES reconciliation_run (cluster_id, run_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
