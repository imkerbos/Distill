-- 回滚三张资产表的 (cluster_id, observed_at) 索引。
--
-- **一行数据都不丢**：索引是同一批行的另一种排法，删掉之后查询仍然答得出
-- 同样的结果，只是要多读该集群其余各代再丢掉 —— 与 000014 那种「丢掉的是
-- 登记不是观测」相比，这一版连登记都不丢。因此它是本仓库里回滚代价最小的
-- 一版：唯一的后果是慢，而慢是看得见的。
--
-- 顺序与 up 相反不是必需的（三条互不依赖），写成相反只是让两个文件读起来
-- 是一对镜像，省掉「这两条是不是漏了一条」这个问题。
DROP INDEX idx_gateway_at ON observed_gateway;
DROP INDEX idx_endpoints_at ON observed_endpoints;
DROP INDEX idx_service_at ON observed_service;
