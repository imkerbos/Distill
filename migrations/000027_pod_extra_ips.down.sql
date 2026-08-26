-- 回滚：删掉额外地址列。回滚之后双栈 Pod 只剩主地址，走另一个协议族的连接
-- 会退回"解不出主体"——那是一次能力回退，不是数据损坏。
ALTER TABLE observed_pod DROP COLUMN extra_ips;
