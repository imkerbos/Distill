-- fixture 的两个集群作为种子数据写入，让 demo 走真实路径而非硬编码。
-- 网段与 internal/fixture/asset.go 中原有的值逐字一致；改动其一必须同步另一处。
INSERT INTO cluster
  (cluster_id, display_name, pod_cidr, node_cidr, ccnp_present, onboard_state, created_at, updated_at)
VALUES
  ('prod-asia-1', 'Asia Prod', '10.4.0.0/14', '10.128.0.0/20', 0, 'READY', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
  ('prod-eu-1',   'EU Prod',   '10.4.0.0/14', '10.132.0.0/20', 0, 'READY', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6));

INSERT INTO cluster_apiserver (cluster_id, host, cidr, port) VALUES
  ('prod-asia-1', '10.9.0.2',  '10.9.0.0/28',  443),
  ('prod-eu-1',   '10.13.0.2', '10.13.0.0/28', 443);

INSERT INTO cluster_health_check_source (cluster_id, cidr) VALUES
  ('prod-asia-1', '35.191.0.0/16'),
  ('prod-asia-1', '130.211.0.0/22'),
  ('prod-eu-1',   '35.191.0.0/16'),
  ('prod-eu-1',   '130.211.0.0/22');
