# Local Base Template Build Debug Notes

## 1. 错误是什么

执行本地 base template 构建：

```bash
make -C packages/shared/scripts local-build-base-template
```

最开始构建在 provisioning 阶段失败，客户端只显示泛化错误：

```text
BuildError: An internal error occurred. Please try again or contact support with the build ID.
```

查看构建日志和 orchestrator 日志后，实际出现过几类错误：

- provisioning VM 内 `apt-get update` 无法解析或访问 Debian 源，导致基础包安装失败。
- sandbox 对公司内网 DNS/proxy 地址访问受限，无法连接 `10.x.x.x` 网络中的 DNS/proxy。
- provisioning 成功后，构建卡在 `Creating base sandbox template layer`，执行 `sync` 时失败：

```text
error building base layer: error running sync command: command failed: permission_denied: HTTP status 403 Forbidden
```

中间还出现过 sandbox proxy 访问 envd 的 `504 Gateway Timeout`；修正 upstream Host 后，错误变成 `403 Forbidden`，说明请求已经到达 envd 或相关代理路径，但鉴权/转发仍不正确。

最终验证：修复后构建已经通过，日志显示构建继续执行到 `DEFAULT USER user`、`finalize`、`optimize`，最后成功结束：

```text
Build finished, took 3m59s
```

## 2. 原因是什么

这次问题不是单一错误，而是一条本地企业网络环境下的链路问题。

1. 构建 VM 默认 DNS 不适合当前网络

rootfs 中的 `/etc/resolv.conf` 默认使用外部 DNS，例如 `8.8.8.8`。在当前网络环境里该 DNS 不可用或被限制，导致 VM 内 `apt-get update` 无法解析 `deb.debian.org`。

2. sandbox 防火墙默认不允许访问公司内网段

当前可用 DNS/proxy 位于 `10.0.0.0/8` 内网段，例如：

- DNS: `10.109.19.199`
- apt proxy: `http://proxy-dmz.intel.com:912`

但 sandbox 内部访问私有网段受 orchestrator 配置控制，需要显式允许 `10.0.0.0/8`，否则即使 DNS/proxy 地址正确也无法连通。

3. apt 没有使用 host 的代理配置

provisioning 脚本原本没有把 host proxy 配置写给 apt。在当前环境中，apt 直连 Debian 源不可用，需要通过公司 proxy 才能下载包。

4. sandbox proxy 转发 envd 请求时 Host 不正确

构建进入 base layer 后，`SyncChangesToDisk` 通过 sandbox proxy 调用 envd process API。envd 端口是 `49983`，请求 Host 会被设置成类似：

```text
49983-<sandboxID>-00000000.localhost
```

这个虚拟 Host 用于 sandbox proxy 路由，但不应该继续作为 upstream Host 转发给 envd。envd upstream 应该看到真实目标 host，例如：

```text
10.11.x.x:49983
```

否则请求会在 envd/proxy 路径上失败，之前表现为 `504 Gateway Timeout`。

5. envd process API 需要 `X-Access-Token`

envd 的 `/process.Process/Start` 属于受保护接口。如果 sandbox 配置了 envd access token，请求必须带：

```text
X-Access-Token: <token>
```

直接 envd helper 会设置该 header，但 sandbox proxy 路径之前没有注入这个 token，因此 Host 修正后错误变成 `403 Forbidden`。

6. shared reverse proxy 不应该为内部 sandbox upstream 使用 host 环境代理

shared proxy 的 upstream `http.Transport` 之前使用 `http.ProxyFromEnvironment`。当前 host 环境设置了公司代理：

```text
http_proxy=http://proxy-dmz.intel.com:912
https_proxy=http://proxy-dmz.intel.com:912
```

这会导致本应直连的内部 sandbox upstream，例如 `10.11.x.x:49983`，被错误地送到公司代理，进而出现误导性的 `403/504`。sandbox proxy 转发到本机内部网络目标时不应该走 host corporate proxy。

## 3. 通过哪些改动修复

### 允许 sandbox 访问公司内网段

修改 `packages/orchestrator/.env.local`，允许 sandbox 访问整个 `10.0.0.0/8`：

```env
ALLOW_SANDBOX_INTERNAL_CIDRS=10.0.0.0/8
```

这让构建 VM 可以访问公司 DNS 和 apt proxy。

### 在 provisioning 阶段给 apt 配置 proxy/DNS

修改 `packages/orchestrator/pkg/template/build/phases/base/provision.sh`：

- 从环境变量读取 `HTTP_PROXY` / `http_proxy`、`HTTPS_PROXY` / `https_proxy`、`NO_PROXY` / `no_proxy`。
- 如果环境变量没有 proxy，则 fallback 到当前验证可用的 apt proxy：

```text
http://proxy-dmz.intel.com:912
```

- 写入 apt proxy 配置：

```text
/etc/apt/apt.conf.d/99proxy
```

- 临时把 VM 内 DNS 从 `8.8.8.8` 替换为公司 DNS：

```text
10.109.19.199
```

这些改动修复了 provisioning 阶段 `apt-get update/install` 失败的问题。

### 修正 sandbox proxy 到 envd 的 upstream Host

修改 `packages/shared/pkg/proxy/pool/client.go`：

- 普通 sandbox 业务端口仍保留原始 request Host。
- envd 端口 `49983` 使用真实 upstream host：

```go
r.Out.Host = t.Url.Host
```

这样 envd 不再收到 `49983-<sandboxID>-00000000.localhost` 这种只用于 proxy 路由的虚拟 Host。

### 给 envd 端口请求注入 access token

修改 `packages/shared/pkg/proxy/pool/destination.go`，增加 envd token 字段：

```go
EnvdAccessToken *string
```

修改 `packages/orchestrator/pkg/proxy/proxy.go`，创建 destination 时传入 sandbox 的 envd token：

```go
EnvdAccessToken: sbx.Config.Envd.AccessToken,
```

修改 `packages/shared/pkg/proxy/pool/client.go`，只在转发 envd 端口请求时设置：

```go
if t.EnvdAccessToken != nil {
    r.Out.Header.Set("X-Access-Token", *t.EnvdAccessToken)
}
```

这让 sandbox proxy 路径和 direct envd helper 的鉴权行为一致，修复 envd process API 的 `403 Forbidden`。

### 禁止 shared proxy 内部 upstream 使用 host 环境代理

修改 `packages/shared/pkg/proxy/pool/client.go`，将 upstream transport 的 proxy 从环境代理改为禁用：

```go
Proxy: nil,
```

这样 sandbox proxy 转发到内部 sandbox 地址 `10.11.x.x:49983` 时会直连，不会被 host 上的 `http_proxy/https_proxy` 送到公司代理。

### 给 sync 命令增加短暂 retry

修改 `packages/orchestrator/pkg/template/build/sandboxtools/command.go`：

- `SyncChangesToDisk` 对 `connect.CodeUnavailable` 做短暂重试。
- retry 时间窗口为 `20s`，间隔为 `250ms`。

该改动用于覆盖 envd/process API 刚启动时的短暂不可用窗口。最终调试用的 `SyncChangesToDiskDirect` helper 已移除，没有保留临时诊断入口。

## 验证结果

执行以下命令验证通过：

```bash
make -C packages/orchestrator build-debug
make -C packages/shared/scripts local-build-base-template
```

结果：

- orchestrator 编译成功。
- base template 构建成功。
- 构建日志没有再出现 `permission_denied`、`HTTP status 403` 或 envd access token 相关错误。
