-- 回滚：删掉样本表。回滚之后分歧率仍然算得出、门禁照常拦，只是拦下来之后
-- 给不出可下钻的证据 —— 那是一次能力回退，不是数据损坏。
DROP TABLE IF EXISTS reconciliation_sample;
