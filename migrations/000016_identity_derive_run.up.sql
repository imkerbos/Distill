-- 身份推导的运行记录（design doc 2026-08-18 §2.2）。
--
-- 本文件是新增的第 16 号迁移，不改动 000009–000015 中任何一条已应用的迁移。
-- golang-migrate 只按版本号判断某个库有没有跑过某一版：改一个已被记录的版本，
-- 那个库永远不会重跑修正后的文件，于是它与从零建起来的测试库静默分叉，而分叉
-- 本身不报任何错（见 docs/superpowers/HANDOFF.md 里 000012 的教训）。

-- identity_derive_run 是「这次采集运行的身份区间推导成没成」。
--
-- **与 collection_run 分开一张表**，理由与 000013 把 flow_ingest_run 分出去
-- 逐字相同：资产已经落库是既成事实，一次推导失败不得把它抹掉；反过来，推导
-- 失败也不得被吞掉。合进 collection_run（多加一列 status）之后，只剩两条路 ——
-- 要么让推导失败把一次成功的采集改写成失败（抹掉既成事实），要么让它没有落脚点
-- （被吞掉）。而被吞掉的那一轮在界面上完全正常，下游的表现却是这个集群的每一条
-- 连接都归属不了，因为事实层里连接的两端都靠 pod_identity_interval 解析主体。
--
-- 主键是**被推导的那次采集运行**，不是一个新的推导 ID：这张表要回答的问题是
-- "这次采集的身份推出来了吗"，两行答同一个问题就等于答不出。补跑与重试因此
-- 落回同一行（SaveDeriveRun 走 upsert），与推导本身的幂等一致。
--
-- status 只有 OK 与 FAILED，**没有 PARTIAL**：推导整个跑在一个事务里，要么
-- 整体生效要么整体回滚（见 snapshotstore.DeriveIdentityIntervals），"推了一半"
-- 这个状态构造不出来。给它留一个取值，等于给后来的人一个填它的位置。
--
-- error_reason 是封闭枚举、不用自由文本（CLAUDE.md §3），取值与
-- snapshotstore.DeriveErrorReason 一一对应。空串表示没有失败原因，成败看
-- status —— 与 000011 的 collection_run.error_reason 同一条理由，不用 NULL：
-- 这一列的"没有原因"是一个确定的事实。
--
-- LOCK_UNAVAILABLE 必须与其它原因分开：它表示这个集群另有一次推导正在跑，
-- 处置是重跑而不是排查数据，而它恰恰是最容易被当成数据问题去查的那一种。
--
-- 外键指向 collection_run：一行说不出自己推的是哪次采集的推导记录，事后既
-- 追不到 Pod 观测，也判不出当时那次运行可不可信。
CREATE TABLE identity_derive_run (
  cluster_id   VARCHAR(64) NOT NULL,
  run_id       VARCHAR(64) NOT NULL,   -- 被推导的那次采集运行
  started_at   DATETIME(6) NOT NULL,
  finished_at  DATETIME(6) NOT NULL,
  status       VARCHAR(16) NOT NULL,   -- OK | FAILED
  error_reason VARCHAR(32) NOT NULL DEFAULT '',  -- '' | LOCK_UNAVAILABLE | TIMEOUT | OTHER
  PRIMARY KEY (cluster_id, run_id),
  CONSTRAINT fk_derive_run_collection FOREIGN KEY (cluster_id, run_id)
    REFERENCES collection_run(cluster_id, run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
