-- 回滚：去掉业务周期两列。回滚之后写回门禁失去这一道判断 —— 那是一次
-- 能力回退，不是数据损坏。
ALTER TABLE cluster
  DROP COLUMN business_cycle_reason,
  DROP COLUMN business_cycle_seconds;
