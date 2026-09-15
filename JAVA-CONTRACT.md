# Java 管理端对接约定

Java 服务和管理页面由管理端项目实现，本项目只提供 Higress 插件及摘要计算示例。

## 数据契约

- 数据库：示例 DB 15，必须与插件一致。
- Key：`gray:whitelist:token-sha256:v1`，类型 Redis Set。
- Member：Token 的 SHA-256，UTF-8 编码，64 位小写十六进制。
- 输入可为裸 Token 或 Authorization 完整值。只移除一次精确的 `Bearer ` 前缀，不转小写、不 trim，不修改剩余字符。
- 空 Token 拒绝；`bearer abc` 不会被当作 `Bearer abc` 处理。
- 示例实现：`examples/TokenDigest.java`。
- `abc` 和 `Bearer abc` 的结果均为：
  `ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad`。

这是 Token 白名单，不是用户 ID 白名单。用户换发 Token 后需要重新加入；同一用户多个 Token 需要分别管理。
摘要不是 JWT 验签，不能判断 Token 过期或撤销。业务鉴权必须独立完成。

## 建议管理接口（插件不提供这些接口）

| 接口 | 职责 |
|---|---|
| POST /gray-whitelist | 接收 Token，归一化、计算摘要、加入 Set |
| DELETE /gray-whitelist/{digest} | 按摘要删除成员 |
| GET /gray-whitelist | 分页展示摘要和管理端保存的备注 |

原始 Token 只用于计算摘要，不持久化、不进入访问日志/审计正文，也不要放 URL。
管理接口必须鉴权、授权、审计，并避免前端泄漏 Token。管理元数据可放 Java 数据库，不影响 Redis Set 格式。

## Redis 操作

下列仅是命令示意，不含连接凭据：

```text
SADD gray:whitelist:token-sha256:v1 <digest>
SREM gray:whitelist:token-sha256:v1 <digest>
SMEMBERS gray:whitelist:token-sha256:v1
```

Java 必须限制成员数量不超过插件 max_entries；并发新增时用 Redis Lua 等原子方式检查容量并写入，不能仅在应用层先 SCARD 再 SADD。
批量替换应在同一数据库先写临时 Set 再原子 RENAME；空列表直接 DEL 正式 Key。不要先删正式 Key 再逐条写入，以免插件读到中间状态。
临时 Key 必须唯一；若以后迁移到 Redis Cluster，须另外设计同槽 Key 和数据库配置。
Key 不建议设置自动过期，否则过期会被视为空白名单。单成员到期由 Java 侧调度删除，Redis Set 不提供此处需要的成员 TTL。

插件使用只读 Redis 身份；Java 使用单独的写入身份。插件需允许认证、选择目标数据库以及 SMEMBERS，具体 ACL 按运行环境验证。
插件不会写回 Redis，也不提供手动刷新接口；Java 返回写入成功不等于所有网关已经生效。

## 一致性与故障

默认每 10 秒同步，正常情况预计在下一次成功刷新后生效；不是立即全局生效。
各 Wasm 实例独立刷新，短暂不一致是预期行为。
Redis 出错保留上次完整有效名单，距离上次成功刷新满 60 秒后走 stable；恢复成功后重新生效。
Redis 返回空 Set/Key 不存在视为有效空名单，立即清空本地名单。
发现非法摘要或超限时整次更新被拒绝，旧缓存 TTL 不延长。
管理页面建议提示“已写入，等待网关同步”，不要将其显示为“全部网关已生效”。
