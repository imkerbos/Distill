# 平台侧 Kubernetes 部署清单

**这些是模板，不是可以直接 apply 的成品。** 所有 `REPLACE_ME` 都必须换掉。
完整说明见 [../../docs/deployment.md](../../docs/deployment.md)。

## 前置

- 一个 MySQL 8.4（集群内或托管实例）。schema 不需要手工初始化 ——
  `distill-api` 启动时自己跑迁移。
- 一个镜像仓库，里面有 `distill-api` 与（需要拉取式采集时）`distill-collector`。

## 顺序

```bash
kubectl apply -f 00-namespace.yaml
kubectl apply -f 10-serviceaccount.yaml
kubectl apply -f 20-configmap.yaml

# 凭据不进 Git。30-secret.example.yaml 只是字段说明。
kubectl -n distill create secret generic distill-api-secrets \
  --from-literal=DISTILL_DATABASE__DSN='...' \
  --from-literal=DISTILL_AUTH__BOOTSTRAP_USER__USERNAME='admin' \
  --from-literal=DISTILL_AUTH__BOOTSTRAP_USER__PASSWORD_HASH='$2a$10$...'

# 服务端证书。集群内那一跳也要 TLS —— agent 只接受 https 地址。
# 自签就够：不需要公网、不需要公共 CA，SAN 写 Service 的域名。
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout tls.key -out tls.crt \
  -subj "/CN=distill-api.distill.svc" \
  -addext "subjectAltName=DNS:distill-api.distill.svc,DNS:distill-api.distill.svc.cluster.local"
kubectl -n distill create secret tls distill-api-tls --cert=tls.crt --key=tls.key
# tls.crt 同时就是 agent 那边 -ca-file 要信任的那份（自签时证书即 CA）。

kubectl apply -f 40-deployment-api.yaml -f 50-service.yaml
kubectl apply -f 60-networkpolicy.yaml     # 先按注释改 namespace 标签与 DB 位置
kubectl apply -f ingress.example.yaml      # 改完再 apply，必须是 https
```

拉取式采集按需跑：

```bash
kubectl -n distill create -f 70-job-collector.example.yaml
```

## 三条不要改的

- **`replicas: 1`。** 会话与限流计数都在进程内存里，第二个副本会让人随机掉登录。
- **不给 `distill-api` 任何 RBAC。** 它不读写 Kubernetes 对象，加 Role 是设计错误。
- **平台自己就监听 https。** 证书由 `distill-api-tls` 这个 Secret 提供，路径写在
  ConfigMap 里。两项只给一半会在启动时被拒 —— 它最坏的落法是静默退回明文监听。
  不想经过 Ingress 时，agent 直接指 `https://distill-api.distill.svc:10100` 即可。
- **要对外暴露时，Ingress 也必须是 https。** 只做分析、不对外时用
  `kubectl -n distill port-forward svc/distill-api 10100:10100`，省掉整套证书签发与
  暴露面 —— 但注意本机访问要用 `https://localhost:10100`，且证书 SAN 里没有
  localhost，浏览器会告警（`curl -k` 或把 localhost 加进 SAN）。
