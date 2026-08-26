-- 每条候选规则背后的证据积累（design doc 2026-08-25 §4）。
--
-- 本文件是新增的第 24 号迁移，不改动 000001–000023 中任何一条已应用的迁移。
--
-- **为什么要有这张表**：一条"看了 15 分钟、3 条连接"的规则与一条"看了 30 天、
-- 200 万条"的规则，今天在界面上长得一模一样，而它们的可靠性差着数量级。
-- 候选集是每次读时现算的，算出来的 flowCount 只描述当前那一个窗口 ——
-- "这条规则我们观察了多久"必须跨窗口积累，现算不出来。
--
-- **主键含 cluster_id**（CLAUDE.md §4）：规则指纹只在集群内唯一，不同集群
-- 的同名 workload 会生成同样的指纹，缺了它两个集群的证据会累加到一起。
--
-- **主体进主键**，与 rule_override（000003）的键形状一致：规则指纹只覆盖
-- 规则内容，不含主体 —— "egress 到 kube-dns:53" 在集群里每个 workload 上
-- 都是同一个指纹。只按指纹归集的话，一个窗口里 40 个 workload 会把 windows
-- 加 40 次，一次采集看起来像观察了 40 个窗口，而这个虚高的数字正是给人
-- 判断"这条规则能不能信"用的。
--
-- 指纹本身也必须在键里：一个 workload 上有多条规则，而"这一条放行观察了
-- 多久"才是要回答的问题。
CREATE TABLE rule_evidence (
  cluster_id    VARCHAR(64)  NOT NULL,
  fingerprint   CHAR(64)     NOT NULL,
  namespace     VARCHAR(253) NOT NULL,
  workload      VARCHAR(253) NOT NULL,
  -- 首末观测：**取的是观测窗口的边界，不是记录时刻**。窗口才是"我们在看"
  -- 的那段时间；用记录时刻会把一次补采说成一次新观测。
  first_seen    DATETIME(6)  NOT NULL,
  last_seen     DATETIME(6)  NOT NULL,
  -- 这条规则出现过的窗口数。跨越的窗口越多，它越不像一次偶然。
  windows       INT UNSIGNED NOT NULL,
  -- 其中**完整度为 COMPLETE** 的窗口数（spec §4 的 completeness 一项）。
  --
  -- 与 windows 分开而不是只留一个数：一条规则在二十个"证明不了看全"的窗口里
  -- 出现过，说明的是"我们看了很多次"，不是"我们看全了"。只报总窗口数会让
  -- 前者显示成后者，而这个平台最不能给出的正是那种错觉。
  complete_windows INT UNSIGNED NOT NULL,
  -- 累计观测到的连接次数。与 windows 分开：一个窗口里刷了十万次的规则
  -- 与十个窗口里各出现一次的规则，可靠性不是一回事。
  observations  BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (cluster_id, namespace, workload, fingerprint),
  -- 按指纹反查："这条规则在这个集群里还有谁在用"，用于将来按规则聚合。
  KEY idx_rule_evidence_fingerprint (cluster_id, fingerprint)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
