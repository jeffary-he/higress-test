# Java 管理端灰度配置契约 0.7.0

## 双模式控制

管理端提供互斥的 whitelist（租户/用户白名单）和 traffic（完整 Token Hash 比例）模式。
白名单增删改查独立于开关和当前模式；关闭时也可维护。traffic 模式不使用白名单。

| Redis Key（以插件配置为准） | 类型 | 内容 |
| --- | --- | --- |
| gray:whitelist:pc:enabled | String | `0` / `1` |
| gray:whitelist:pc:mode | String | `whitelist` / `traffic` |
| gray:whitelist:pc:percent | String | 规范整数 `0`～`100` |
| gray:whitelist:pc:tenantId | Set | 纯租户 ID |
| gray:whitelist:pc:userId | Set | 纯用户 ID |

模式 Key 缺失兼容为 whitelist；显式空值或未知模式属于错误。traffic 的比例缺失或非法会拒绝本轮刷新。
管理端切换 traffic 时应在一个 Redis 事务内写入模式和比例，例如在 Redis CLI 的同一连接执行：

```text
MULTI
SET gray:whitelist:pc:percent 10
SET gray:whitelist:pc:mode traffic
SET gray:whitelist:pc:enabled 1
EXEC
```

切回 whitelist 使用 `SET gray:whitelist:pc:mode whitelist`；关闭使用 `SET gray:whitelist:pc:enabled 0`。
关闭不删除白名单；不使用的比例和名单可保留在 Redis。

插件条件读取后发布整套本地快照，失败保留旧快照至 TTL；不会逐字段切换。
管理端“保存成功”表示 Redis 写入成功，不代表全部 Gateway 已同步。展示“等待同步”提示；
不要把短暂的旧模式误报为新模式已生效。多 Pod/工作实例各自刷新。
Redis 事务保证写入原子性，但插件的分次读取可能跨越另一次修改。0.7.0 没有配置版本校验；
管理端应限制连续发布频率。如要求严格跨字段同版本读取，需要后续版本引入版本号校验或事务读取。

流量算法：完整 Token UTF-8 → SHA-256 → 前 8 字节大端无符号 uint64 → `% 100`。
槽位小于 percent 时 canary；否则 stable。Java 如需预览，应使用 `Long.remainderUnsigned(value, 100)`，
避免 Java 有符号 long 导致结果不一致。校验向量：Token `abc` 的槽位为 74，74% 为 stable，75% 为 canary。
不要记录原始 Token。比例表示约定比例的 Token 分组；同一用户多个 Token 可能不同，实际请求占比不保证。
此模式不解析或验证 JWT，缺失、空、重复或超长 Token 走 stable，认证仍由原有业务鉴权负责。

页面新增模式选择和比例输入；whitelist 显示名单配置，traffic 显示 0～100 整数比例。
操作审计记录模式和比例的前后值。Header、前端 Cookie 和刷新头协议沿用现有规则。

以下为白名单模式契约。gray-whitelist 插件从 PC_AUTH_TOKEN 的 JWT Payload 读取 tenantId/id，并与 Redis 本地缓存比较。

按当前业务决定，插件不验证 JWT 签名、issuer、iat 或 exp；这些字段只用于灰度分流，不能作为安全鉴权依据。

## Redis 数据格式

- 数据库：示例 DB 15，必须与插件一致。
- 灰度开关 Key：`gray:whitelist:pc:enabled`，String 值 `1` 表示开启，`0` 表示关闭。
- 租户 Key：`gray:whitelist:pc:tenantId`；用户 ID Key：`gray:whitelist:pc:userId`。
- 类型：Redis Set。
- 租户 Set Member 是纯 `tenantId`，用户 Set Member 是纯 `id`，必须是无符号十进制整数的规范字符串。
- 不允许空值、负数、前导零、空格或附加字段。

例如 JWT Claim：

```json
{"tenantId":2,"id":283778812672}
```

开启灰度并加入租户白名单：

```text
SET gray:whitelist:pc:enabled 1
SADD gray:whitelist:pc:tenantId 2
```

也可以只加入指定用户：

```text
SADD gray:whitelist:pc:userId 283778812672
```

两者是 OR 关系；任意一个存在即进入灰度。

命令示意：

```text
SADD gray:whitelist:pc:tenantId 2
SADD gray:whitelist:pc:userId 283778812672
SREM gray:whitelist:pc:tenantId 2
SREM gray:whitelist:pc:userId 283778812672
SMEMBERS gray:whitelist:pc:tenantId
SMEMBERS gray:whitelist:pc:userId
```

管理接口明确区分租户白名单和用户白名单，在 Java 中验证 ID 后写入纯 ID，不添加 tenant: 或 user: 前缀。不要从前端接收任意 Redis Member 字符串。
Java 必须限制成员数量不超过插件 max_entries；并发新增时使用 Lua 等原子方式检查容量并写入。
批量替换应先写唯一临时 Set，校验后原子 RENAME；空列表直接 DEL 正式 Key，避免插件读到半成品。

插件使用只读 Redis 身份；Java 管理端使用独立写入身份。临时连通性测试开启时，插件身份还需对测试 Key 具有 INCR 权限，测试后应关闭并恢复只读权限。

## Java 灰度 Web 管理端设计

Java 管理端负责提供灰度白名单的 Web 管理页面和后端接口。Wasm 插件只负责从 Redis 读取灰度配置和白名单，并在 Higress 网关内部执行灰度判断。

### 页面功能

- 灰度总开关：展示当前状态，支持开启和关闭灰度。
- 租户白名单管理：添加、查询和删除租户 ID。
- 用户 ID 白名单管理：添加、查询和删除用户 ID。
- 白名单搜索：支持按租户 ID 或用户 ID 查询。
- 当前灰度状态：展示灰度开关、租户白名单数量和用户白名单数量。
- 操作记录：记录操作人、操作时间、操作类型和操作内容。

### 页面结构

```text
PC 灰度管理
├── 灰度开关：开启 / 关闭
├── 租户白名单
│   ├── 租户 ID
│   ├── 添加
│   ├── 查询
│   └── 删除
└── 用户 ID 白名单
    ├── 用户 ID
    ├── 添加
    ├── 查询
    └── 删除
```

### Java 后端职责

- 校验租户 ID 和用户 ID，拒绝空值、负数、前导零及非法字符。
- 灰度开关写入 `gray:whitelist:pc:enabled`，值只能是 `1` 或 `0`。
- 租户白名单写入 `gray:whitelist:pc:tenantId`。
- 用户 ID 白名单写入 `gray:whitelist:pc:userId`。
- Java 管理端使用独立 Redis 写入账号，Wasm 插件使用只读 Redis 账号。
- 添加、删除和开关变更必须记录操作日志，便于审计和问题排查。

### 推荐接口

```text
GET  /api/gray/pc/status              查询灰度开关和白名单统计
PUT  /api/gray/pc/enabled             修改灰度开关
GET  /api/gray/pc/tenants              查询租户白名单
POST /api/gray/pc/tenants              添加租户 ID
DELETE /api/gray/pc/tenants/{tenantId} 删除租户 ID
GET  /api/gray/pc/ids                  查询用户 ID 白名单
POST /api/gray/pc/ids                  添加用户 ID
DELETE /api/gray/pc/ids/{id}            删除用户 ID
```

默认每 10 秒同步。Java 写入成功表示 Redis 已更新，不代表所有 Gateway 已立即同步；管理页面应显示“等待网关同步”。
Redis 刷新失败时插件保留旧快照但不续期，上次成功同步满 60 秒后全部走 stable。
