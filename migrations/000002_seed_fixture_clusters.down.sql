DELETE FROM cluster_health_check_source WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
DELETE FROM cluster_apiserver WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
DELETE FROM cluster WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
