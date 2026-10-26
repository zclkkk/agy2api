# 配置参考

agy2api 使用扁平的 TOML 配置。以 [config.example.toml](../config.example.toml) 为起点，仅填写需要覆盖的选项：

```toml
listen = "127.0.0.1:8080"
data_dir = "./data"
api_key = ""
```

通过 `--config config.toml` 指定文件。省略 `--config` 时使用默认值和环境变量，不自动寻找配置文件。配置在启动时读取，修改后需要重启；相对路径以进程工作目录为基准。

## 选项

| 字段 | 默认值 | 说明 |
|---|---|---|
| `listen` | `"127.0.0.1:8080"` | HTTP 监听地址 |
| `data_dir` | `"./data"` | 账号凭据与会话状态目录 |
| `api_key` | `""` | 客户端访问密钥；留空时只允许本机访问 |
| `proxy_url` | `""` | 上游连接代理，支持 HTTP(S)、SOCKS5、SOCKS5H |
| `max_concurrent_requests` | `4` | 同时执行的生成与压缩请求数，范围 1～128；token 计数不占名额 |
| `max_body_bytes` | `33_554_432` | 请求体上限，单位字节，范围 1 KiB～256 MiB；也作为远程附件下载总预算 |
| `state_ttl_hours` | `168` | 会话状态有效期，单位小时，范围 1～2,562,047 |
| `max_state_bytes` | `1_073_741_824` | 状态记录内容的总大小软上限，至少 64 MiB；不等于数据库文件大小 |
| `sanitize_client_markers` | `true` | 清理已知客户端的 system 身份及计费标记；兼容排障时可关闭 |

未知字段、重复字段、类型错误和非法值会拒绝启动。配置文件上限为 1 MiB。

并发达到上限后，请求最多等待 2 分钟，超时返回 429。模型目录缓存 5 分钟。配置不提供这些内部时长的覆盖项。

## 环境变量

优先级为 **环境变量 > TOML 文件 > 默认值**。只支持以下四个覆盖项；环境变量即使为空，也会覆盖文件值。

| 环境变量 | 配置项 |
|---|---|
| `AGY2API_LISTEN` | `listen` |
| `AGY2API_DATA_DIR` | `data_dir` |
| `AGY2API_API_KEY` | `api_key` |
| `AGY2API_PROXY_URL` | `proxy_url` |

`proxy_url` 留空时沿用系统代理环境变量。OAuth、模型调用和上游版本发现使用该代理；HTTPS 媒体附件下载使用独立连接，不经过代理，也不携带账号凭据。

## 访问控制

监听非回环地址必须设置 `api_key`。通过反向代理以域名访问时，也应设置密钥。

客户端使用 `Authorization: Bearer <api_key>`；Messages 也接受 `x-api-key`。这里的 API Key 是访问 agy2api 的密钥，与 Google OAuth 凭据分开管理。`/healthz` 不需要认证。

## 数据目录

```text
data/
  credentials.json   OAuth 凭据
  state.db           会话历史、签名索引和回放密钥
```

凭据由 `login` 命令写入并自动刷新；凭据和数据库文件权限设为 `0600`。数据库中的普通会话记录没有数据库级加密。更换账号时使用新的 `data_dir`，同一数据目录只运行一个服务进程。

状态超出有效期、容量或内部记录数量限制后，会清理最旧的记录。过期的 Responses ID 无法再用于续聊；客户端仍可携带完整历史发起新请求，无法恢复的思考内容或签名会降级处理。

## 客户端标记清理

`sanitize_client_markers` 只处理 system 中独立出现的已知身份句和 `x-anthropic-billing-header:` 行。业务指令、代码围栏、用户消息、工具描述、工具参数及结果不参与清理。

日志记录命中的规则和数量，不记录被移除的文本。排查请求差异时可以设置：

```toml
sanitize_client_markers = false
```
