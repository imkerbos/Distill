-- 集群 agent 的机器身份（design doc 2026-08-18 §3）。
--
-- 本文件是新增的第 17 号迁移，不改动 000001–000016 中任何一条已应用的迁移。
-- golang-migrate 只按版本号判断某个库跑过哪一版：改一个已被记录的版本，那个库
-- 永远不会重跑修正后的文件，于是它与从零建起来的测试库静默分叉，而分叉本身
-- 不报任何错。
--
-- **为什么要有这张表**：今天从 UI 登记一个集群到平台真的拿到数据之间是断的 ——
-- 凭据没有写入路径（internal/httpapi 零处 import internal/secrets），采集器只能
-- 靠人把一份 kubeconfig 手工放进凭据后端。推送式接入把方向反过来：采集器跑在
-- 被管集群里，用自己的 ServiceAccount 读，拿这张表里的 token 把结果推回平台。
-- 平台从此不持有那个集群的任何凭据。
--
-- **主键含 cluster_id**（CLAUDE.md §4）。这条记录决定一次推送的数据归到哪个
-- 集群，而不同集群的 Pod CIDR 可能重叠 —— 归错的后果是 A 集群的 Pod 写进
-- B 集群的身份表，之后 join 落到错误的 Pod 上**且不报错**。
--
-- agent_id 另建唯一索引：认证时只有 token 在手，要先由公开段反查出集群，
-- 那一步必须是索引命中而不是全表比对 —— 摄入是高频路径（规范 §24）。
--
-- **token 只存 SHA-256，不存明文，也不用 bcrypt**（design doc §3.2）。bcrypt 是
-- 为低熵人类口令设计的，慢哈希买的是暴力破解成本；agent token 是 256 bit
-- crypto/rand，没有字典空间可省，慢哈希只会给每一次摄入请求加一次 bcrypt 的
-- CPU，而 /me/password 已经是全平台唯一的 CPU 放大点，不该再往最热的路上搬
-- 第二个。
--
-- VARBINARY(32) 而不是 CHAR(64) 存 hex：摘要是字节不是文本，存 hex 会让比对
-- 多一层编码约定，而那层约定只活在代码里。
--
-- last_seen_at / revoked_at 可空：两者都表示「还没发生过」，而不是某个默认
-- 时刻。填一个零值日期会让「从未用过」读起来像「公元元年用过一次」。
CREATE TABLE cluster_agent (
  cluster_id   VARCHAR(64)   NOT NULL,
  agent_id     VARCHAR(32)   NOT NULL,
  token_hash   VARBINARY(32) NOT NULL,
  state        VARCHAR(16)   NOT NULL,
  created_by   VARCHAR(128)  NOT NULL,
  created_at   DATETIME(6)   NOT NULL,
  last_seen_at DATETIME(6)   NULL,
  revoked_at   DATETIME(6)   NULL,
  PRIMARY KEY (cluster_id, agent_id),
  UNIQUE KEY uk_cluster_agent_agent_id (agent_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
