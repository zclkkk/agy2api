# API 与兼容边界

服务提供 OpenAI Responses、Images 和 Anthropic Messages 接口，调用 Antigravity 账号可用的 Gemini 模型；使用 `/v1/models` 获取模型 ID。

## 接口

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/healthz` | 健康检查，无需认证 |
| `GET` | `/v1/models` | 模型列表 |
| `GET` | `/v1/models/{id}` | 模型信息 |
| `POST` | `/v1/responses` | Responses 生成 |
| `POST` | `/v1/responses/input_tokens` | Responses 输入 token 计数 |
| `POST` | `/v1/responses/compact` | 压缩上下文 |
| `GET` | `/v1/responses/{id}` | 查询已保存的响应 |
| `DELETE` | `/v1/responses/{id}` | 删除已保存的响应 |
| `GET` | `/v1/responses/{id}/input_items` | 查询保存的输入项 |
| `POST` | `/v1/messages` | Messages 生成 |
| `POST` | `/v1/messages/count_tokens` | Messages 输入 token 计数 |
| `POST` | `/v1/images/generations` | 图片生成 |
| `POST` | `/v1/images/edits` | 图片编辑 |

Responses / Messages 生成请求支持 `stream: true`，以 SSE 返回事件。生成 Messages 时必须提供 `max_tokens`。表外的接口不提供，包括 Chat Completions、Realtime、Files、Batch 和 Message Batches。

## 与标准的差异

未提到的参数和行为与 OpenAI、Anthropic 的标准一致。降级处理和未识别的字段会写入服务日志的 `warnings`，不影响请求。

### 不支持的功能

以下请求返回 400：

- 托管和服务端工具，如网页搜索、文件搜索、代码执行、图片生成和远程 MCP。
- 平台预定义、由客户端执行的工具：Responses 的 `shell`、`local_shell`、`apply_patch`，Messages 的 `bash`、`text_editor`、`memory`、`computer`。同名的普通函数工具不受影响。
- 平台功能：`background`、`conversation`、`prompt` 模板、`item_reference`、Files 引用（`file_id` 和 `file` 来源），以及带 `stream` 或 `starting_after` 的响应查询。
- 其他服务商生成的历史内容，如 Anthropic 服务端工具块、`redacted_thinking` 和 OpenAI 的 `refusal`。对话应从本服务开始。
- 在 Responses / Messages 中使用图片生成模型；图片生成请使用 Images 接口。

### 行为差异

- **思考**：Gemini 无法完全关闭思考，请求关闭时使用模型允许的最低预算。思考强度映射到 Gemini 的档位（`minimal` 为 `low`，`xhigh` / `max` 为 `high`）；`reasoning.summary` 不区分详略。
- **结构化输出**：工具参数、`text.format` 和 `output_config.format` 的 JSON Schema 转换为上游支持的子集，不保证 `strict` 语义。自定义工具的 grammar 只作为说明，不做语法校验。
- **并行工具调用**：`parallel_tool_calls: false` 和 `disable_parallel_tool_use` 无法生效，模型仍可能一次返回多个调用。
- **停止序列**：`stop_sequences` 能截断输出，但不报告命中的序列，`stop_reason` 不会是 `stop_sequence`。
- **预填充**：以 assistant 消息结尾的请求会补一条 “Continue.” 用户消息，模型不一定从断点逐字续写。
- **接受但不生效**：`text.verbosity`、logprobs、`truncation: "auto"`、上下文管理（Messages 的上下文编辑、Responses 的 `context_management`）、引用（`citations`）和图片 `detail`。
- **附件**：HTTPS 附件由服务下载，只允许公网地址，总大小受 `max_body_bytes` 限制。实际接受的媒体类型取决于模型。
- **输出结构**：Responses 的 reasoning 项总是携带 `encrypted_content`，输出中还可能多出不含摘要的 reasoning 项，用于回放签名。
- **扩展**：Messages 的 `messages` 中也接受 system 消息，并入系统指令。

### 依赖本地状态

- 多轮对话中，工具调用、思考块和 `encrypted_content` 的签名依赖本服务的数据目录。客户端应原样回传服务返回的这些内容。
- 状态按账号保存：响应 ID 和压缩项只能用签发它们的账号的 Key 访问，其他账号返回 404 或 400。
- 状态过期、换用其他实例或数据目录后，携带完整历史的对话仍可继续，但无法恢复的思考会被丢弃；依赖 `previous_response_id` 或压缩项的续聊会返回 400。有效期和容量见[配置参考](configuration.md)。
- 对话中切换模型时，之前的思考会被丢弃。

## Responses 状态与压缩

Responses 默认 `store: true`，可用 `previous_response_id` 续聊；已保存的响应可以查询、删除，并分页读取输入项。`store: false` 不保存会话历史，但工具回合所需的签名记录仍会保存。

`/v1/responses/compact` 返回 `response.compaction` 对象。也可以在输入中发送 `compaction_trigger`（Codex 使用），在普通响应中获得压缩项。压缩项由实例密钥加密，只能由使用同一数据目录的服务展开；其他实例或服务商签发的压缩项返回 400。

## Images

图片生成使用 JSON；编辑支持 `multipart/form-data` 上传或 JSON 图片引用。`model` 须为图片生成模型，以下 `IMAGE_MODEL_ID` 替换为账号可用的图片模型 ID：

```sh
curl http://127.0.0.1:8080/v1/images/generations \
  -H 'Content-Type: application/json' \
  -d '{"model":"IMAGE_MODEL_ID","prompt":"白色背景上的红色圆形"}'

curl http://127.0.0.1:8080/v1/images/edits \
  -F model=IMAGE_MODEL_ID \
  -F prompt='把圆形改为蓝色' \
  -F image=@input.png
```

OpenAI SDK 的 `client.images.generate(...)` 和 `client.images.edit(...)` 可直接使用。JSON 编辑请求使用 `images: [{"image_url": "..."}]`，支持 HTTPS 和 data URL；多文件上传使用重复的 `image[]` 字段。输入支持 PNG、JPEG、WebP，最多 16 张，每张不超过 32 百万像素。

| 参数 | 支持范围 |
|---|---|
| `prompt` | 非空，最多 32000 字符 |
| `n` | 1–10，默认 1 |
| `size` | `auto`（默认）或 `1024x1024` |
| `output_format` | `png`（默认）、`jpeg` |
| `output_compression` | JPEG 输出时 0–100，默认 100 |
| `response_format` | `b64_json`，或省略 |
| `quality`、`moderation`、`input_fidelity`、`style` | 接受但不生效 |
| `background` | 仅 `auto` |
| `stream`、`partial_images`、`mask` | 不支持 |

表外的取值返回 400。结果以 `b64_json` 返回，JPEG 输出会把透明区域合成为白色。`n` 大于 1 时逐张调用上游，任一张失败则整次请求失败，已完成的调用仍消耗额度。仅在上游提供完整的输入模态计数时返回 `usage`。

## 模型列表

`/v1/models` 只列出 Antigravity 自身使用的模型。图片生成模型只用于 Images 接口，因此不出现在 Anthropic 格式（带 `anthropic-version` 请求头）的列表和查询中。

账号上游目录中的其他 Gemini 模型虽未列出，仍可按完整 ID 查询和调用。上游标记为禁用的模型视为不存在，调用返回 400。模型可用性、容量和额度由账号及上游决定；限流响应保留上游提供的 `Retry-After`。
