# agy2api

把一个 Antigravity 账号接入 **OpenAI Responses** 和 **Anthropic Messages** API。Go 编写，交付一个原生可执行文件；运行数据保存在本地目录。

2026-10-26 的真实订阅验收覆盖 **101 个场景：97 项通过、2 项受上游阻止、2 项存在结束元数据兼容差异**。结果见 [验收记录](docs/validation-2026-10-26.json)。上游模型与额度会变化，接口覆盖范围见下表。

## 构建

需要 Go 1.27.x。依赖版本由 `go.mod` 和 `go.sum` 固定。运行 Python 验收脚本时需要 Python 3.10 或以上版本。

```sh
CGO_ENABLED=0 go build -trimpath -o bin/agy2api ./cmd/agy2api
./bin/agy2api version
```

编译后的程序不依赖 Go、Python、系统 SQLite 或容器运行时。HTTPS 连接使用操作系统的 CA 信任库。

## 登录与运行

```sh
cp config.example.json config.json
./bin/agy2api login --config config.json
./bin/agy2api models --config config.json
./bin/agy2api serve --config config.json
```

`login` 会打印 Google 授权链接，并在 `127.0.0.1:51121` 接收回调。打开链接完成授权即可；程序验证 OAuth state，使用 PKCE，再获取 Antigravity project 信息。账号必须已能正常使用 Antigravity。

远程主机没有可访问的本地回调端口时：

```sh
./bin/agy2api login --config config.json --manual
```

在自己的浏览器里授权，然后把浏览器地址栏里的完整 localhost 回调 URL 粘贴到该命令的终端。回调页连接失败是预期情况。不要把 URL 或凭据发到聊天、工单或日志中。

默认监听 `127.0.0.1:8080`。监听非本机地址时必须配置 `api_key`。支持以下环境变量覆盖配置：

```text
AGY2API_LISTEN
AGY2API_DATA_DIR
AGY2API_API_KEY
AGY2API_PROXY_URL
```

客户端可用 `Authorization: Bearer <api_key>`；Messages 也接受 `x-api-key`。代理密钥与 Google OAuth token 相互独立。`proxy_url` 支持 HTTP(S) 和 SOCKS5 上游代理。媒体 URL 下载使用独立连接，不携带账号凭据，不经过该代理。

## 调用

先运行 `models`，使用账号实际返回的模型 ID 替换下方的 `MODEL_ID`。

```sh
curl http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","input":"你好","stream":true}'

curl http://127.0.0.1:8080/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"MODEL_ID","max_tokens":1024,"messages":[{"role":"user","content":"你好"}],"stream":true}'
```

配置了代理密钥时，为上述请求补充 `Authorization` 或 `x-api-key` 请求头。

## 当前覆盖范围

| 功能 | 状态 |
| --- | --- |
| OAuth 登录、自动刷新与凭据原子更新 | 已实现 |
| `/v1/models` 与 `/v1/models/{id}` | 从账号实时发现，缓存 5 分钟；排除 `API_PROVIDER_INTERNAL` 条目 |
| `/v1/responses`、`/v1/messages` | 流式、非流式、文本和多轮对话 |
| 函数工具 | 声明、工具选择、并行调用、工具结果回传、原始 ID 和签名恢复 |
| 思考内容 | Gemini、Sonnet、Opus 的输出与工具回放通过；支持预算/强度转换、加密回放、篡改检查 |
| 图片与文件输入 | 图片、PDF、HTTPS 图片均通过；Responses 的音频和视频文件输入通过；`gs://` URI 透传尚未实测 |
| JSON 输出与 JSON Schema | 映射到上游；`strict:true` 暂不支持 |
| 原生搜索 | 两个协议的搜索、引用及中文流式引用均通过；仅对模型目录宣告支持搜索的模型开放 |
| 图片生成输出 | Responses 图片生成通过；后续图片编辑被上游额度阻止，尚未完成验收 |
| token 计数 | `/v1/messages/count_tokens`、`/v1/responses/input_tokens` |
| Responses 历史 | `previous_response_id`、查询、删除、`input_items`；SQLite 持久化 |
| 取消、限流、流中错误 | 取消传播到上游；保留 `Retry-After`；断流不返回成功终止事件 |
| 停止序列 | 文本截断通过；精确的 `stop_reason` / `stop_sequence` 匹配元数据尚不能还原 |

尚未实现：Chat Completions（明确不在范围内）、Files 上传服务与 `file_id`、后台任务、WebSocket、Conversations 服务、远程 MCP、托管代码执行、音视频专用接口、compaction、显式 prompt cache 控制、严格结构化输出保证，以及完整的平台级参数集合。

不支持的请求参数或内容会返回明确错误，不会通过删掉附件、工具或约束来让请求“成功”。具体模型是否接受某种输入、schema 或思考参数，还取决于 Antigravity 上游。

原生搜索暂不支持与自定义函数工具混用。图片生成目前使用账号提供的图片模型及其默认生成设置，未提供独立 Images API；Messages 不能返回生成图片。`user_agent` 可在配置里更新，以适应上游客户端版本要求；当前默认值已用于本次真实验收。

Claude 的 Responses `reasoning.effort` 映射为预算：`low=1024`、`medium=8192`、`high=24576`。未指定输出上限时使用模型目录的 `maxOutputTokens`；显式输出上限必须大于思考预算。Gemini 使用原生推理等级。部分 Gemini 模型会只把思考签名放在工具调用上，未必返回可见思考块。

## 数据与状态

默认数据目录：

```text
data/
  credentials.json   OAuth 凭据，权限 0600
  state.db           Responses 历史、工具签名索引和回放加密密钥
```

Responses 默认 `store:true`。设为 `false` 时不保存该响应及其上下文，也不能用其 ID 继续会话；为了工具回合能继续，短期工具签名索引仍保留模型、原始 ID、参数摘要和签名，不保存工具参数原文。思考回放数据随响应加密返回客户端。普通保存的会话记录没有数据库级加密，请保护数据目录。

状态默认保留 168 小时、最多 10000 条；超限会清理旧记录。删除或丢失数据库会使已有签名载体失效。状态绑定 Antigravity project，更换账号应使用新的数据目录。不要同时运行多个进程使用同一份账号数据。

客户端应把返回的思考块、`encrypted_content`、工具 ID 和参数原样带到下一轮。不要把签名当作可编辑文本。使用 `previous_response_id` 时，新的顶层 `instructions` 独立生效，不自动继承上一次的顶层指令。

## 验证

```sh
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build -trimpath -o bin/agy2api ./cmd/agy2api
```

测试会启动仅监听本机的模拟上游，无需真实账号，不消耗订阅额度。测试覆盖流式事件顺序、拆分 usage、错误结束、工具第二轮、客户端取消、凭据刷新竞争、SQLite 重启恢复、状态过期和签名篡改。

可选的官方 Python SDK 验收脚本（仅开发测试需要 Python）：

```sh
python -m pip install -r tests/requirements.txt
python tests/sdk_smoke.py --binary bin/agy2api
```

已用 `openai 3.37.0`、`anthropic 2.3.0` 验证文本、流式聚合、思考与工具回合、token 计数和 Responses 历史接口。脚本只连接本机模拟上游，临时账号数据会自动清理。OpenAI usage 中上游未提供的 `cache_write_tokens` 填 0，不代表支持显式缓存写入控制。

真实账号验收脚本会实际消耗订阅额度，需要先完成登录。它在临时目录复制凭据、使用独立 SQLite 状态库，不把测试会话写入正在使用的数据目录：

```sh
python -m pip install -r tests/live-requirements.txt
python tests/live_validate.py --config config.json --report /tmp/agy2api-live-report.json
# 只复验部分场景：
python tests/live_validate.py --only thinking --report /tmp/agy2api-thinking-report.json
```

验收覆盖模型目录、两个协议的流式和非流式调用、系统指令、约 4.8 万输入 token 的长上下文、单个/并行工具、工具返回图片、思考及签名回放、结构化输出、图片/PDF/音视频文件、搜索引用、输出上限、取消、真实 OAuth 刷新、重启恢复、删除、API 密钥验证和并发限流恢复。测试使用合成内容；报告不包含凭据或原始签名。原始运行报告属于生成文件，Git 会忽略 `tests/live-report*.json`；`docs/` 只保留整理后的验收记录。

该次验收的限制：

- `gemini-2.5-pro`：上游持续返回 `503 / MODEL_CAPACITY_EXHAUSTED`。模型仍保留在目录里，不把暂时无容量误判为永久不支持。
- 图片生成成功；连续图片编辑随后收到 `429 / QUOTA_EXHAUSTED`，上游要求等待约 4.7 小时，未继续请求。额度恢复后需单独复验 `image_edit_roundtrip`。
- `stop_sequences` 可以正确截断文本，但上游仅返回 `STOP`，不包含命中的序列。当前 Messages 仍返回 `end_turn` 和 `null`，这两项验收标记为 `partial`，不能视为完整 Anthropic 元数据兼容。
- `gs://` 文件 URI 尚未使用真实可访问对象验收；严格 schema、后台任务等未实现能力仍按上面的范围明确拒绝。
- usage 使用上游提供的字段；部分模型不单独上报思考 token，此时 `reasoning_tokens=0` 不表示没有思考。报告中的 usage 是场景内采样响应的数据，不能相加当作整轮消耗。

## 代码结构

```text
cmd/agy2api          login / serve / models / version
internal/auth       单账号 OAuth 与刷新
internal/config     配置及校验
internal/antigravity 原生请求、SSE、模型发现和 token 计数
internal/protocol   Responses / Messages 编解码与回放
internal/server     HTTP 路由、认证、并发控制、媒体下载
internal/state      SQLite 状态与加密载体
internal/securefile 私有文件原子写入
tests/              可选的 SDK 模拟验收与真实账号验收
docs/               已整理的验收记录
```

上游协议参考本地 CLIProxyAPI；本项目不依赖 CPA 模块。相关来源与许可保留在 `THIRD_PARTY_NOTICES`。
