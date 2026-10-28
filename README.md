# Higress 租户/用户灰度插件 0.7.3

插件在 Higress 中定时读取 Redis 灰度配置，并使用本地快照处理符合条件的 API 请求。支持白名单和流量两种模式，通过 `x-gray-user` 请求头参与后端路由，通过前端灰度 Cookie 和刷新响应头同步前端状态。

## 请求入口条件

请求必须同时满足两个条件，才进入灰度开关、白名单或流量模式判断：路径命中 `gateway_include_path_prefixes`，并且 Cookie 中存在 `jwt_token_cookie_name` 指定的唯一、非空 Token。

在域名级配置一套插件：

```yaml
gateway_include_path_prefixes:
  - /api/
jwt_token_cookie_name: "PC_AUTH_TOKEN"
```

- 两个条件为 AND 关系，适用于 whitelist 和 traffic 两种模式；不满足任一条件就跳过请求和响应处理。
- Token Cookie 缺失、值为空或同名重复时跳过；只有字段名、没有值的 `PC_AUTH_TOKEN=` 也跳过。此入口检查不验证 JWT 签名或登录状态。
- 默认包含列表为空，跳过全部请求。部署时必须配置需要处理的路径；需要全路径处理时显式配置 `/`。
- 纯字符串前缀匹配，区分大小写，忽略查询参数，不支持 `*`；多个前缀命中任意一个即可。Cookie 字段名称也区分大小写。
- 使用请求头回调实际可见的路径；如果更早的过滤器已重写路径，配置必须匹配重写后的可见路径。
- 在解析身份、灰度计算或修改 Header 前检查两个条件，并保存本次决定；响应阶段不重新判断路径和 Token。无 Token 的登录请求即使在响应中创建了 Token，本次响应仍跳过，后续携带 Token 的 API 请求才参与判断。
- 跳过时不修改请求 Header、Cookie、响应头或上游 Set-Cookie；不是拒绝请求，也不强制转到正式环境。
- Redis 定时刷新不受入口条件影响；请求处理不新增 Redis 调用。
- 建议域名级使用一份配置。部署文件中的 `defaultConfig` 本身不限定域名，需通过控制台域名级配置或对应 matchRules 限定域名作用域。

| 请求情况 | 行为 |
| --- | --- |
| `/api/users` + 唯一非空 `PC_AUTH_TOKEN` | 进入灰度开关和模式判断 |
| `/api/users` + 没有 Token 或 `PC_AUTH_TOKEN=` | 跳过，不清理已有前端灰度 Cookie |
| `/api/users` + 重复同名 Token Cookie | 跳过 |
| `/index.html` + 有 Token | 跳过 |
| `/api/internal/v1/pc/login/actions/exchangeToken` 或 `/api/internal/v1/pc/logout` + 唯一非空 Token | 同样进入灰度判断 |

插件只通过包含路径和 Token 两个条件决定是否参与处理，不按接口名称排除请求。业务退出流程负责清理登录身份和灰度 Cookie；插件不识别退出接口语义。

## 灰度模式

灰度总开关与模式由 Redis 下发，通过两个入口条件的请求只使用本地完整快照进行判断。

### 白名单模式 whitelist

读取 `jwt_token_cookie_name` 指定的 Token，解码 JWT Payload，并根据 `jwt_tenant_id_claim`、`jwt_user_id_claim` 提取租户和用户 ID。默认字段为 `PC_AUTH_TOKEN`、`tenantId`、`id`。

插件生成两个候选项，例如 `tenant:2` 和 `user:283778812672`。租户或用户任意一个命中本地名单，即判定 canary；两者均未命中则为 stable。Redis Set 中保存纯数字 ID，内部候选项前缀由插件添加。

JWT 无法解码、没有可匹配的 Claim、ID 非规范无符号整数或 Token 超过 16384 字节时判定 stable。Token 缺失、为空或同名重复的请求已经在入口跳过。

插件只解码 JWT Payload，不验证签名、issuer、iat 或 exp；灰度结果只用于分流，业务接口继续执行原有身份认证。

### 流量模式 traffic

流量模式不解析 JWT，直接读取 `jwt_token_cookie_name` 指定的完整 Token：

```text
SHA-256(完整 Token UTF-8 字节)
→ 取摘要前 8 字节，按大端无符号 uint64 解释
→ slot = 数值 % 100
→ slot < percent 时 canary，否则 stable
```

`percent` 为 0～100 的规范十进制整数。0 全部 stable；100 使符合输入条件的 Token 全部 canary。
Token 缺失、为空或同名重复时已在入口跳过。通过入口的 Token 超过 16384 字节时仍判定 stable。此模式不验证 JWT 或认证有效性。
比例是约定比例的 Token 分组，不保证实际请求数量占比。Token 不变则结果稳定，Token 更新可能重新分组。
分桶不使用批次或额外的分配 Cookie。两种模式共用下文的 Header、前端 Cookie 和刷新头行为。

## Redis 同步与本地缓存

插件通过 SDK RedisClient 定时读取配置。每个 Wasm 实例独立维护一份本地快照，请求处理不访问 Redis。

| 总开关 / 模式 | 本轮 Redis 读取 |
| --- | --- |
| 关闭 `0` | 只 GET 开关；成功后立即发布关闭状态 |
| 开启 `1`，`whitelist` | GET 开关 → GET 模式 → SMEMBERS 租户 → SMEMBERS 用户 |
| 开启 `1`，`traffic` | GET 开关 → GET 模式 → GET 比例 |

模式 Key 不存在时默认 `whitelist`。模式为空或未知、traffic 比例缺失或非法时，本轮更新失败。白名单成员非法或单个 Set 超过 `gray_max_entries` 时，也会拒绝整套新快照；完整刷新成功时，空 Set 会清空对应本地名单。

只有该模式需要的数据全部成功读取并校验后，才一次性替换本地快照。模式切换准备期间继续使用旧快照；失败不续期。超过 `gateway_cache_ttl_seconds` 后，参与处理的请求走 stable，首次加载失败也如此。超时旧回调不能覆盖新一轮配置。

默认刷新间隔为 10 秒，TTL 为 60 秒；部署值以配置为准。正常同步延迟约为等待下次刷新加该分支的 Redis 读取时间。多个工作实例和 Pod 独立同步，不保证同一时刻切换。

本地整体替换不等于 Redis 多次读取的事务快照。Java 管理端职责和写入约定见 [JAVA-CONTRACT.md](JAVA-CONTRACT.md)。

临时设置 `redis_connectivity_test_enabled: true` 后，每个 Wasm 实例按周期对测试 Key 执行 INCR 并记录 count。测试 Key 应与灰度配置和名单 Key 分开；多 Pod/工作实例会分别递增。验证完成后关闭测试并删除测试 Key。

## 当前运行逻辑

建议按域名配置插件，用包含路径和 Token 两个条件限定 API 请求。前端静态资源由 Higress 的 Cookie 灰度路由处理。

```text
后端 API 请求
    ↓
路径命中包含前缀 AND 存在唯一非空 Token
    ↓
读取本地灰度开关，关闭或无有效快照时 stable
    ↓
开启时按 whitelist 匹配租户/用户，或按 traffic 对完整 Token 分桶
    ↓
生成 canary 或 stable
```

### 后端 API 路由

命中租户/用户白名单，或流量分桶命中比例时，插件修改当前请求：

```http
x-gray-user: canary
```

对于通过两个入口条件的请求，未命中、灰度开关关闭、缓存未加载或缓存过期时：

```http
x-gray-user: stable
```

后端灰度路由示例：

```yaml
higress.io/canary: "true"
higress.io/canary-by-header: "x-gray-user"
higress.io/canary-by-header-value: "canary"
```

参与处理的请求会覆盖客户端传入的 `x-gray-user`，由本次本地判断生成结果。跳过插件的请求保留原始 Header。

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

此处判断的是请求携带的 Cookie 与本次灰度结果是否一致。并发请求可能都携带旧 Cookie，因此刷新通知不保证只出现一次。

## 插件配置

WasmPlugin 外层配置：

插件描述：路径命中包含前缀且携带指定的唯一非空 Token Cookie 时，按白名单或流量模式判断灰度，在网关内部写入 `x-gray-user` 请求头，并按请求 Cookie 差异同步前端灰度状态。

```yaml
url: oci://YOUR_REGISTRY/gray-whitelist-wasm:0.7.3 # Wasm 镜像地址
phase: AUTHN                                      # 认证阶段，先处理请求头再参与路由
priority: 100                                     # 同阶段优先级，数字越大越优先
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
# 模式 String：whitelist / traffic；Key 不存在时默认 whitelist。
gray_mode_key: "gray:whitelist:pc:mode"
# 比例 String：0～100 整数，仅 traffic 模式读取。
gray_percent_key: "gray:whitelist:pc:percent"
# 租户白名单 Set，成员为纯租户 ID，例如 2。
gray_tenant_key: "gray:whitelist:pc:tenantId"
# 用户白名单 Set，成员为纯用户 ID，例如 283778812672。
gray_user_key: "gray:whitelist:pc:userId"

# ==================== JWT 配置 ====================
# 入口条件之一：Cookie 中必须存在此名称的唯一、非空 Token。
jwt_token_cookie_name: "PC_AUTH_TOKEN"
# JWT Payload 中的租户 ID 字段名。
jwt_tenant_id_claim: "tenantId"
# JWT Payload 中的用户 ID 字段名。
jwt_user_id_claim: "id"

# ==================== 本地缓存配置 ====================
# 入口条件之一：命中任意包含前缀；同时须携带上述 Token 才参与处理。
gateway_include_path_prefixes:
  - /api/
# 从 Redis 同步开关和白名单的间隔，单位秒。
gateway_refresh_interval_seconds: 10
# 本地缓存最长有效时间，单位秒；超过后默认 stable。
gateway_cache_ttl_seconds: 60
# 单个白名单 Set 允许的最大成员数量。
gray_max_entries: 10000

# ==================== 请求头行为配置 ====================
# true 表示向客户端返回 x-gray-user 响应头，供前端读取。
frontend_response_header_enabled: true
# 前端灰度 Cookie 名称，需与 Higress canary-by-cookie 一致。
frontend_gray_cookie_name: "x-gray-user"
# 可选：跨子域共享时填写，例如 .example.com。
frontend_gray_cookie_domain: ""

# 仅当 x-gray-user Cookie 状态发生变化时，插件才返回：
# x-frontend-gray-refresh: true
# 前端读取该响应头后处理刷新；并发请求可能重复收到通知。

# ==================== Redis 连通性测试 ====================
# 仅用于临时测试 Redis 写入连通性，正常运行必须关闭。
redis_connectivity_test_enabled: false
# 连通性测试执行 INCR 的 Redis Key。
redis_connectivity_test_key: "gray:whitelist:pc:connectivity-test"
# 连通性测试间隔，单位秒。
redis_connectivity_test_interval_seconds: 60
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

如果日志出现 `configuration refresh rejected; retaining previous snapshot until TTL`，可先检查开关的类型和值：

```bash
TYPE gray:whitelist:pc:enabled
GET gray:whitelist:pc:enabled
```

`TYPE` 必须返回 `string`，`GET` 必须返回 `1` 或 `0`。不能写成 `true`、`false`、`on`、`off`，也不能使用 `SADD` 写入开关 Key。Key 不存在或值为空时，插件会保留上一次状态；如果从未成功加载，参与处理的请求走 `stable`。

开关正确时，继续检查当前模式所需的数据：whitelist 检查两个名单 Set，traffic 检查比例 String。同步和失败保留规则见“Redis 同步与本地缓存”。

## 构建发布

直接使用当前环境安装的 Go 版本构建：

```bash
go version
go test ./...
go vet ./...
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o main.wasm ./

IMAGE="harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.7.3"
docker build --no-cache -t "$IMAGE" .
docker push "$IMAGE"
```

镜像 URL：

```text
oci://harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.7.3
```

## 验证

通过两个入口条件的请求，插件会在网关内部写入请求头，并在成功响应中通过 `x-gray-user` 返回结果。前端可读取该结果用于页面状态；灰度判断以网关根据 Token、本地模式及对应数据重新计算的结果为准。

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

成功日志：`gray-whitelist: configuration refreshed; enabled=true mode=whitelist percent=0 tenants=N users=N`。
将测试用户加入 Redis，等待 10–15 秒，携带包含对应 Claim 的 Cookie 请求；应进入 canary。删除成员并等待同步后应进入 stable。

## 前后端一体 Demo

完整的前端 stable/canary、后端 API stable/canary、Higress 路由和 Redis 初始化示例见：[demo/README.md](demo/README.md)。
