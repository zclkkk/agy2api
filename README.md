# agy2api

把一个 Antigravity 账号的 **Gemini 模型**接入 **OpenAI Responses** 和 **Anthropic Messages** API。Go 编写，交付一个原生可执行文件；运行数据保存在本地目录。

只提供 Gemini 模型：模型目录中 `apiProvider` 为 `API_PROVIDER_GOOGLE_GEMINI` 的条目。Antigravity 目录里的 Claude、GPT-OSS 等第三方模型各有一套不同的请求规则（思考预算、schema 限制、签名处理），而且 Antigravity 已通知它们自 2026-11-02 起在普通套餐中下线，因此不列出也不转发；请求这些模型会返回 400。两种 API 协议都保留，Messages 协议同样用于调用 Gemini。

2026-10-26 的真实订阅验收覆盖 **118 个场景：113 项通过、3 项受上游容量或额度阻止、2 项存在结束元数据兼容差异，无失败**。结果见 [验收记录](docs/validation-2026-10-26.json)。上游模型与额度会变化，接口覆盖范围见下表。

## 构建

需要 Go 1.27.x。依赖版本由 `go.mod` 和 `go.sum` 固定。运行 Python 验收脚本时需要 Python 3.11 或以上版本。

```sh
CGO_ENABLED=0 go build -trimpath -o bin/agy2api ./cmd/agy2api
./bin/agy2api version
```

编译后的程序不依赖 Go、Python、系统 SQLite 或容器运行时。HTTPS 连接使用操作系统的 CA 信任库。

## 登录与运行

```sh
cp config.example.toml config.toml
./bin/agy2api login --config config.toml
./bin/agy2api models --config config.toml
./bin/agy2api serve --config config.toml
```

`login` 在本机和 SSH 环境下使用同一个授权码回填流程：

1. 终端打印 Google 授权链接，在浏览器中打开并完成账号授权。
2. 浏览器跳转到官方 `https://antigravity.google/oauth-callback` 页面，点击复制授权码。
3. 将授权码粘回运行 `login` 的终端并按 Enter。程序换取凭据、发现 Antigravity project 后原子保存，成功才替换已有凭据。

无需监听本地回调端口、SSH 隧道或判断远程环境；登录只接受 `--config`，旧的 `--manual`、`--port` 已移除。粘贴的是页面中的授权码，不是回调 URL、Access Token 或 Refresh Token；账号必须已能正常使用 Antigravity。

每次登录生成独立的 PKCE S256 挑战，Google 在换取凭据时验证授权码与本次登录的绑定。登录最多等待 5 分钟，超时或失败后重新运行以生成新链接。已有凭据仍可使用，无需因升级重新登录。授权码与凭据不写入配置或日志，请只在登录终端输入。

默认监听 `127.0.0.1:8080`。监听非本机地址时必须配置 `api_key`。未配置 `api_key` 时只接受本机客户端：`Host` 必须是本机地址，浏览器请求的 `Origin` 也必须是本机地址，用来阻止网页跨站调用和 DNS rebinding。经同机反向代理以域名访问时，请配置 `api_key`。支持以下环境变量覆盖配置：

```text
AGY2API_LISTEN
AGY2API_DATA_DIR
AGY2API_API_KEY
AGY2API_PROXY_URL
```

客户端可用 `Authorization: Bearer <api_key>`；Messages 也接受 `x-api-key`。代理密钥与 Google OAuth token 相互独立。`proxy_url` 支持 HTTP(S)、SOCKS5 和 SOCKS5H 上游代理；留空时沿用系统代理环境变量。媒体 URL 下载使用独立连接，不携带账号凭据，不经过代理。

## 配置

配置使用扁平的 TOML 文件，支持注释。示例只启用部署需要关注的字段，其余覆盖项作为注释列出。省略字段使用程序默认值，优先级为 **环境变量 > TOML 文件 > 默认值**；环境变量即使为空也会覆盖文件值。只有上面列出的四个环境变量生效。

| 字段 | 默认值 | 含义与限制 |
|---|---|---|
| `listen` | `"127.0.0.1:8080"` | 监听地址；非回环地址需要 API Key |
| `data_dir` | `"./data"` | 凭据和状态目录；相对路径基于进程工作目录 |
| `api_key` | `""` | 客户端访问密钥 |
| `proxy_url` | `""` | 上游连接代理 |
| `max_concurrent_requests` | `4` | 生成与压缩的并发上限，1～128；计数不占名额 |
| `max_body_bytes` | `33_554_432` | 请求体上限，1 KiB～256 MiB；也作为远程附件下载总预算 |
| `state_ttl_hours` | `168` | 状态有效期，单位小时；1～2,562,047，避免时间长度溢出 |
| `max_state_bytes` | `1_073_741_824` | 状态记录内容的总大小软上限，至少 64 MiB；不等于数据库文件大小 |
| `sanitize_client_markers` | `true` | 已知客户端 system 标记清理开关，用于兼容排障 |

通过 `--config config.toml` 显式指定文件；省略 `--config` 时只使用默认值和环境变量，不自动查找文件。配置在启动时读取，修改后需要重启。未知字段、重复字段、类型错误和非法值会拒绝启动；配置文件上限为 1 MiB。不支持嵌套配置、文件包含或热重载。

从旧 JSON 配置升级时，以 `config.example.toml` 为起点，将保留字段写成 `key = value`，去掉 JSON 的外层花括号和字段间逗号，启动命令改用 `--config config.toml`。删除 `upstream_url`、`user_agent`、`max_state_entries`：上游地址由适配层决定，客户端版本自动更新，记录数量采用内部保护限制。运行时不再接受旧 JSON 配置或这三个字段。`credentials.json` 和 SQLite 数据格式不变，无需重新登录。

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
| 客户端 system 标记清理 | 默认清理 billing 归属行和已知 Claude Code 身份行；业务指令、消息、工具数据与签名保留 |
| `/v1/models` 与 `/v1/models/{id}` | 从账号实时发现，只列出 Gemini 模型，缓存 5 分钟 |
| `/v1/responses`、`/v1/messages` | 流式、非流式、文本和多轮对话；Messages 支持 `system` 角色消息 |
| 真实客户端请求形态 | Codex CLI 0.164 端到端通过，本地压缩和远程压缩都可用；Claude Code 2.1.310 端到端通过（依赖默认开启的客户端 system 标记清理，见下方） |
| 函数工具 | 声明、工具选择、并行调用、工具结果回传、签名恢复；Responses 的 `namespace` 工具组与 `custom` 自由文本工具；未强制调用时使用 Gemini 的 `VALIDATED` 模式，参数受 schema 约束 |
| 工具 schema | 转换为上游接受的子集：内联 `$ref`、合并 `allOf`、类型列表转 `nullable`、`const`/非字符串 `enum` 转换；参数根节点必须是对象，根节点的联合类型保留最具体的分支 |
| 工具名 | Gemini 接受字母、数字、`_`、`.`、`:`、`-`，以字母或下划线开头，最长 128 字符；合法普通函数名原样发送；命名空间、自定义及需规范化的名称使用可读名加 6 位摘要，返回时还原完整身份 |
| 思考内容 | 输出与工具回放通过；支持预算/强度转换、加密回放、篡改检查 |
| 图片与文件输入 | 图片、PDF、HTTPS 图片均通过；Responses 的音频和视频文件输入通过；`gs://` URI 透传尚未实测 |
| JSON 输出与 JSON Schema | 映射到上游；`strict:true` 暂不支持 |
| 原生搜索 | 两个协议的搜索、引用及中文流式引用均通过；引用链接解析为真实网址；Messages 回复附带 `server_tool_use` 和 `web_search_tool_result` 块（Claude Code 的网页搜索从中读取来源链接）；`tool_choice` 指定搜索工具时照常搜索；模型不支持搜索时转给目录中的搜索模型 |
| 图片生成输出 | Responses 图片生成，以及基于生成图片的后续编辑（本轮验收受图片额度阻止，前一轮通过；回放由单元测试覆盖） |
| token 计数 | `/v1/messages/count_tokens`、`/v1/responses/input_tokens` |
| Responses 历史 | `previous_response_id`、查询、删除、`input_items`；SQLite 持久化 |
| 取消、限流、流中错误 | 取消传播到上游；保留 `Retry-After`；断流不返回成功终止事件；并发满时排队等待；空闲流定时心跳 |
| 状态缺失时的降级 | 工具签名记录过期、数据库丢失、会话中途换模型时，丢弃无法恢复的思考块并使用占位签名，会话继续 |
| 以 assistant 结尾的请求 | Antigravity 对所有模型都拒绝以 assistant 结尾的请求（空的 user 轮也不行）。服务会补一个内容为 `Continue.` 的 user 轮，模型会接着已有内容写下去，近似 Anthropic 的预填。末尾只有推理内容的轮次没有可接续的内容，直接丢弃 |
| 上下文压缩 | `/v1/responses/compact`，以及 Codex 远程压缩发出的 `compaction_trigger`。由请求的模型写摘要，结果以 `compaction` 项返回，后续请求带回该项时展开为摘要。用法见下方“Codex 远程压缩” |
| 上游会话 ID | 优先使用客户端自己的会话 ID（Codex 的 `Session-Id` 请求头或 `prompt_cache_key`，Claude Code 的会话 ID），否则按对话的第一条用户消息生成，同一对话的后续请求使用同一个会话。格式与官方客户端一致 |
| 停止序列 | 文本截断通过；精确的 `stop_reason` / `stop_sequence` 匹配元数据尚不能还原 |

尚未实现：Chat Completions（明确不在范围内）、Files 上传服务与 `file_id`、后台任务、WebSocket、Conversations 服务、远程 MCP、托管代码执行、音视频专用接口、显式 prompt cache 控制、严格结构化输出保证，以及完整的平台级参数集合。

请求字段的处理原则：

- 不影响生成结果的字段直接忽略，例如 `cache_control`、`context_management`、`metadata`、`prompt_cache_key`、`client_metadata`、`service_tier`、`text.verbosity`、工具的 `strict` 和 `defer_loading`。
- 其他不认识的字段也会被忽略，字段名写入本地日志的 `warnings`，方便发现新字段。
- 会改变语义而上游做不到的请求明确拒绝，例如 `background`、`conversation`、`prompt` 模板、`item_reference`、`file_id`、远程 MCP 和未支持的托管工具类型。不会通过删掉附件或工具来让这类请求“成功”。
- 尽力满足的参数：`parallel_tool_calls=false` 和 `disable_parallel_tool_use` 没有上游等价设置；`strict` 结构化输出依赖上游的 schema 约束解码；搜索的域名限制（Messages 的 `allowed_domains`/`blocked_domains`、Responses 的 `filters`）和 `user_location` 上游不生效，忽略并记入日志的 `warnings`。

具体模型是否接受某种输入或思考参数，还取决于 Antigravity 上游。

原生搜索不能与函数工具同时发送给上游：两者同时出现时保留函数工具、忽略搜索，并在日志里记录。Codex 默认附带搜索工具，因此不受影响。客户端用 `tool_choice` 显式指定搜索工具时则相反：这一轮去掉函数工具，按搜索请求发出，同样记入日志。历史中已有的函数调用和结果照常保留，实测上游接受未声明的历史调用，前提是它们带签名，而服务端发送前会为缺签名的调用补上占位签名。Gemini 搜索的引用链接是 `vertexaisearch.cloud.google.com` 的跳转地址，标题只有域名；服务会对每个跳转地址发一次不带凭据的 HEAD 请求（经过 `proxy_url`，不跟随跳转），把引用换成真实网址，最多等待 3 秒，未解析的保留原链接。Messages 的搜索回复在正文之后附上 `server_tool_use`（搜索词）和 `web_search_tool_result`（去重后的来源网址和标题）两个块；搜索来源随最后一个数据块到达，所以这两个块排在正文后面，与 Responses 的 `web_search_call` 一致。客户端在后续历史中带回这两种块时，它们会被丢弃并记入日志。实测上游接受 `includedDomains`/`excludeDomains` 但不执行，所以域名限制不转发。

工具选择为 `auto` 或未指定时，函数调用使用 Gemini 的 `VALIDATED` 模式：实测在默认的 `AUTO` 模式下，Gemini 会返回 schema 不允许的参数（例如枚举之外的值），`VALIDATED` 下参数保持在 schema 范围内。强制调用（`required`/指定工具）保持原样。Responses 的 `allowed_tools` 在 `auto` 和 `required` 下都保留允许子集；选择未声明的工具会返回 400。工具选择为 `none` 时关闭原生搜索，不切换搜索模型；函数声明保留，调用模式保持 `NONE`。图片生成目前使用账号提供的图片模型及其默认生成设置，未提供独立 Images API；Messages 不能返回生成图片。上游只对较新的客户端版本开放新模型（实测 `antigravity/1.0.0` 调用 `gemini-3.8-flash-high` 返回 404）。服务在启动时和之后每 6 小时读取 Antigravity 官方发布清单，自动更新 `antigravity/<版本>`；读取失败时沿用已知版本，最低使用内置版本 2.24.0。读取发布清单经过 `proxy_url`，User-Agent 不再作为用户配置。

工具名编码保留可读部分：命名空间工具按 `namespace__name` 拼接，已有 MCP 或命名空间前缀时不重复添加；自定义工具保留原名。非法字符替换为 `_`，首字符不合法时补 `_`。需要编码时，对固定 JSON 数组 `[namespace, original_name, kind]`（`kind` 为 `function` 或 `custom`）计算 SHA-256，取前 6 位十六进制作为后缀。可读部分最多 121 字符，加 `_` 和后缀后总长不超过 128；合法普通函数名包括 `agy_` 开头的名称都保持原样。描述、参数、工具顺序及其他工具增删不影响名字。例如 `files.read` 映射为 `files__read_b88df0`，custom `apply_patch` 映射为 `apply_patch_616f84`。

短摘要不提供唯一性保证：当前声明与历史调用中的不同身份若产生同一个上游名，返回 400，不静默覆盖。签名回放同时校验完整工具身份；历史工具不会因出现在上下文中而自动成为本轮可调用的声明。

客户端回放历史调用时，缺失或空字符串 `namespace` 都视为未指定，不算身份冲突。服务先按同一 `call_id` 的签名记录确认身份（名称、类型和参数都一致时）；记录确认是普通函数时，空 namespace 也是完整身份，不再根据当前声明重解释。无法从记录确认时，优先匹配同名同类型的普通工具声明，再尝试唯一匹配的命名空间工具；多个候选时不猜。补齐或歧义会记入 `warnings`。客户端提供的名称、类型和非空 namespace 保持不变。确认身份后再计算上游名字、检查冲突。身份或参数与记录不一致时，视为历史被修改：不复用原签名，改用占位签名，请求照常进行；补齐后确实发生名字冲突时仍返回 400。可用 `--only tools/readable` 单独验收普通、命名空间、MCP 和 custom 工具的自主选择及第二轮调用。

推理强度映射为 Gemini 的推理等级：`minimal` 按 `low` 发送（部分模型不接受 `minimal`），`xhigh`/`max` 按 `high` 发送。指定思考预算时不低于模型目录的 `minThinkingBudget`；自适应思考使用动态预算。Gemini 无法关闭思考（预算 0 会被部分模型拒绝、被其他模型忽略），因此 `reasoning.effort: "none"` 和 `thinking.type: "disabled"` 使用目录给出的最低预算并不返回思考内容，同时在日志的 `warnings` 中记录。部分 Gemini 模型会只把思考签名放在工具调用或回复文本上，未必返回可见思考块。Messages 的 `thinking.display: "omitted"` 会返回不含思考文本的签名块，回放照常生效。

### Codex 远程压缩

Codex 对自定义服务商默认使用本地压缩：它自己发一次普通请求生成摘要，再用摘要替换历史，这种方式无需服务端额外支持，已可正常使用。在服务商配置里声明远程压缩后，Codex 改为在请求末尾附加 `compaction_trigger`，由 agy2api 生成摘要：

```toml
[model_providers.agy]
name = "agy"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"

[model_providers.agy.capabilities]
remote_compaction = "v2"
```

摘要由请求的模型生成，期间禁止调用工具。`compaction` 项的 `encrypted_content` 携带摘要本身：它只包含客户端自己的对话内容，因此只做编码、不加密，也不依赖本地状态库，状态库丢失后压缩过的会话仍能继续。其他服务商签发的 `compaction` 项无法展开，会被丢弃并记录到日志的 `warnings`，模型将看不到它代表的历史。`/v1/responses/compact` 返回 `response.compaction` 对象，不保存记录；`compaction_trigger` 请求按普通 Responses 请求处理，`store:true` 时保存的上下文只包含摘要。

### 客户端 system 标记清理

`sanitize_client_markers` 默认开启，已有配置省略此项时也会开启。设为 `false` 可以原样转发 system 标记，便于对照排查：

```toml
sanitize_client_markers = false
```

清洗在两个协议统一解码为 system 指令后执行，生成、流式、计数和压缩共用这一步。仅删除以下独立文本行：

- 去掉首尾空白后以 `x-anthropic-billing-header:` 开头的归属行。
- 与以下已知身份句完全匹配的行：`You are Claude Code, Anthropic's official CLI for Claude.`、`You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.`、`You are a Claude agent, built on Anthropic's Claude Agent SDK.`。

同一文本块里的其他指令及其换行保持原样；代码围栏中的示例、引用行、用户消息、工具描述、工具参数、工具结果和已有签名不参与清洗。完全清空的 system 块会移除。没有零宽字符替换、通用关键词过滤或自定义正则规则。

日志的 `warnings` 只记录规则与命中数量，例如 `sanitize_client_markers: billing_header=1 claude_identity=1`，不记录被移除的文本。关闭清洗时，Claude Code 的这些标记可能使 Antigravity 返回 `429 RESOURCE_EXHAUSTED`；实际额度耗尽等其他上游错误仍按原样处理。

Messages 正文后不再追加空的 thinking 签名块，以免 Claude Code 把它当作最后一条消息而输出空 `result`。正文签名改用现有状态库保存，索引绑定模型、会话、system 指令、之前的历史和正文哈希，下一轮 Messages 请求自动恢复；Responses 继续用随响应返回的签名载体，不查这份记录。每段回复正文都会记录，没有签名的也记，因此查不到记录就说明之前的历史有变化（例如客户端删改了更早的思考块）或记录已过期：这些正文不带签名发出，并在日志的 `warnings` 中记录数量。新签名载体使用 `agy2.` 版本，携带经过认证的实际模型来源；历史 `agy1.` 载体仍可按实际执行模型回放。

2026-10-26 使用 Claude Code 2.1.310 对照验证：同一请求关闭清洗时返回 429，开启后返回 200；普通回答及只读工具完整回合通过。Codex 0.164.0、OpenCode V2 2.1.4 的回归也通过。详见[本功能验收记录](docs/client-markers-validation-2026-10-26.json)。

## 数据与状态

默认数据目录：

```text
data/
  credentials.json   OAuth 凭据，权限 0600
  state.db           Responses 历史、工具签名索引和回放加密密钥
```

Responses 默认 `store:true`。设为 `false` 时不保存该响应及其上下文，也不能用其 ID 继续会话；为了工具回合能继续，短期工具签名索引仍保留模型、完整工具身份、上游名称、参数摘要和签名，不保存工具参数原文。Messages 的正文签名也使用有界缓存，仅保存摘要索引和签名，不保存正文原文；缓存未命中时保留正文、继续请求。思考回放数据随响应加密返回客户端。普通保存的会话记录没有数据库级加密，请保护数据目录。新 Responses 记录带版本号、实际模型和持久历史指令。旧记录在读取时保留正文、工具数据并从原始输入恢复 system/developer 消息；由于缺少可靠的实际模型来源，旧思考与签名先降级，再按可验证的工具索引恢复。升级不删除凭据或数据库。存储故障会报错，不会伪装成缓存未命中。

状态默认保留 168 小时（`state_ttl_hours`），内容总计不超过 1 GiB（`max_state_bytes`）；内部另保留 10000 条记录的保护限制，不提供配置覆盖。超限时先清理最旧的记录，上限为软限制，可能短暂超出几十条小记录。记录过期、被清理或数据库丢失后，旧会话仍能继续：无法恢复的思考块会被丢弃，工具调用改用 Antigravity 接受的占位签名，代价是模型看不到之前的思考内容。状态绑定 Antigravity project，更换账号应使用新的数据目录。不要同时运行多个进程使用同一份账号数据。

并发请求达到 `max_concurrent_requests` 后，新请求最多排队 2 分钟，超时才返回 `429`。`count_tokens` 不占用并发名额。模型目录缓存 5 分钟；刷新失败时继续使用旧目录，30 秒后重试。

客户端应把返回的思考块、`encrypted_content`、工具 ID 和参数原样带到下一轮。不要把签名当作可编辑文本；被修改的思考内容会被丢弃，不会发给上游。使用 `previous_response_id` 时，新的顶层 `instructions` 独立生效，不自动继承上一次的顶层指令；历史中的 `input` system/developer 消息会继承；链中途实际执行模型发生变化（包括搜索路由的变化）时，之前的思考内容和签名会被移除。

本地日志每个请求一行，包含状态码、模型和 `warnings`（被忽略的字段、降级、预算调整等）。上游错误的原文只写本地日志，不返回给客户端。

## 验证

```sh
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build -trimpath -o bin/agy2api ./cmd/agy2api
```

测试会启动仅监听本机的模拟上游，无需真实账号，不消耗订阅额度。测试覆盖流式事件顺序、拆分 usage、错误结束、工具第二轮、客户端取消、凭据刷新竞争、401 后强制刷新、SQLite 重启恢复、旧表迁移、状态体积上限、签名篡改、Claude Code 与 Codex 请求形态、schema 转换、工具名映射、状态缺失降级、本机访问限制、排队、目录缓存兜底、流式心跳、以 assistant 结尾的请求、上游会话 ID、上下文压缩、客户端版本更新、函数调用模式、搜索引用链接解析和被忽略的搜索选项。

可选的官方 Python SDK 验收脚本（仅开发测试需要 Python）：

```sh
python -m pip install -r tests/requirements.txt
python -m unittest discover -s tests -p 'test_*.py'
python tests/sdk_smoke.py
```

已用 `openai 3.37.0`、`anthropic 2.3.0` 验证文本、流式聚合、思考与工具回合、token 计数和 Responses 历史接口。脚本从 `cmd/agy2api` 编译临时 CLI 二进制，仅通过链接参数把上游和版本清单地址替换成本机模拟服务；仍走正式的 TOML 配置加载、HTTP 路由及协议实现。测试需要 Go，测试地址不能通过正式配置或环境变量覆盖。临时二进制与账号数据会自动清理。OpenAI usage 中上游未提供的 `cache_write_tokens` 填 0，不代表支持显式缓存写入控制。

真实账号验收脚本会实际消耗订阅额度，需要先完成登录。它在临时目录复制凭据、使用独立 SQLite 状态库，不把测试会话写入正在使用的数据目录：

```sh
python -m pip install -r tests/live-requirements.txt
python tests/live_validate.py --config config.toml --report /tmp/agy2api-live-report.json
# 只复验部分场景：
python tests/live_validate.py --only thinking --report /tmp/agy2api-thinking-report.json
```

验收覆盖模型目录（只列出 Gemini，拒绝其他模型）、两个协议的流式和非流式调用、系统指令、约 4.8 万输入 token 的长上下文、单个/并行工具、工具返回图片、思考及签名回放、结构化输出、图片/PDF/音视频文件、搜索引用（含链接解析）与搜索转发、输出上限、取消、真实 OAuth 刷新、重启恢复、删除、API 密钥验证、并发排队、Claude Code 与 Codex 请求形态、会话中途换模型、状态库丢失后的续聊、以 assistant 结尾的请求、两种上下文压缩方式和客户端版本检查。测试使用合成内容；报告不包含凭据或原始签名。任何 `fail` 都使验收脚本以非零状态退出，`blocked` 和 `partial` 单独报告。原始运行报告属于生成文件，Git 会忽略 `tests/live-report*.json`；`docs/` 只保留整理后的验收记录。

该次验收的限制：

- 同一天的额外验证：Codex CLI 0.164 用 `gemini-3-flash` 完成真实文件任务，并用 `gemini-3-flash` 和 `gemini-3.8-flash-medium` 通过远程压缩完成同一任务；抓取的 Codex（22 个工具）和 Claude Code（23 个工具）请求在 `VALIDATED` 模式下均被接受。
- 图片生成和图片编辑两项受图片模型额度阻止（`429 / QUOTA_EXHAUSTED`，同一天的前一轮验收用掉了额度，上游要求约 3.9 小时后重试）；前一轮验收中这两项通过，生成图片的回放由单元测试覆盖。
- 上游会话 ID 不影响缓存命中：实测 Gemini 按请求前缀缓存，换会话 ID 仍然命中。固定会话 ID 只是让请求与官方客户端一致。
- 该次验收发生在加入客户端标记清理之前；原样转发的 Claude Code 标记会被上游拒绝。当前行为见上方“客户端 system 标记清理”。
- `gemini-2.5-pro`：上游持续返回 `503 / MODEL_CAPACITY_EXHAUSTED`。模型仍保留在目录里，不把暂时无容量误判为永久不支持。
- `stop_sequences` 可以正确截断文本，但上游仅返回 `STOP`，不包含命中的序列。当前 Messages 仍返回 `end_turn` 和 `null`，这两项验收标记为 `partial`，不能视为完整 Anthropic 元数据兼容。
- `gs://` 文件 URI 尚未使用真实可访问对象验收；后台任务等未实现能力仍按上面的范围明确拒绝。
- usage 使用上游提供的字段；部分模型不单独上报思考 token，此时 `reasoning_tokens=0` 不表示没有思考。报告中的 usage 是场景内采样响应的数据，不能相加当作整轮消耗。

本次核心重构的独立验收记录见[架构重构验收](docs/architecture-validation-2026-10-26.json)。记录保留全量运行与定向复验的结果区别；上面的旧验收记录继续作为历史快照。

## 代码结构

```text
cmd/agy2api          login / serve / models / version
internal/auth       单账号 OAuth 与刷新
internal/config     配置及校验
internal/antigravity 原生请求、SSE、模型发现、token 计数和 Gemini schema 转换
internal/protocol   纯协议解码、统一请求规划、版本化回放、共享流聚合与两套输出编码器
internal/server     HTTP 路由、认证、并发控制、媒体下载
internal/state      SQLite 状态与加密载体
internal/securefile 私有文件原子写入
tests/              可选的 SDK 模拟验收与真实账号验收
docs/               已整理的验收记录
```

请求处理只经过一个规划入口：`Decoder` 解析为 `RequestIntent`，保留工具选择、原始工具身份与回放引用；`Planner.Prepare` 负责读取历史、工具选择与实际模型路由、历史拼接、清理、签名恢复、媒体处理和会话规范化，最后生成独立的 `PreparedRequest`。对话内容是否为空在历史合并与回放后检查，允许仅更新指令或触发压缩的续聊请求。HTTP 路由显式指定协议和操作，生成、计数与压缩共用该入口。

`output.go` 只聚合上游分块和推进响应生命周期，`output_state.go` 在完成事件发出前保存回放记录。`encode_messages.go`、`encode_responses.go` 只根据已准备好的块生成协议内容和事件，编码器不持有数据库或上游客户端。流式与非流式响应使用同一份聚合结果。客户端标记规则保留在 `sanitize.go`，Gemini schema 限制归属 `antigravity/schema.go`。

上游协议参考本地 CLIProxyAPI；本项目不依赖 CPA 模块。相关来源与许可保留在 `THIRD_PARTY_NOTICES`。
