CREATE TABLE cluster (
  cluster_id     VARCHAR(64)  NOT NULL,
  display_name   VARCHAR(128) NOT NULL,
  pod_cidr       VARCHAR(64)  NOT NULL,
  node_cidr      VARCHAR(64)  NOT NULL,
  ccnp_present   TINYINT(1)   NOT NULL DEFAULT 0,
  onboard_state  VARCHAR(32)  NOT NULL,
  deleted_at     DATETIME(6)  NULL,
  created_at     DATETIME(6)  NOT NULL,
  updated_at     DATETIME(6)  NOT NULL,
  PRIMARY KEY (cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE cluster_apiserver (
  cluster_id VARCHAR(64)  NOT NULL,
  host       VARCHAR(128) NOT NULL,
  cidr       VARCHAR(64)  NOT NULL,
  port       INT          NOT NULL,
  PRIMARY KEY (cluster_id, host),
  CONSTRAINT fk_apiserver_cluster FOREIGN KEY (cluster_id) REFERENCES cluster(cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE cluster_health_check_source (
  cluster_id VARCHAR(64) NOT NULL,
  cidr       VARCHAR(64) NOT NULL,
  PRIMARY KEY (cluster_id, cidr),
  CONSTRAINT fk_hcs_cluster FOREIGN KEY (cluster_id) REFERENCES cluster(cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE cluster_git_binding (
  cluster_id          VARCHAR(64)  NOT NULL,
  repo_url            VARCHAR(512) NOT NULL,
  branch              VARCHAR(128) NOT NULL,
  policy_path         VARCHAR(512) NOT NULL,
  credential_ref      VARCHAR(256) NULL,
  last_written_commit CHAR(40)     NULL,
  PRIMARY KEY (cluster_id),
  CONSTRAINT fk_git_cluster FOREIGN KEY (cluster_id) REFERENCES cluster(cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE policy_import (
  cluster_id     VARCHAR(64)  NOT NULL,
  import_id      VARCHAR(64)  NOT NULL,
  plane          VARCHAR(32)  NOT NULL,
  role           VARCHAR(32)  NOT NULL,
  source         VARCHAR(32)  NOT NULL,
  namespace      VARCHAR(253) NOT NULL,
  name           VARCHAR(253) NOT NULL,
  yaml           MEDIUMTEXT   NOT NULL,
  spec_hash      CHAR(64)     NOT NULL,
  git_commit_sha CHAR(40)     NULL,
  imported_by    VARCHAR(128) NOT NULL,
  imported_at    DATETIME(6)  NOT NULL,
  deleted_at     DATETIME(6)  NULL,
  PRIMARY KEY (cluster_id, import_id),
  KEY idx_role (cluster_id, role, namespace),
  CONSTRAINT fk_import_cluster FOREIGN KEY (cluster_id) REFERENCES cluster(cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 审计表刻意不设外键，也不随集群软删失效：集群下线之后，
-- 它的审计记录仍须可查 —— 而那正是复盘时最需要的部分。
CREATE TABLE audit_log (
  id         BIGINT       NOT NULL AUTO_INCREMENT,
  cluster_id VARCHAR(64)  NOT NULL,
  actor      VARCHAR(128) NOT NULL,
  action     VARCHAR(64)  NOT NULL,
  target     VARCHAR(512) NOT NULL,
  before_val JSON         NULL,
  after_val  JSON         NULL,
  at         DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_cluster_time (cluster_id, at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
