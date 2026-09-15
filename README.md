# Higress Token 灰度白名单插件 0.2.0

Go 编写的 Wasm 插件；Java 管理端负责名单写入，插件只定时读取 Redis 并标记请求。

管理页面 → Java 接口 → Redis Set → Higress Wasm 本地缓存。
业务请求不查 Redis：读取 Authorization → 摘要匹配本地名单 → 覆盖 x-gray-user → Higress 路由。

## 行为

- 精确去掉一次 `Bearer ` 前缀，保留 Token 大小写及剩余字符，计算 SHA-256。
- 命中输出 `x-gray-user: canary`，未命中、缺失、重复 Authorization、冷启动或缓存过期输出 `stable`。
- 删除客户端传入的所有同名灰度 Header，再写入唯一结果；Authorization 原值保持不变。
- 不验证 JWT，不读取 x-user-id，不提供管理接口，不写 Redis。
- 本插件不能替代鉴权。保证鉴权链正常工作，且前置插件不会在此插件读取前删除 Authorization。

## Redis 与缓存配置

`wasm-plugin.yaml` 提供配置示例，需要替换镜像地址和密码后才能使用。

| 配置 | 示例值 | 含义 |
|---|---|---|
| redis_cluster | outbound\|6379\|\|aws-redis.dns | 完整 Envoy 集群名称，插件不拼接 |
| redis_database | 15 | 白名单所在 DB |
| redis_key | gray:whitelist:token-sha256:v1 | 存放摘要的 Redis Set |
| redis_username | saas | Redis 用户名 |
| redis_password | 占位符 | Redis 密码 |
| redis_timeout_ms | 1000 | Redis 调用超时，毫秒 |
| refresh_interval_ms | 10000 | 定时同步间隔，毫秒 |
| cache_ttl_ms | 60000 | 上次成功同步后缓存可用时间，毫秒 |
| max_entries | 10000 | 接受的最大名单数量 |

新 Key 与旧用户 ID 名单隔离，不能把旧名单直接复制进来。Java 契约见 JAVA-CONTRACT.md。
redis_database 代码支持 0–15；AWS Redis Cluster 模式只能使用 DB 0，需按实际实例选择。
密码不会自动展开环境变量；部署时通过受控流程注入，不要提交真实密码或开启会输出配置的 SDK debug 日志。

首次 SDK 定时回调启动同步，首次加载之前走 stable。每次全量 SMEMBERS，校验完整响应后原子替换缓存。
刷新失败保留旧快照但不续期，满 TTL 后走 stable；成功空名单立即清空缓存。
超时后后续周期可重试，旧响应晚到不能覆盖较新的快照。
本地缓存属于 Wasm 实例/配置，不是整个集群共享缓存，也不保证一个 Pod 只有一份。
Redis 调用量约为活动实例数 ÷ 刷新间隔，而不是业务请求量。
max_entries 是响应接收后的校验，不是 Redis 服务端返回大小限制；Java 写入端也必须限制容量。

## AWS TLS

复用已有 EnvoyFilter 创建的 `outbound|6379||aws-redis.dns` 集群。
实际 AWS 域名、端口、TLS、SNI、CA 和证书校验仍由该 Envoy 集群负责，不在插件里再次建立 TLS。
本项目不修改你已验证可用的 TLS 配置。上线前确认目标 Gateway 实际存在该集群，CA 文件存在且证书校验成功。

## 路由与作用域

当前 YAML 使用 defaultConfig，属于全局配置示例。生产部署前限定到目标业务作用域；
必须同时覆盖该业务的稳定与灰度路由，不能只挂在灰度路由上，否则无法可靠清除伪造 Header。
沿用现有 Header 灰度路由，将匹配 Header 设为 x-gray-user，匹配值设为 canary。
stable 应进入稳定服务；排除其他权重或 Cookie 灰度规则干扰，并在真实 Gateway 验证 Header 改写后路由重算。
本插件不禁用重路由，但本地模拟测试不能证明实际控制面生成的路由配置正确。

## 构建与发布

使用支持 wasip1 c-shared 的 Go 工具链（项目 go.mod 要求 Go 1.24.1 或更高）。

```powershell
go test ./...
go vet ./...
$env:GOOS = 'wasip1'
$env:GOARCH = 'wasm'
go build -buildmode=c-shared -o main.wasm ./
Remove-Item Env:GOOS
Remove-Item Env:GOARCH
docker build -t YOUR_REGISTRY/gray-whitelist-wasm:0.2.0 .
docker push YOUR_REGISTRY/gray-whitelist-wasm:0.2.0
# 先编辑 wasm-plugin.yaml 的镜像、凭据及业务作用域，再应用：
kubectl apply -f wasm-plugin.yaml
```

Linux 构建：`GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o main.wasm ./`。
Dockerfile 只打包已构建的 main.wasm，每次发布必须先重新编译。
仓库依赖固定版本，无需本地 SDK 路径。

## 本地验证与上线验收

`go test ./...` 包括摘要、大小写、伪造 Header 覆盖、配置校验、缓存过期、失败保留、超时晚到及 SDK 请求 Hook 测试。
`java examples/TokenDigest.java` 执行 Java 摘要契约自检（需要支持源码运行的 JDK）。
`cmd/token-digest` 是从标准输入读取原值的离线摘要工具，不自动去掉换行；勿将真实 Token 放命令行参数。

上线验收：

1. Redis 加入测试 Token 摘要，等待刷新，确认命中灰度服务。
2. 非白名单伪造 x-gray-user: canary，确认仍走稳定服务。
3. 删除成员，等待刷新，确认回到稳定服务。
4. 中断 Redis，确认短期使用旧名单，TTL 到期后走稳定服务且业务请求不等待 Redis。
5. 恢复 Redis，确认自动恢复刷新；多副本网关分别检查。

Redis 故障按上述规则降级，并不保证所有故障均不影响业务：插件 Header 宿主操作失败会返回 503；
Wasm 加载失败或运行时异常的行为还取决于网关配置，需单独演练。
本地测试和编译不等于已完成 AWS/Higress 集群部署验证。
