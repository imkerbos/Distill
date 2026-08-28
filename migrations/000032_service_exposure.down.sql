-- 三列都没有被别的表引用，直接删。
ALTER TABLE observed_service
  DROP COLUMN lb_ingress_ips,
  DROP COLUMN lb_source_ranges;

ALTER TABLE observed_pod
  DROP COLUMN named_ports;
