# CLIProxyAPI Auth Concurrency · 认证账号并发管理

![Language](https://img.shields.io/badge/Go-1.23%2B-blue) ![Platform](https://img.shields.io/badge/Linux-amd64-brightgreen) ![License](https://img.shields.io/badge/license-MIT-lightgrey)

一个支持 **CLIProxyAPI 原生插件 ABI** 的账号独立并发管理插件。**有管理界面，不用手动输入认证 ID，不用编辑 YAML 来调整每个账号的并发。**

> **版本：v0.2.1（预览版）**。已通过本地 `go test -race`、Linux amd64 `c-shared` 编译、Go 插件 RPC 单元测试。**尚未在你的线上 CPA 版本上进行完整集成与真实并发压测**，首次部署务必先在测试 CPA 上验证。

## 功能

- 原生管理控制台菜单：**账号并发控制**。
- 管理员输入 CPA `Management Key` 后自动读取认证文件账号列表（`host.auth.list`），通过 **下拉框** 选取账号，设置整数并发上限。
- 点击 **保存并热更新** 调用 CPA 原生 `PATCH /v0/management/plugins/auth-concurrency/config`，将设置写入 **CPA 原始配置文件**；服务异步应用插件 `plugin.reconfigure`。页面会轮询 **插件当前实际运行配置**，成功后才提示生效。
- `0` 表示不限制；**恢复默认** 会删除此账号的覆盖配置。
- 页面展示账号运行状态、实时并发数量、上限、满载拒绝次数，每 5 秒刷新。
- 按认证 **ID** 独立限制同时请求数，满载时挑选其他候选账号，所有账号都满载时拒绝；不无限排队。
- 通过 `request.complete` 在请求完成、取消和流结束时归还占用；重复完成事件安全。
- 控制面走管理密钥认证，不把账号密钥/认证文件内容公开给页面，管理密钥不写入浏览器持久存储。

## 用户界面

安装后在 CPA **插件管理 → 账号并发控制** 打开插件资源页面；也可以直接打开：

```text
https://YOUR-CPA-HOST/v0/resource/plugins/auth-concurrency/status
```

界面流程：

1. 输入 CPA Management Key，点 **读取账号**。页面自动列出认证文件的名称、邮箱（存在时）、认证 ID、provider 与状态。
2. 通过 **账号下拉框** 选择目标账号，比如某个 Codex 账号。
3. **最大同时请求数** 填 `2`，点击 **保存并热更新**。
4. 页面先保存到 CPA 配置文件，然后验证插件的运行时配置；确认生效才显示成功。
5. 随时在表格中查看已用并发和上限；需要取消账号特殊上限时点击 **恢复默认**。

**为什么需要再次输入 Management Key？** 插件菜单对应的 `/v0/resource/plugins/...` 是 CPA 提供的未认证静态资源入口；账号读取和配置修改调用 `/v0/management/...` 时必须单独完成权限验证。为避免把密钥放到网页 URL 或持久存储，页面仅在当前标签页内存中使用密钥。建议通过 HTTPS 或 SSH 端口转发打开管理界面。

## 安装（Linux amd64）

需要 CPA 二进制开启原生插件支持（检查管理 API 响应 `X-CPA-SUPPORT-PLUGIN: 1`），支持 RPC schema ≥ 2，且提供 `host.auth.list` 回调。

从 [GitHub Releases](https://github.com/tz1102/cliproxy-auth-concurrency/releases) 下载 `auth-concurrency_0.2.1_linux_amd64.zip` 并解压获得 `auth-concurrency.so`，然后放到配置的插件目录。例如，当 CPA 工作目录包含 `plugins` 时：

```bash
mkdir -p plugins/linux/amd64
cp auth-concurrency.so plugins/linux/amd64/auth-concurrency.so
```

在 CPA 现有 `config.yaml` 合并以下内容（**不要覆盖已有配置**）：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    auth-concurrency:
      enabled: true
      priority: 100
      default_limit: 0
      strategy: round_robin
      auth_limits_json: '{}'
```

初次安装发现新 `.so` 可能需要重启 CPA 一次；**后续在插件界面调整账号限额不需要重启**。修改会写入 CPA 配置文件并触发异步热加载，页面等待运行时验证。请先备份服务配置，并在独立测试进程中验证你的 CPA 构建支持上述行为。

如果当前 CPA 版本不支持 `host.auth.list`，页面会明确显示读取失败，状态接口仍可使用；可升级 CPA 或通过插件配置手动输入真实 auth ID。

## API 与配置

| API | 用途 |
| --- | --- |
| `GET /v0/resource/plugins/auth-concurrency/status` | 不含认证信息的静态管理页面 |
| `GET /v0/management/plugins/auth-concurrency/status` | 带 Management Key 的实时账号与并发状态 |
| `GET /v0/management/plugins/auth-concurrency/config` | CPA 原生插件持久化配置 |
| `PATCH /v0/management/plugins/auth-concurrency/config` | CPA 原生配置更新并异步热加载 |

`auth_limits_json` 的键是 **真正的认证 ID（`id`）**，不是文件名、登录邮箱或 `auth_index`：

```yaml
auth_limits_json: '{"auth-real-id-A":2,"auth-real-id-B":1}'
default_limit: 0
strategy: round_robin
```

仅更新一个账号覆盖值的示例（一般直接使用图形界面）：

```bash
curl -X PATCH 'http://127.0.0.1:8317/v0/management/plugins/auth-concurrency/config' \
  -H 'X-Management-Key: <KEY>' -H 'Content-Type: application/json' \
  -d '{"auth_limits_json":"{\"auth-real-id-A\":2}"}'
```

**注意：** 手工 PATCH 以上 JSON 会覆盖原 `auth_limits_json` 的整体映射；图形界面在保存前先读取并合并已有配置，以保留其他账号的设置。CPA 目前没有针对同一映射的 CAS/事务更新，因此多个管理员**同时保存**仍可能发生最后写入覆盖。

## 技术结构

- `cmd/cliproxy-auth-concurrency/main.go`: CPA Native ABI / Host callback / 注册菜单与管理路由
- `cmd/cliproxy-auth-concurrency/status.html`: 单文件、无外部依赖的中文响应式管理界面
- `internal/limiter/`: Go 并发额度租约、实时统计、热重配置
- `.github/workflows/ci.yml`: 自动构建与 Race 测试

**实现流程：** `request.intercept_before` 用 CPA 生成的 `RequestID` 打内部关联标签 → `scheduler.pick` 原子预留账号额度 → `request.intercept_after` 移除内部关联标签 → `request.complete` 释放额度。插件配置更新仅调用 `State.Configure`，**不重置正在执行的占用**。假如从 2 降为 1，已执行的 2 个请求会完成，新请求等当前占用下降到小于 1 才有资格选择该账号。

## 构建与测试

```bash
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o auth-concurrency.so ./cmd/cliproxy-auth-concurrency
```

## 限制与上线注意事项

- **每个 CPA 进程独立统计**：多节点部署不能以该插件实现跨节点总并发上限。
- 通过 `scheduler.pick` 预留并发名额，要求 CPA 调度器将 `RequestID` 对应的内部 header 传递至插件，并将 `request.complete` 送达；必须结合所用 CPA 版本验证。
- 重试可能为同一请求同时预留多个认证账号的额度，直到该请求整体结束才释放（保守但安全，会降低部分吞吐）。
- 不设置抢占、超时强行释放，因为超时释放仍然运行的流会破坏硬性并发上限。
- 变更限额/路由策略可能影响原有 CPA 默认权重、优先级和会话粘性效果。只有一个调度插件应对目标业务生效。
- 完整端到端 CPA 实例、浏览器/控制台实际联调及生产负载测试 **仍需执行**；只提供经单元测试和构建验证的预览版，不宣称生产可用。
- 来源代码基于官方公布的插件 ABI 和示例设计，但 **非 CLIProxyAPI 官方项目**；Native `.so` 作为可信代码在进程内运行。

## License

MIT


## 发布到 CPA 官方插件商店

1. 在 [GitHub Releases](https://github.com/tz1102/cliproxy-auth-concurrency/releases/new) 发布版本标签 `v0.2.1`（选择 main）。
2. 本仓库的 `.github/workflows/release.yml` 会自动构建并向 Release 上传 `auth-concurrency_0.2.1_linux_amd64.zip` 及 `checksums.txt`。确认两者上传成功后，再提交商店 PR。
3. Fork [官方商店注册表](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)，仅修改 `registry.json` 的 `plugins` 数组，新增：

```json
{
  "id": "auth-concurrency",
  "name": "Auth Concurrency Guard",
  "description": "Per-credential concurrency limits, live stats and browser management UI with hot reload.",
  "author": "tz1102",
  "repository": "https://github.com/tz1102/cliproxy-auth-concurrency",
  "homepage": "https://github.com/tz1102/cliproxy-auth-concurrency",
  "license": "MIT",
  "tags": ["scheduler", "management", "concurrency"]
}
```

4. 向官方 main 提交 PR，并提供 Release 标签、zip 资产与 checksums 证明。合并后才会在官方商店显示。注意发布之前先在测试 CPA 上进行端到端验证。

CPA 商店安装还会在插件配置中注入嵌套的 `store` 元数据，v0.2.1 已允许此元数据，避免注册失败。
