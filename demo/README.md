# 前后端灰度发布 Demo

本 Demo 按 `gray-whitelist-wasm 0.7.3` 的白名单模式验证完整链路（未设置模式 Key 时默认为 whitelist）：

```text
PC_AUTH_TOKEN Cookie
  -> gray-whitelist 解析 JWT
  -> Redis 白名单和灰度开关
  -> x-gray-user: canary/stable
  -> Higress API 路由选择后端版本
  -> API 返回 x-frontend-gray-refresh
  -> Set-Cookie: x-gray-user=always
  -> Higress 前端 Cookie 路由选择静态资源版本
```

## 1. 构建镜像

```bash
docker build -t harbor-ningxia.pontosense.net/base-image/gray-demo-api:stable ./api
docker build -t harbor-ningxia.pontosense.net/base-image/gray-demo-api:canary ./api
docker build -t harbor-ningxia.pontosense.net/base-image/gray-demo-frontend:stable ./frontend/stable
docker build -t harbor-ningxia.pontosense.net/base-image/gray-demo-frontend:canary ./frontend/canary

docker push harbor-ningxia.pontosense.net/base-image/gray-demo-api:stable
docker push harbor-ningxia.pontosense.net/base-image/gray-demo-api:canary
docker push harbor-ningxia.pontosense.net/base-image/gray-demo-frontend:stable
docker push harbor-ningxia.pontosense.net/base-image/gray-demo-frontend:canary
```

将 `k8s/apps.yaml` 中的镜像地址替换为同样的仓库。

## 2. 部署应用和路由

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/apps.yaml
kubectl apply -f k8s/ingress.yaml
```

`canary.pontosense.net` 需要解析到 Higress Gateway。

## 3. 配置 0.7.3 插件

将根目录的 `wasm-plugin.yaml` 复制为环境配置，并修改：

```yaml
url: oci://harbor-ningxia.pontosense.net/base-image/gray-whitelist-wasm:0.7.3
```

Demo 的 API 路由必须绑定 `gray-whitelist` 插件。插件只绑定 `/api` 路由，不绑定前端静态资源路由；前端静态资源由 Higress 的 Cookie 灰度规则处理。

插件配置的 Redis Key 使用：

```yaml
gray_enabled_key: gray:whitelist:pc:enabled
gray_tenant_key: gray:whitelist:pc:tenantId
gray_user_key: gray:whitelist:pc:userId
gateway_include_path_prefixes:
  - /api/
jwt_token_cookie_name: PC_AUTH_TOKEN
frontend_gray_cookie_name: x-gray-user
frontend_gray_cookie_domain: ""
```

只有路径命中 `/api/` 且携带唯一、非空的 `PC_AUTH_TOKEN`，才参与灰度判断。缺失、空值或重复 Token 会跳过请求及响应修改；升级时删除旧 `gateway_exclude_path_prefixes` 参数。

## 4. 初始化 Redis

```bash
REDIS_ADDR=redis.gray-demo.svc.cluster.local:6379 bash k8s/redis-init.sh
```

该脚本会：

```text
gray:whitelist:pc:enabled = 1
gray:whitelist:pc:tenantId 里加入 2
```

## 5. 测试

使用包含以下 Payload 的 Demo JWT：

```json
{"tenantId":2,"id":283778812672}
```

```bash
curl -k -i \
  -H 'Host: canary.pontosense.net' \
  -H 'Cookie: PC_AUTH_TOKEN=YOUR_DEMO_JWT' \
  https://canary.pontosense.net/api/gray/status
```

预期 API 响应包含：

```http
x-gray-user: canary
Set-Cookie: x-gray-user=always; Path=/; Secure; HttpOnly; SameSite=Lax
x-frontend-gray-refresh: true
```

浏览器访问：

```text
https://canary.pontosense.net/
```

stable 和 canary 使用两套实际不同的 HTML 页面。前端调用 `/api/gray/status` 后，页面会展示后端返回的实际版本；检测到 `x-frontend-gray-refresh: true` 时刷新页面。刷新后的前端资源携带 `x-gray-user=always`，进入 `gray-demo-frontend-canary`。

页面首次加载后还会每 10 秒轮询一次 `/api/gray/status`。当插件返回 `x-frontend-gray-refresh: true` 且距离上次刷新超过 5 秒时，前端立即给当前 URL 增加 `_gray_refresh` 时间戳并重新加载，以绕过旧页面缓存；5 秒内重复收到刷新头会忽略，防止多个 Gateway 实例缓存短暂不一致导致重复刷新。后端容器会打印每次 API 请求的访问日志，可使用 `kubectl logs` 查看。

## 6. 关闭灰度

```bash
redis-cli SET gray:whitelist:pc:enabled 0
```

再次调用 `/api/gray/status`。插件会返回 stable，并在成功的 2xx 响应中清理 Cookie：

```http
x-gray-user: stable
Set-Cookie: x-gray-user=; Path=/; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Secure; HttpOnly; SameSite=Lax
x-frontend-gray-refresh: true
```

前端收到刷新头后刷新页面，回到 stable 前端。

## 注意事项

- API `/api/**` 和前端 `/` 使用同一 Host，避免 Cookie 域名不一致。
- API 路由和前端静态资源路由分开，避免登录/API 请求被转发到前端 Nginx。
- 当前 0.7.3 只有满足路径和 Token 两个入口条件，才在 2xx 响应中写入或清理前端 Cookie；登录 302 不负责清理 Cookie。
- Demo JWT 仅用于演示，当前插件只解码 Payload，不验证 JWT 签名。
