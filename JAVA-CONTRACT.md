# Java 管理端用户白名单契约

Java 管理端维护灰度用户。gray-whitelist 插件从 PC_AUTH_TOKEN 的 JWT Payload 读取 tenantId/id，并与 Redis 本地缓存比较。

按当前业务决定，插件不验证 JWT 签名、issuer、iat 或 exp；这些字段只用于灰度分流，不能作为安全鉴权依据。

## Redis 数据格式

- 数据库：示例 DB 15，必须与插件一致。
- Key：`gray:whitelist:user:v1`。
- 类型：Redis Set。
- Member 有两种：`tenant:<tenantId>` 或 `user:<userId>`，ID 必须是无符号十进制整数的规范字符串。
- 不允许空值、负数、前导零、空格或附加字段。

例如 JWT Claim：

```json
{"tenantId":2,"id":283778812672}
```

可以按租户加入全部用户：

```text
tenant:2
```

也可以只加入指定用户：

```text
user:283778812672
```

两者是 OR 关系；任意一个存在即进入灰度。

命令示意：

```text
SADD gray:whitelist:user:v1 tenant:2
SADD gray:whitelist:user:v1 user:283778812672
SREM gray:whitelist:user:v1 tenant:2
SREM gray:whitelist:user:v1 user:283778812672
SMEMBERS gray:whitelist:user:v1
```

管理接口建议明确区分租户白名单和用户白名单，在 Java 中验证 ID 后添加对应前缀。不要从前端接收任意 Redis Member 字符串。
Java 必须限制成员数量不超过插件 max_entries；并发新增时使用 Lua 等原子方式检查容量并写入。
批量替换应先写唯一临时 Set，校验后原子 RENAME；空列表直接 DEL 正式 Key，避免插件读到半成品。

插件使用只读 Redis 身份；Java 管理端使用独立写入身份。临时连通性测试开启时，插件身份还需对测试 Key 具有 INCR 权限，测试后应关闭并恢复只读权限。

默认每 10 秒同步。Java 写入成功表示 Redis 已更新，不代表所有 Gateway 已立即同步；管理页面应显示“等待网关同步”。
Redis 刷新失败时插件保留旧快照但不续期，上次成功同步满 60 秒后全部走 stable。
