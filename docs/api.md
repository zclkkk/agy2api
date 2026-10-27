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

Responses / Messages 生成请求支持 `stream: true`，以 SSE 返回事件。生成 Messages 时必须提供 `max_tokens`。表外的接口不提供，包括 Chat Completions、Realtime、WebSocket、Conversations、Files、Batch 和 Message Batches、Responses 的 cancel。

## 能力分档

服务不实现 Responses / Messages 的全集。每项能力按对客户端的承诺分为三档：

- **保证**：标准承诺的行为都成立。标准不承诺可观察效果的参数也属于这一档，例如缓存提示：接受而不执行仍然符合标准。
- **尽力**：请求被接受并返回合法的响应，但标准的某一条承诺不成立。下表写明每一项不成立的承诺。承诺实际被打破时记录告警；无法判断时，参数出现即告警。
- **拒绝**：忽略会让客户端基于未执行的语义继续行动，例如以为工具存在、内容已被模型看到、结果满足约束或拿到的是事件流。这类请求返回 400。

新参数按以下顺序归档：能按标准实现，或标准不承诺可观察效果，归入保证；否则只要忽略或近似后响应仍然合法、客户端流程不会出错，归入尽力；其余拒绝。服务不通过改写提示词模拟参数，只在请求否则无法执行时注入文本。

告警写入服务日志的 `warnings` 字段，不返回给客户端。未识别的字段被接受并告警。

Codex 和 Claude Code 的日常工作流承诺可用，由真实账号验证覆盖；这两个客户端用到的个别参数属于尽力档，不影响工作流。其他客户端和官方 SDK 按下表执行。

## 保证

通用：

- 流式、非流式生成，token 计数，模型列表。
- 文本对话；instructions、system / developer 消息。Messages 也接受 `messages` 中的 system 消息（Claude Code 使用），并入系统指令。
- `temperature`、`top_p`、Messages 的 `top_k`、最大输出 token。
- function 工具及工具结果，结果可携带图片；`tool_choice` 的自动、禁止、强制及指定工具。选择未声明的工具返回 400。
- 图片、PDF、音视频输入，支持 base64、data URL 和 HTTPS URL。远程附件只从公网地址下载，大小受 `max_body_bytes` 限制；实际接受的媒体类型取决于模型。
- 本服务返回的工具调用 ID、调用参数、思考块和 `encrypted_content` 原样回传后，可继续多轮对话。客户端省略历史调用的 namespace 时，按匹配的记录或唯一声明补齐；确实存在工具身份冲突时返回 400。
- 结束状态：正常结束、达到输出上限、工具调用、安全拦截（Messages 为 `refusal`，Responses 为 `incomplete` 的 `content_filter`）。
- usage 的输入、缓存命中、输出和推理 token。Gemini 使用隐式缓存，Messages 的 `cache_creation_input_tokens` 恒为 0。
- 接受但不产生可观察效果的参数：缓存提示（`cache_control`、`prompt_cache_retention`），服务等级与区域（`service_tier`、`inference_geo`），客户端标识（`user`、`safety_identifier`、`client_metadata`），只限制内置工具的 `max_tool_calls`，只配合代码执行的 `container`，与托管工具相关的 `include` 项，以及 `keep` 为 `all` 的 `clear_thinking` 上下文编辑。

Responses：

- `previous_response_id`、`store` 及已保存响应的查询、删除和输入项分页，见下文“Responses 状态与压缩”。
- `namespace` 工具组、`allowed_tools`；`text.format` 的 `text` 和 `json_object`。
- 请求 `reasoning.summary` 时返回思考摘要；未请求或为 `none` 时不返回思考文本。reasoning 项始终携带 `encrypted_content`。
- `prompt_cache_key` 用作上游会话标识；`metadata`、`parallel_tool_calls` 等请求参数在响应中回显。
- 流式混淆默认开启：每个 delta 事件带有随机字符的 `obfuscation` 字段，把整个事件 JSON 的字节数补齐到 64 的倍数，事件大小不再暴露 token 长度或需要转义的字符。`stream_options.include_obfuscation: false` 时关闭。
- 上下文压缩：`/v1/responses/compact` 和 `compaction_trigger`。

Messages：

- `thinking` 开启时返回思考块；`display: "omitted"` 隐藏思考文本，保留签名。只设置 `output_config.effort` 时不返回思考块。
- Claude Code 的 `defer_loading` 工具和 `tool_reference` 结果。
- document 的 base64、URL、text 和 content 来源；`title` 和 `context` 随文档交给模型。
- `search_result` 块以文本交给模型，可出现在用户消息和工具结果中。

## 尽力

| 能力 | 不成立的承诺 | 告警时机 |
|---|---|---|
| 工具参数、`text.format` 的 JSON Schema、`output_config.format` | 转换为上游支持的子集，不保证 `strict` 语义和完整校验 | `strict: true`，或使用 `output_config.format` |
| custom 自由文本工具 | 映射为单个字符串参数的函数；grammar 只作为说明，不做语法校验 | 声明 grammar 时 |
| 思考强度：`reasoning.effort`、`output_config.effort`、`budget_tokens` | 映射到 Gemini 的档位（`minimal` 为 `low`，`xhigh` / `max` 为 `high`）或预算；预算不低于模型下限 | 档位无法识别时 |
| 关闭思考：`thinking` 的 `disabled`、`reasoning.effort: "none"` | Gemini 无法关闭思考，改用模型允许的最低预算。没有思考块或思考 token 为零，不代表模型没有思考 | 每次 |
| `reasoning.summary` 的 `concise` / `detailed` | 不区分详略，返回上游可用的思考文本 | 每次 |
| `stop_sequences` | 能截断输出，但不报告命中的序列；`stop_reason` 不会是 `stop_sequence` | 每次 |
| `parallel_tool_calls: false`、`disable_parallel_tool_use: true` | 不强制只返回一个工具调用 | 上游产生第二个调用时，流随后中断也会记录 |
| 以 assistant 结尾的请求（预填充） | 补一条 “Continue.” 用户消息，不保证从断点逐字续写 | 每次 |
| 图片 `detail` | 使用模型的默认分辨率 | 非 `auto` 时 |
| document、`search_result` 的 `citations.enabled` | 不生成引用 | 开启时 |
| `text.verbosity` | 不控制回答详略 | 设置时 |
| `top_logprobs`、`include` 的 `message.output_text.logprobs` | 不返回 logprobs，相应字段为空数组 | 设置时 |
| `truncation: "auto"` | 不自动截断输入，超出上下文时返回上游错误 | 设置时 |
| 上下文管理：Messages 的上下文编辑、Responses 的 `context_management` | 不清理历史，不自动压缩 | 设置时 |
| 多轮回放状态 | 工具签名和 Messages 文本签名依赖本地状态；过期、更换实例或数据目录后使用占位签名，无法恢复的思考被丢弃 | 发生时 |
| 对话中切换模型 | 之前的思考和签名被丢弃 | 发生时 |
| Responses 输出项 | reasoning 项不论 `include` 都携带 `encrypted_content`；带签名的文本后会多一个空摘要的 reasoning 项，用于回放签名 | 不告警，始终如此 |

## 拒绝

- 托管和服务端工具。Responses：`web_search`、`file_search`、`code_interpreter`、`image_generation`、`computer_use`、`mcp`；Messages：`web_search`、`web_fetch`、`code_execution`、服务端 tool search、`mcp_servers`。
- 平台预定义、由客户端执行的工具。Responses：`shell`、`local_shell`、`apply_patch`；Messages：`bash`、`text_editor`、`memory`、`computer`。普通函数名不会因为与这些工具重名而被拒绝，客户端应关闭服务不支持的工具。
- 平台功能：`background`、`conversation`、`prompt` 模板、`item_reference` 输入项，以及 Files 引用（`file_id`、`file` 来源）。
- 只有其他服务商才会生成的历史内容，如 Anthropic 服务端工具块、`redacted_thinking` 和 OpenAI 的 `refusal`；对话应从本服务开始。
- 无法表达的内容类型和工具选择、生成图片的模型（Responses / Messages 只接受对话模型）、带 `stream` 或 `starting_after` 的响应查询。

## 模型列表

`models` 命令和 `/v1/models` 展示 Antigravity 模型选择器中的 Gemini 候选，保留上游顺序，并在末尾补充图片生成模型。重复或不在当前 Gemini 目录中的 ID 会跳过；上游未提供对话候选时，仅展示明确列出的图片模型。服务端目录缓存 5 分钟，刷新失败时暂用上次成功的目录。

列表使用完整的上游模型 ID，各 effort 档位分别列出。未展示的 Gemini 模型仍可通过完整 ID 查询或调用，前提是它存在于当前账号的上游目录中，并符合对应接口的能力要求。模型可用性、容量和额度由账号及上游决定，限流响应会保留上游提供的 `Retry-After`。

## Images

图片生成使用 JSON，编辑支持 `multipart/form-data` 文件上传或 JSON 图片引用。请求必须提供图片模型的 `model` 和 `prompt`；以下 `IMAGE_MODEL_ID` 替换为账号可用的图片模型 ID。

```sh
curl http://127.0.0.1:8080/v1/images/generations \
  -H 'Content-Type: application/json' \
  -d '{"model":"IMAGE_MODEL_ID","prompt":"白色背景上的红色圆形"}'

curl http://127.0.0.1:8080/v1/images/edits \
  -F model=IMAGE_MODEL_ID \
  -F prompt='把圆形改为蓝色' \
  -F image=@input.png
```

设置了 `api_key` 时，添加 `Authorization: Bearer <api_key>` 请求头。OpenAI SDK 可使用 `client.images.generate(...)` 和 `client.images.edit(...)`，Base URL 与 Responses 相同。

JSON 编辑请求使用 `images: [{"image_url": "https://example.com/input.png"}]`，也接受 `data:image/png;base64,...`。多文件上传使用重复的 `image[]` 字段。输入支持 PNG、JPEG、WebP，最多 16 张，每张不超过 32 百万像素。上传总大小及远程附件下载大小受 `max_body_bytes` 限制。

Images 采用同样的分档：

| 参数 | 档位与范围 |
|---|---|
| `prompt` | 保证；非空，最多 32000 字符 |
| `n` | 保证；1–10，默认 1，逐张调用上游 |
| `size` | 保证；`auto`（默认）或 `1024x1024`，显式尺寸会校验实际结果，其他尺寸拒绝 |
| `output_format` | 保证；`png`（默认）、`jpeg`，其他格式拒绝 |
| `output_compression` | 保证；JPEG 输出时可设为 0–100，默认 100 |
| `response_format` | 保证；`b64_json`，也可省略；`url` 拒绝 |
| `quality`、`moderation`、`input_fidelity`、`style` | 尽力；接受任意值但不生效，非 `auto` 时告警 |
| `background` | 只接受 `auto`；`transparent`、`opaque` 无法保证，拒绝 |
| `stream`、`partial_images`、`mask` | 拒绝 |

响应的 `data[].b64_json` 是图片 Base64 数据，`output_format` 和 `size` 描述实际输出。JPEG 转换会将透明区域合成到白色背景；服务不缩放图片以伪装上游尺寸。批量结果尺寸不同时省略顶层 `size`。仅在上游提供完整输入模态计数时返回 `usage`，批量请求累加实际用量。

Images 请求不保存会话或生成图片。再次编辑时重新提交图片；批量调用任一失败时整次请求返回错误，已完成的上游调用仍消耗额度。响应图片 Base64 总量最多 64 MiB。不提供 WebP 输出、托管图片 URL、Files 引用或 variations 接口。

## Responses 状态与压缩

Responses 默认 `store: true`，可通过 `previous_response_id` 续聊。新请求的顶层 `instructions` 独立生效；历史 `input` 中的 system/developer 消息会保留。

`GET /v1/responses/{id}/input_items` 返回带稳定 ID 的规范化输入项，支持 `limit`（1–100，默认 20）、`order`（`asc` / `desc`，默认 `desc`）和 `after` 游标。分页元数据包含 `first_id`、`last_id` 和 `has_more`。

输入项 ID 必须在完整历史中唯一；同次输入中重复，或与 `previous_response_id` 引入的历史输入、输出 ID 重复时，返回 400。

`GET /v1/responses/{id}` 返回保存的 JSON；不提供事件重放，带 `stream` 或 `starting_after` 时返回 400。

`store: false` 不保存该响应的会话历史，也不能用其 ID 续聊；工具回合所需的短期签名索引仍会保存。Messages 的文本签名同样依赖本地状态。有效期和容量由[配置](configuration.md)控制。

`/v1/responses/compact` 返回包含摘要的 `response.compaction` 对象，不保存响应记录。客户端也可以在普通 Responses 输入中发送 `compaction_trigger`，获得可用于后续请求的压缩项。

压缩项中的 `encrypted_content` 使用实例密钥加密摘要并验证完整性，不需要保存每个压缩项的服务端记录。恢复时需要保留该实例的数据目录。无效、被修改或来自其他实例及服务商的压缩项返回 400。
