# 配置参考

agy2api 使用扁平的 TOML 配置。以 [config.example.toml](../config.example.toml) 为起点，仅填写需要覆盖的选项：

```toml
listen = "127.0.0.1:8080"
data_dir = "./data"
```

配置中没有账号和密钥：账号由命令行添加，保存在 `data_dir` 中，见[账号](#账号)。

通过 `--config config.toml` 指定文件。省略 `--config` 时使用默认值和环境变量，不自动寻找配置文件。配置在启动时读取，修改后需要重启；相对路径以进程工作目录为基准。

## 选项

| 字段 | 默认值 | 说明 |
|---|---|---|
| `listen` | `"127.0.0.1:8080"` | HTTP 监听地址 |
| `data_dir` | `"./data"` | 账号目录，保存凭据、API Key 与会话状态 |
| `proxy_url` | `""` | 上游连接代理，支持 HTTP(S)、SOCKS5、SOCKS5H |
| `max_concurrent_requests` | `4` | 每个账号同时执行的生成与压缩请求数，范围 1～128；token 计数不占名额 |
| `max_body_bytes` | `33_554_432` | 请求体上限，单位字节，范围 1 KiB～256 MiB；也作为远程附件下载总预算 |
| `state_ttl_hours` | `168` | 会话状态有效期，单位小时，范围 1～2,562,047 |
| `max_state_bytes` | `1_073_741_824` | 每个账号状态记录内容的总大小软上限，至少 64 MiB；不等于数据库文件大小 |
| `sanitize_client_markers` | `true` | 清理已知客户端的 system 身份及计费标记；兼容排障时可关闭 |

未知字段、重复字段、类型错误和非法值会拒绝启动。配置文件上限为 1 MiB。

并发达到上限后，请求最多等待 2 分钟，超时返回 429。模型目录缓存 5 分钟。配置不提供这些内部时长的覆盖项。

## 环境变量

优先级为 **环境变量 > TOML 文件 > 默认值**。只支持以下三个覆盖项；环境变量即使为空，也会覆盖文件值。

| 环境变量 | 配置项 |
|---|---|
| `AGY2API_LISTEN` | `listen` |
| `AGY2API_DATA_DIR` | `data_dir` |
| `AGY2API_PROXY_URL` | `proxy_url` |

`proxy_url` 留空时沿用系统代理环境变量。OAuth、模型调用和上游版本发现使用该代理；HTTPS 媒体附件下载使用独立连接，不经过代理，也不携带账号凭据。

## 账号

一个服务可以托管多个 Antigravity 账号。每个账号有自己的 API Key，请求按 Key 使用对应账号。

| 命令 | 作用 |
|---|---|
| `login` | 添加账号，完成后显示账号 ID 与 API Key；再次登录同一个 Google 账号只更新凭据，Key 不变 |
| `logout <账号 ID>` | 删除账号及其凭据、Key 和会话状态；不会撤销 Google 授权 |
| `accounts` | 列出账号 ID、邮箱、显示名、API Key 和创建时间 |
| `quota` | 列出每个账号的模型、剩余额度和重置时间 |

命令都接受 `--config`，`logout` 的账号 ID 放在最后。添加、更新或删除账号后需要重启 `serve`。账号 ID 由 Google 账号派生，邮箱变更后不变。

Key 由程序生成，不能自定义或更换。Key 泄露时，执行 `logout` 后重新 `login` 获得新 Key，该账号的会话状态随之清空。账号的 Antigravity 项目变化时，`login` 会拒绝更新，同样需要先 `logout`。

除 `/healthz` 外，所有请求都需要 Key：使用 `Authorization: Bearer <API Key>`，也可以使用 `x-api-key`。API Key 只用于访问 agy2api，与 Google OAuth 凭据分开保存。

每个账号使用自己的上游凭据、模型列表、额度、并发名额和会话状态；进程、监听地址、日志和代理由所有账号共享。状态存储上限和并发数按账号计算，总量随账号数增加。

## 数据目录

```text
data/
  accounts/
    .lock                协调账号变更与运行中服务的锁文件
    <账号 ID>/
      credentials.json   OAuth 凭据
      account.json       API Key、Google 账号 ID、邮箱和显示名
      state.db           会话历史、签名索引和回放密钥
```

账号目录由 `login` 创建，凭据会自动刷新；目录权限为 `0700`，文件为 `0600`。`login`、`logout` 与服务回写凭据时都会短暂持有锁文件，可以在服务运行时执行；Windows 上 `logout` 需要先停止服务。数据库中的普通会话记录没有数据库级加密。账号目录缺失文件或内容不一致时，服务拒绝启动并指出该目录。同一数据目录只运行一个服务进程。

状态超出有效期、容量或内部记录数量限制后，会清理最旧的记录。过期的 Responses ID 无法再用于续聊；客户端仍可携带完整历史发起新请求，无法恢复的思考内容或签名会降级处理。

## 客户端标记清理

`sanitize_client_markers` 只处理 system 中独立出现的已知身份句和 `x-anthropic-billing-header:` 行。业务指令、代码围栏、用户消息、工具描述、工具参数及结果不参与清理。

日志记录命中的规则和数量，不记录被移除的文本。排查请求差异时可以设置：

```toml
sanitize_client_markers = false
```
