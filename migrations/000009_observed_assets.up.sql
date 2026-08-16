-- 采集层轮 1：资产快照落库（design doc 2026-08-16 §4）。
--
-- **时间语义是 append-only。** 每次采集写一批新行，主键含 observed_at，
-- 不覆盖旧行、不计算 valid_to。理由是 CLAUDE.md §4：禁止用当前状态解释
-- 历史数据。只存最新快照会让"这条三周前的流量当时的 Pod 标签是什么"
-- 这类问题拿今天的标签回答，且答得出、不报错。
--
-- 区间闭合（valid_from / valid_to）与身份解析留给下一轮。地基在这里就位，
-- 那一轮只需要补区间与解析器，不需要重做这些表。
--
-- 所有身份类表主键首列都是 cluster_id（CLAUDE.md §4）：不同集群的
-- Pod CIDR 可能重叠，缺了它会 join 到错误集群的 Pod 上且不报错。
--
-- 字段长度按 Kubernetes 自己的上限取：namespace 是 DNS label，最长 63；
-- 其余名字是 DNS subdomain，最长 253。取满 255 会让几张表的主键超过
-- InnoDB 3072 字节的上限。

-- collection_run 是一次采集运行的元数据。
--
-- status 是封闭枚举。PARTIAL 必须与 OK 区分：采到 Pod 但 NetworkPolicy
-- 权限不足时得到的是一份缺了策略的快照，报成 OK 会让下游把它当作完整
-- 事实使用，于是"这个集群没有任何策略"与"我们没被授权看策略"变得无法
-- 区分 —— 而前者会让平台推荐一份 default-deny。
--
-- observed_at 与 started_at 分开：前者是这批记录共用的观测时刻，
-- 是各 observed_* 表的 join 键；后者只是运维时间线上的一个点。
CREATE TABLE collection_run (
  cluster_id  VARCHAR(64) NOT NULL,
  run_id      VARCHAR(64) NOT NULL,
  observed_at DATETIME(6) NOT NULL,
  started_at  DATETIME(6) NOT NULL,
  finished_at DATETIME(6) NOT NULL,
  status      VARCHAR(16) NOT NULL,   -- OK | PARTIAL | FAILED
  PRIMARY KEY (cluster_id, run_id),
  KEY idx_run_recent (cluster_id, observed_at),
  CONSTRAINT fk_run_cluster FOREIGN KEY (cluster_id) REFERENCES cluster(cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- collection_run_resource 是各类资源的记录条数。
--
-- 一类资源一行而非一类资源一列：新增一种被采资源时不需要改表结构，
-- 也就不会出现"表里少一列、而少的那一列恰好是这次没采到的那类"。
CREATE TABLE collection_run_resource (
  cluster_id  VARCHAR(64) NOT NULL,
  run_id      VARCHAR(64) NOT NULL,
  resource    VARCHAR(32) NOT NULL,
  item_count  INT         NOT NULL,
  PRIMARY KEY (cluster_id, run_id, resource),
  CONSTRAINT fk_run_resource FOREIGN KEY (cluster_id, run_id)
    REFERENCES collection_run(cluster_id, run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- collection_run_failure 是各类资源的失败记录，一类最多一条。
--
-- reason 是封闭枚举，detail 是给人读的原始错误文本。两者分开的理由与
-- unknown_reason 相同（CLAUDE.md §3）：统计口径只认 reason，
-- 自由文本进了统计就再也对不上账。
--
-- FORBIDDEN 单独成项：它是唯一一种"集群是好的、是我们没被授权"的失败，
-- 处置是改 RBAC 而不是重试，而重试恰恰是它最常被误配的处置方式。
CREATE TABLE collection_run_failure (
  cluster_id VARCHAR(64) NOT NULL,
  run_id     VARCHAR(64) NOT NULL,
  resource   VARCHAR(32) NOT NULL,
  reason     VARCHAR(32) NOT NULL,   -- FORBIDDEN | NOT_FOUND | TIMEOUT | UNAVAILABLE | OTHER
  detail     TEXT        NOT NULL,
  PRIMARY KEY (cluster_id, run_id, resource),
  CONSTRAINT fk_run_failure FOREIGN KEY (cluster_id, run_id)
    REFERENCES collection_run(cluster_id, run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- collection_warning 是采集当时发现的、事实与登记不符的地方。
--
-- 与 failure 分开：这些不是采集失败。一条落在登记网段之外的 Pod IP
-- 说明注册表填错了，而采集本身是成功的。混进 failure 会让一次成功的
-- 采集被判成 PARTIAL；忽略则会让这个错误一直等到求值阶段，
-- 以"这条流量属于另一个集群"的形式表现出来。
CREATE TABLE collection_warning (
  cluster_id VARCHAR(64)  NOT NULL,
  run_id     VARCHAR(64)  NOT NULL,
  seq        INT          NOT NULL,
  kind       VARCHAR(64)  NOT NULL,
  subject    VARCHAR(320) NOT NULL,
  detail     TEXT         NOT NULL,
  PRIMARY KEY (cluster_id, run_id, seq),
  KEY idx_warning_kind (cluster_id, run_id, kind),
  CONSTRAINT fk_warning_run FOREIGN KEY (cluster_id, run_id)
    REFERENCES collection_run(cluster_id, run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE observed_namespace (
  cluster_id  VARCHAR(64)  NOT NULL,
  name        VARCHAR(63)  NOT NULL,
  observed_at DATETIME(6)  NOT NULL,
  run_id      VARCHAR(64)  NOT NULL,
  labels      JSON         NOT NULL,
  in_mesh     TINYINT(1)   NOT NULL,
  mesh_source VARCHAR(32)  NOT NULL,
  mesh_detail VARCHAR(255) NOT NULL,
  PRIMARY KEY (cluster_id, name, observed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- observed_pod 是身份还原的主表。
--
-- idx_pod_ip 是下一轮 (cluster_id, pod_ip, 时间) → Pod 映射的入口。
-- 现在就建：那条查询是历史回放的必经之路，而没有这个索引它会退化成
-- 全表扫描，届时表已经很大了。
--
-- ip_scope 与 ip_scope_reason 是采集当时的归属判定。留在行上而非查询时
-- 重算：当时的登记内容才是这条判定的依据，事后登记改了，重算会得到
-- 一个与当时的告警对不上的结果。
--
-- workload_* 与 owner_* 都保留：对账一律用 workload 而非 pod，
-- 而 ReplicaSet 每次发布都换名字，用它当主体会让同一个服务在发布前后
-- 变成两个东西。
CREATE TABLE observed_pod (
  cluster_id      VARCHAR(64)  NOT NULL,
  namespace       VARCHAR(63)  NOT NULL,
  name            VARCHAR(253) NOT NULL,
  observed_at     DATETIME(6)  NOT NULL,
  run_id          VARCHAR(64)  NOT NULL,
  uid             VARCHAR(36)  NOT NULL,
  phase           VARCHAR(16)  NOT NULL,
  ip              VARCHAR(45)  NOT NULL,
  ip_scope        VARCHAR(16)  NOT NULL,
  ip_scope_reason VARCHAR(32)  NOT NULL,
  labels          JSON         NOT NULL,
  host_network    TINYINT(1)   NOT NULL,
  node_name       VARCHAR(253) NOT NULL,
  service_account VARCHAR(253) NOT NULL,
  owner_kind      VARCHAR(64)  NOT NULL,
  owner_name      VARCHAR(253) NOT NULL,
  workload_kind   VARCHAR(64)  NOT NULL,
  workload_name   VARCHAR(253) NOT NULL,
  in_mesh         TINYINT(1)   NOT NULL,
  mesh_source     VARCHAR(32)  NOT NULL,
  mesh_detail     VARCHAR(255) NOT NULL,
  PRIMARY KEY (cluster_id, namespace, name, observed_at),
  KEY idx_pod_ip (cluster_id, ip, observed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- observed_node 记的是集群自己报的网段。
--
-- 它与 cluster.pod_cidr 是两个来源：后者是人填的登记，前者是事实。
-- 两者不一致时错的是登记，而这张表让那次不一致可以被看见。
CREATE TABLE observed_node (
  cluster_id   VARCHAR(64)  NOT NULL,
  name         VARCHAR(253) NOT NULL,
  observed_at  DATETIME(6)  NOT NULL,
  run_id       VARCHAR(64)  NOT NULL,
  pod_cidrs    JSON         NOT NULL,
  internal_ips JSON         NOT NULL,
  PRIMARY KEY (cluster_id, name, observed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE observed_service (
  cluster_id  VARCHAR(64)  NOT NULL,
  namespace   VARCHAR(63)  NOT NULL,
  name        VARCHAR(253) NOT NULL,
  observed_at DATETIME(6)  NOT NULL,
  run_id      VARCHAR(64)  NOT NULL,
  service_type VARCHAR(32) NOT NULL,
  selector    JSON         NOT NULL,
  cluster_ip  VARCHAR(45)  NOT NULL,
  ports       JSON         NOT NULL,
  PRIMARY KEY (cluster_id, namespace, name, observed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- observed_endpoints 已按 Service 归并，一个 Service 一行。
-- 只收就绪后端：一个后端全部未就绪的 Service，照它生成的放行规则
-- 同样指向一个当下谁也到不了的集合。
CREATE TABLE observed_endpoints (
  cluster_id  VARCHAR(64)  NOT NULL,
  namespace   VARCHAR(63)  NOT NULL,
  name        VARCHAR(253) NOT NULL,
  observed_at DATETIME(6)  NOT NULL,
  run_id      VARCHAR(64)  NOT NULL,
  addresses   JSON         NOT NULL,
  ports       JSON         NOT NULL,
  PRIMARY KEY (cluster_id, namespace, name, observed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- observed_network_policy 保留 YAML 原文而非解析后的结构。
--
-- 这是"集群当时是什么样"的证据。解析可以重做，丢掉的字段找不回来。
-- 唯一被刻意丢掉的是 managedFields —— 那是 apply 的簿记，与策略语义
-- 无关，体积却常常超过 spec 本身。
CREATE TABLE observed_network_policy (
  cluster_id  VARCHAR(64)  NOT NULL,
  namespace   VARCHAR(63)  NOT NULL,
  name        VARCHAR(253) NOT NULL,
  observed_at DATETIME(6)  NOT NULL,
  run_id      VARCHAR(64)  NOT NULL,
  uid         VARCHAR(36)  NOT NULL,
  manifest    MEDIUMTEXT   NOT NULL,
  PRIMARY KEY (cluster_id, namespace, name, observed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- observed_gateway 一个后端 Service 一行：一个 Ingress 可以指向多个后端，
-- 而 Baseline 关心的"哪些 workload 是外部入口"是按 Service 数的。
-- 因此 backend_service 必须进主键，否则同一个 Ingress 的多个后端
-- 会互相覆盖，只剩最后写进去的那个。
CREATE TABLE observed_gateway (
  cluster_id      VARCHAR(64)  NOT NULL,
  namespace       VARCHAR(63)  NOT NULL,
  name            VARCHAR(253) NOT NULL,
  backend_service VARCHAR(253) NOT NULL,
  observed_at     DATETIME(6)  NOT NULL,
  run_id          VARCHAR(64)  NOT NULL,
  gateway_kind    VARCHAR(32)  NOT NULL,   -- 本轮只有 Ingress
  PRIMARY KEY (cluster_id, namespace, name, backend_service, observed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
