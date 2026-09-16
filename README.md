# Higress 租户/用户灰度白名单插件 0.6.0

执行链：PC_AUTH_TOKEN → gray-whitelist 解码 JWT Payload → 提取 tenantId/id → 匹配本地用户白名单 → 写入 x-gray-user → Higress 路由。

插件直接读取 Cookie 中的 `PC_AUTH_TOKEN`，只解码 JWT Payload，不验证签名、issuer、iat 或 exp。按当前业务约定，用户伪造身份进入灰度可以接受，因此该结果不得用于安全鉴权。

插件生成两个候选项：`tenant:2` 和 `user:283778812672`。任意一个在本地缓存中即写 `x-gray-user: canary`，两者都不在才写 `stable`。
Cookie/JWT/Claim 缺失、同名 Token Cookie 重复、非规范无符号整数、缓存冷启动或过期时均走 stable。客户端传入的 x-gray-user 总会被覆盖。

## 插件配置

```yaml
redis_cluster: "outbound|6379||aws-redis.dns"
redis_database: 15
redis_key: "gray:whitelist:user:v1"
redis_username: "saas"
redis_password: "REPLACE_WITH_REDIS_PASSWORD"
redis_timeout_ms: 1000
refresh_interval_ms: 10000
cache_ttl_ms: 60000
max_entries: 10000
token_cookie_name: "PC_AUTH_TOKEN"
tenant_id_claim: "tenantId"
user_id_claim: "id"
connectivity_test_enabled: false
connectivity_test_key: "gray:whitelist:connectivity-test:v1"
connectivity_test_interval_ms: 60000
```

Redis Set 成员必须带类型前缀。两类名单是 OR 关系：

```text
SADD gray:whitelist:user:v1 tenant:2
SADD gray:whitelist:user:v1 user:283778812672
```

插件每 10 秒全量 SMEMBERS，完整校验后原子替换每个 Wasm 实例的本地快照。请求不访问 Redis。
刷新失败保留旧缓存但不续期，60 秒后走 stable；成功读取空 Set 会清空缓存。
非法成员或超过 max_entries 会拒绝整个新快照。

临时设置 `connectivity_test_enabled: true` 后，每个 Wasm 实例按周期对独立测试 Key 执行 INCR 并记录 count；它不修改用户名单。多 Pod/工作实例会分别递增。验证完成后关闭开关并删除测试 Key。

## 构建发布

直接使用当前环境安装的 Go 版本构建：

```bash
go version
go test ./...
go vet ./...
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o main.wasm ./

IMAGE="harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.6.0"
docker build --no-cache -t "$IMAGE" .
docker push "$IMAGE"
```

镜像 URL：

```text
oci://harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.6.0
```

## 验证

查看同步日志：

```bash
kubectl logs -n higress-system -l app=higress-gateway \
  --all-containers=true --prefix --since=5m | grep 'gray-whitelist:'
```

成功日志：`gray-whitelist: refresh succeeded; entries=N`。
将测试用户加入 Redis，等待 10–15 秒，携带包含对应 Claim 的 Cookie 请求；应进入 canary。删除成员并等待同步后应进入 stable。
Java 管理端数据约定见 JAVA-CONTRACT.md。
