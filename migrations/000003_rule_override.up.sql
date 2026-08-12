CREATE TABLE rule_override (
  cluster_id        VARCHAR(64)  NOT NULL,
  namespace         VARCHAR(253) NOT NULL,
  workload          VARCHAR(253) NOT NULL,
  rule_fingerprint  CHAR(64)     NOT NULL,
  decision          VARCHAR(16)  NOT NULL,
  reason            TEXT         NOT NULL,
  decided_by        VARCHAR(128) NOT NULL,
  decided_at        DATETIME(6)  NOT NULL,
  -- 这条决定落进 Git 的 commit；NULL 表示决定已做出但尚未落地。
  -- 本轮恒为 NULL，轮 3 写回 Git 时填。
  merged_commit_sha CHAR(40)     NULL,
  deleted_at        DATETIME(6)  NULL,
  PRIMARY KEY (cluster_id, namespace, workload, rule_fingerprint),
  CONSTRAINT fk_override_cluster FOREIGN KEY (cluster_id) REFERENCES cluster(cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
