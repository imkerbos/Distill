-- 集群实际跑着的 CNI（design doc 2026-08-25-existing-policies §2.2）。
--
-- 本文件是新增的第 29 号迁移，不改动 000001–000028 中任何一条已应用的迁移。
--
-- **这是一个事实，不是一个判断。** 平台记下"这个集群跑着 Cilium"，
-- 而**不**记"Cilium 执不执行 ANP"—— 后者会变成一张随版本过时的表，
-- 而过时的那天没有任何东西会报错（CLAUDE.md：不得硬编码常量表）。
--
-- 它存在的理由：第二策略平面（CNP / ANP / Calico 私有策略）是否真的生效，
-- 取决于 CNI。实测（2026-08-26）：原生 Calico v3.30.4 执行 ANP，
-- Cilium 1.19.5 完全不实现它。把 CNI 呈现出来，读的人才判断得了那些对象
-- 是不是活的。平台自己照旧走保守路线：探测到第二平面就降级，不管 CNI 是什么。
--
-- 默认 UNKNOWN 而不是空串：**认不出不等于没有 CNI**，每个集群都有。
-- 与 other_planes 同一条纪律。
ALTER TABLE cluster
  ADD COLUMN cni VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN';
