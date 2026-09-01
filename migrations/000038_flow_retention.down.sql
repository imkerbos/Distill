-- 删掉水位表。回滚后读取端不再区分"已清理"与"没有流量"，也就是这次变更
-- 之前的行为；已经被清理掉的连接不会回来。
DROP TABLE IF EXISTS flow_retention;
