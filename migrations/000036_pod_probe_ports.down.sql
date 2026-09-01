-- 没有别的表引用这一列，直接删。
ALTER TABLE observed_pod
  DROP COLUMN probe_ports;
