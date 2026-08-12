-- 删除顺序与外键依赖相反：policy_import 与 cluster_git_binding 都引用
-- cluster，先删子表再删父行。少了这两条，只要有人往种子集群里导入过一条
-- 策略，回滚就会撞上外键失败 —— 而回滚脚本第一次真正被执行的那一天，
-- 恰恰是最不能失败的时候。
DELETE FROM policy_import WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
DELETE FROM cluster_git_binding WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
DELETE FROM cluster_health_check_source WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
DELETE FROM cluster_apiserver WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
DELETE FROM cluster WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
