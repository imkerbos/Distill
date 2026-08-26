-- 回滚：删掉覆盖范围表与完整度列。回滚之后精确降级不再可用，判定退回
-- "有第二平面就整片降级"——那是一次能力回退，不是数据损坏，且方向安全。
ALTER TABLE collection_run DROP COLUMN foreign_scopes_complete;
DROP TABLE IF EXISTS collection_foreign_scope;
