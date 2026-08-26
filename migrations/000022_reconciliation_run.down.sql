-- 回滚：删掉两张对账表。先删子表 —— 外键指向 reconciliation_run。
DROP TABLE IF EXISTS reconciliation_subject;
DROP TABLE IF EXISTS reconciliation_run;
