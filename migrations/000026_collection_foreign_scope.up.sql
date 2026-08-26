-- 平台不解释的策略平面所覆盖的主体范围（design doc 2026-08-25 §2）。
--
-- 本文件是新增的第 26 号迁移，不改动 000001–000025 中任何一条已应用的迁移。
--
-- **为什么必须按 run 存，而不是记在 cluster 上**：CNP 的覆盖范围会变，
-- 今天那条选中 A、昨天那条可能选中 B。拿"当前范围"去解释历史窗口，B 那一批
-- 就不会被降级 —— 而那正是它需要被降级的时候（CLAUDE.md §4）。
-- 集群登记上那个三态（other_planes）是另一回事：它只说"有没有"，用当前值
-- 解释历史只会多降级，方向安全。
--
-- **为什么要有它**：在此之前，集群里只要存在一条 CNP，每一条判定都会被标成
-- DEGRADED。粒度粗到等于宣布这个集群完全不可信，而降级面越大，操作者越会
-- 习惯性忽略它 —— 那个标记的全部意义恰恰是让他在真该停手的地方停手。
--
-- **主键含 cluster_id**（CLAUDE.md §4）：不同集群可能有同名 namespace 与
-- 同样的标签，缺了它两个集群的覆盖范围会混在一起，且不报错。
CREATE TABLE collection_foreign_scope (
  cluster_id   VARCHAR(64)  NOT NULL,
  run_id       CHAR(32)     NOT NULL,
  -- seq 让同一次采集里多条范围各占一行；范围本身没有天然唯一键
  -- （两条 CNP 完全可以选中同一批主体，那是两条策略，不该被折叠）。
  seq          SMALLINT UNSIGNED NOT NULL,
  -- 空串表示集群级（CiliumClusterwideNetworkPolicy），跨全部 namespace。
  namespace    VARCHAR(253) NOT NULL,
  -- 标签相等条件，JSON 对象。空对象 {} 表示选中该范围内全部主体，
  -- 与 endpointSelector: {} 的语义一致。
  match_labels JSON         NOT NULL,
  PRIMARY KEY (cluster_id, run_id, seq),
  CONSTRAINT fk_foreign_scope_run FOREIGN KEY (cluster_id, run_id)
    REFERENCES collection_run (cluster_id, run_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 这一次采集算出来的范围完不完整。
--
-- **不完整时判定必须整片降级**，因此它必须与范围本身存在同一次采集里：
-- 分开存会出现"读到了范围、没读到完整度"的窗口，而那时最自然的写法
-- （当作完整）恰好是危险的那一个。
ALTER TABLE collection_run
  ADD COLUMN foreign_scopes_complete TINYINT(1) NOT NULL DEFAULT 0;
