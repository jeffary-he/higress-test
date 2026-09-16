# Higress 租户/用户灰度白名单插件 0.6.3

执行链：配置的 Cookie 字段名称 → gray-whitelist 解码 JWT Payload → 提取配置的 tenantId/id 字段 → 匹配本地用户白名单 → 网关内部写入请求头 x-gray-user → Higress 路由。默认 Cookie 字段名称为 `PC_AUTH_TOKEN`，前端不需要处理灰度请求头。

插件直接读取 Cookie 中的 `PC_AUTH_TOKEN`，只解码 JWT Payload，不验证签名、issuer、iat 或 exp。按当前业务约定，用户伪造身份进入灰度可以接受，因此该结果不得用于安全鉴权。

插件生成两个候选项：`tenant:2` 和 `user:283778812672`。任意一个在本地缓存中即写 `x-gray-user: canary`，两者都不在才写 `stable`。插件不信任客户端传入的 `x-gray-user`，每次请求都根据 Cookie 和本地白名单重新计算。
Cookie/JWT/Claim 缺失、同名 Token Cookie 重复、非规范无符号整数、缓存冷启动或过期时均走 stable。客户端传入的 x-gray-user 总会被覆盖。

## 插件配置

WasmPlugin 外层配置：

```yaml
url: oci://YOUR_REGISTRY/gray-whitelist-wasm:0.6.3 # Wasm 镜像地址
phase: AUTHN                                      # 认证阶段，先处理请求头再参与路由
priority: 100                                     # 同阶段优先级，数字越小越优先
```

```yaml
# ==================== Redis 连接配置 ====================
# Envoy 中定义的 Redis 上游 Cluster 名称。
redis_cluster: "outbound|6379||aws-redis.dns"
# Redis 数据库编号，取值范围 0-15。
redis_database: 15
# Redis 用户名；未启用 ACL 时可留空。
redis_username: "saas"
# Redis 密码；生产环境建议通过 Secret 注入。
redis_password: "REPLACE_WITH_REDIS_PASSWORD"
# Redis 请求超时时间，单位毫秒。
redis_timeout_ms: 1000

# ==================== Redis Key 配置 ====================
# 灰度总开关：1 开启，0 关闭。
gray_enabled_key: "gray:whitelist:pc:enabled"
# 租户白名单 Set，成员为纯租户 ID，例如 2。
redis_tenant_key: "gray:whitelist:pc:tenant"
# 用户白名单 Set，成员为纯用户 ID，例如 283778812672。
redis_user_key: "gray:whitelist:pc:id"

# ==================== JWT 配置 ====================
# Cookie 中保存 JWT 的字段名。
token_cookie_name: "PC_AUTH_TOKEN"
# JWT Payload 中的租户 ID 字段名。
tenant_id_claim: "tenantId"
# JWT Payload 中的用户 ID 字段名。
user_id_claim: "id"

# ==================== 本地缓存配置 ====================
# 从 Redis 同步开关和白名单的间隔，单位毫秒。
refresh_interval_ms: 10000
# 本地缓存最长有效时间，单位毫秒；超过后默认 stable。
cache_ttl_ms: 60000
# 单个白名单 Set 允许的最大成员数量。
max_entries: 10000

# ==================== 请求头行为配置 ====================
# true 表示向客户端返回 x-gray-user 响应头，供前端读取。
response_header_enabled: true

# ==================== Redis 连通性测试 ====================
# 仅用于临时测试 Redis 写入连通性，正常运行必须关闭。
connectivity_test_enabled: false
# 连通性测试执行 INCR 的 Redis Key。
connectivity_test_key: "gray:whitelist:pc:connectivity-test"
# 连通性测试间隔，单位毫秒。
connectivity_test_interval_ms: 60000
```

租户 Set 成员是纯租户 ID，用户 Set 成员是纯用户 ID。两类名单是 OR 关系：

```text
SADD gray:whitelist:pc:tenant 2
SADD gray:whitelist:pc:id 283778812672
```

插件每 10 秒分别对两个 Key 执行全量 SMEMBERS，完整校验后更新每个 Wasm 实例的本地快照。请求不访问 Redis。
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

IMAGE="harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.6.3"
docker build --no-cache -t "$IMAGE" .
docker push "$IMAGE"
```

镜像 URL：

```text
oci://harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.6.3
```

## 验证

插件会在网关内部写入请求头，并将结果通过响应头 `x-gray-user` 返回给前端。前端可读取该结果用于页面状态或后续业务逻辑；灰度路由判断仍以网关根据 Cookie 和本地白名单重新计算的结果为准。

路由规则应匹配请求头：

```yaml
headers:
  x-gray-user:
    exact: canary
```

查看同步日志：

```bash
kubectl logs -n higress-system -l app=higress-gateway \
  --all-containers=true --prefix --since=5m | grep 'gray-whitelist:'
```

成功日志：`gray-whitelist: refresh succeeded; entries=N`。
将测试用户加入 Redis，等待 10–15 秒，携带包含对应 Claim 的 Cookie 请求；应进入 canary。删除成员并等待同步后应进入 stable。
Java 管理端数据约定见 JAVA-CONTRACT.md。
