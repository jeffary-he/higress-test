# Higress 租户/用户灰度白名单插件 0.6.5

执行链：后端 API 请求 → gray-whitelist 解码 JWT Payload → 提取配置的 tenantId/id 字段 → 匹配本地用户白名单 → 写入后端路由请求头 `x-gray-user`，并在灰度状态变化时写入前端灰度 Cookie。默认 JWT Cookie 字段名称为 `PC_AUTH_TOKEN`。

插件直接读取 Cookie 中的 `PC_AUTH_TOKEN`，只解码 JWT Payload，不验证签名、issuer、iat 或 exp。按当前业务约定，用户伪造身份进入灰度可以接受，因此该结果不得用于安全鉴权。

插件生成两个候选项：`tenant:2` 和 `user:283778812672`。任意一个在本地缓存中即写 `x-gray-user: canary`，两者都不在才写 `stable`。插件不信任客户端传入的 `x-gray-user`，每次请求都根据 Cookie 和本地白名单重新计算。
Cookie/JWT/Claim 缺失、同名 Token Cookie 重复、非规范无符号整数、缓存冷启动或过期时均走 stable。客户端传入的 `x-gray-user` 总会被覆盖。

## 当前运行逻辑

该插件只配置在需要灰度的后端 API 路由上。前端静态资源由 Higress 的 Cookie 灰度路由处理。

```text
后端 API 请求
    ↓
读取 PC_AUTH_TOKEN Cookie
    ↓
解析 JWT Payload 中的 tenantId 和 id
    ↓
查询本地白名单缓存
    ↓
生成 canary 或 stable
```

### 后端 API 路由

命中租户或用户白名单时，插件修改当前请求：

```http
x-gray-user: canary
```

未命中、灰度开关关闭、缓存未加载或缓存过期时：

```http
x-gray-user: stable
```

后端灰度路由示例：

```yaml
higress.io/canary: "true"
higress.io/canary-by-header: "x-gray-user"
higress.io/canary-by-header-value: "canary"
```

### 前端静态资源路由

当后端 API 判断用户为灰度用户，并且请求中还没有：

```http
Cookie: x-gray-user=always
```

插件在响应中写入：

```http
Set-Cookie: x-gray-user=always; Path=/; Secure; HttpOnly; SameSite=Lax
x-frontend-gray-refresh: true
```

前端读取 `x-frontend-gray-refresh: true` 后刷新页面。浏览器会自动保存 Cookie，后续前端 HTML、JS、CSS 等静态资源请求会自动携带该 Cookie。

插件只在成功的 `2xx` 响应中写入或清理前端灰度 Cookie 和刷新头；登录跳转的 `3xx` 响应、认证失败和其他错误响应保持原样，不参与前端灰度状态同步。

如果前后端跨域，后端还需要暴露刷新响应头：

```http
Access-Control-Expose-Headers: x-frontend-gray-refresh
```

前端灰度路由示例：

```yaml
higress.io/canary: "true"
higress.io/canary-by-cookie: "x-gray-user"
```

Higress Cookie 灰度匹配的值为固定值 `always`。

### 仅状态变化时写入

插件不会每次 API 响应都写入 Cookie：

```text
canary + 没有 x-gray-user=always
→ 写入 Cookie，并返回 x-frontend-gray-refresh: true

canary + 已有 x-gray-user=always
→ 不写 Cookie，不返回刷新头

stable + 已有 x-gray-user=always
→ 清理 Cookie，并返回 x-frontend-gray-refresh: true

stable + 没有灰度 Cookie
→ 不写 Cookie，不返回刷新头
```

因此前端刷新只发生在灰度状态发生变化时，不会形成刷新循环。

## 插件配置

WasmPlugin 外层配置：

插件描述：根据配置的 Cookie 字段名称读取 JWT，从 JWT Payload 的 `tenantId` 或 `id` 判断灰度白名单，并在网关内部写入 `x-gray-user` 请求头，由 Higress 根据该请求头将请求路由到灰度或正常服务。

```yaml
url: oci://YOUR_REGISTRY/gray-whitelist-wasm:0.6.5 # Wasm 镜像地址
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
redis_tenant_key: "gray:whitelist:pc:tenantId"
# 用户白名单 Set，成员为纯用户 ID，例如 283778812672。
redis_user_key: "gray:whitelist:pc:userId"

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
# 前端灰度 Cookie 名称，需与 Higress canary-by-cookie 一致。
frontend_gray_cookie_name: "x-gray-user"
# 可选：跨子域共享时填写，例如 .example.com。
frontend_gray_cookie_domain: ""

# 仅当 x-gray-user Cookie 状态发生变化时，插件才返回：
# x-frontend-gray-refresh: true
# 前端读取该响应头后刷新页面；下一次请求不会重复返回。

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
SADD gray:whitelist:pc:tenantId 2
SADD gray:whitelist:pc:userId 283778812672
```

### 灰度开关配置

灰度开关是 Redis String 类型，Key 必须与 `gray_enabled_key` 配置一致，值只能是字符串 `1` 或 `0`：

```bash
# 开启灰度
SET gray:whitelist:pc:enabled 1

# 关闭灰度
SET gray:whitelist:pc:enabled 0

# 查看开关值
GET gray:whitelist:pc:enabled
```

正常情况下 `GET` 应返回：

```text
1
```

如果日志出现 `invalid gray enabled switch`，请检查：

```bash
TYPE gray:whitelist:pc:enabled
GET gray:whitelist:pc:enabled
```

`TYPE` 必须返回 `string`，`GET` 必须返回 `1` 或 `0`。不能写成 `true`、`false`、`on`、`off`，也不能使用 `SADD` 写入开关 Key。Key 不存在或值为空时，插件会保留上一次状态；如果从未成功加载，则按安全策略关闭灰度，所有请求走 `stable`。

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

IMAGE="harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.6.5"
docker build --no-cache -t "$IMAGE" .
docker push "$IMAGE"
```

镜像 URL：

```text
oci://harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.6.5
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

## 前后端一体 Demo

完整的前端 stable/canary、后端 API stable/canary、Higress 路由和 Redis 初始化示例见：[demo/README.md](demo/README.md)。
