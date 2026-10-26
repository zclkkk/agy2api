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

Responses / Messages 生成请求支持 `stream: true`，以 SSE 返回事件。生成 Messages 时必须提供 `max_tokens`。

## 工具与历史回放

支持普通函数工具及工具结果回传；Responses 另支持 `namespace` 工具组和 `custom` 自由文本工具。工具名、命名空间和类型在客户端接口上保持一致。

不支持的工具类型或工具选择返回 400；普通函数名不会因为与托管工具重名而被拒绝。客户端应关闭服务不支持的托管工具。

`tool_choice` 支持自动、禁止、强制及指定工具；Responses 的 `allowed_tools` 保留允许调用的子集。选择未声明的工具会报错。`parallel_tool_calls=false` 和 `disable_parallel_tool_use` 只能尽力满足。

后续请求应原样携带返回的工具 ID、调用参数、思考块和 `encrypted_content`。客户端省略历史调用的 namespace 时，服务会尝试根据匹配的记录或唯一声明补齐。记录缺失或历史被修改时，不复用不匹配的签名；确实存在工具身份冲突时返回 400。

## 思考与结构化输出

支持 Messages 的 thinking 设置和 Responses 的 reasoning 设置，并转换为上游支持的预算或等级。实际预算、可见思考和 usage 字段由模型决定。

Gemini 无法保证彻底关闭思考；请求关闭时使用模型允许的最低预算。没有可见 thinking 块或思考 token 计数为零，不代表模型没有思考。

支持 JSON 输出和 JSON Schema 转换，但不保证 `strict` 语义。工具和响应 schema 会转换成上游支持的子集。

## 媒体

支持图片、PDF、音视频文件内容输入，实际接受的媒体类型取决于模型。普通函数的工具结果也可以携带图片。`file_id` 引用和 Files 上传接口不在支持范围内。

模型列表反映账号可用的 Gemini 模型。图片生成模型用于 Images API；Responses / Messages 的生成、计数和压缩接口不接受这类模型。服务不提供独立搜索接口。

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

| 参数 | 支持范围 |
|---|---|
| `prompt` | 非空，最多 32000 字符 |
| `n` | 1–10，默认 1；逐张调用上游 |
| `size` | `auto`（默认）或 `1024x1024`；显式尺寸会校验实际结果 |
| `output_format` | `png`（默认）、`jpeg` |
| `output_compression` | JPEG 输出时可设为 0–100，默认 100 |
| `response_format` | `b64_json`，也可省略 |
| `quality`、`background`、`moderation` | 省略或 `auto` |

响应的 `data[].b64_json` 是图片 Base64 数据，`output_format` 和 `size` 描述实际输出。JPEG 转换会将透明区域合成到白色背景；服务不缩放图片以伪装上游尺寸。批量结果尺寸不同时省略顶层 `size`。仅在上游提供完整输入模态计数时返回 `usage`，批量请求累加实际用量。

Images 请求不保存会话或生成图片。再次编辑时重新提交图片；批量调用任一失败时整次请求返回错误，已完成的上游调用仍消耗额度。响应图片 Base64 总量最多 64 MiB。

当前不支持流式输出、渐进预览、蒙版、`input_fidelity`、WebP 输出、托管图片 URL、Files 引用或 variations 接口。不支持的显式生成选项会返回 400。

## Responses 状态与压缩

Responses 默认 `store: true`，可通过 `previous_response_id` 续聊。新请求的顶层 `instructions` 独立生效；历史 `input` 中的 system/developer 消息会保留。

`store: false` 不保存该响应的会话历史，也不能用其 ID 续聊；工具回合所需的短期签名索引仍会保存。Messages 的文本签名同样依赖本地状态。有效期和容量由[配置](configuration.md)控制。

`/v1/responses/compact` 返回包含摘要的 `response.compaction` 对象，不保存响应记录。客户端也可以在普通 Responses 输入中发送 `compaction_trigger`，获得可用于后续请求的压缩项。

压缩项中的 `encrypted_content` 携带编码后的摘要，不用于加密存储。服务无法展开其他服务商签发的压缩项。

## 其他边界

- 不提供 Chat Completions、WebSocket、Conversations 服务、后台任务、远程 MCP、托管代码执行或独立的音视频接口。
- 停止序列可截断文本，但上游未必报告命中的序列；此时 Messages 不能还原精确的 `stop_reason` 和 `stop_sequence`。
- 以 assistant 结尾的普通对话会补上继续请求，以满足上游要求。
- 未识别的扩展字段通常会忽略；不支持的内容类型、附件引用或工具类型会明确报错。转换和降级信息写入日志的 `warnings`。
- 模型可用性、容量和额度由账号及上游决定。限流响应会保留上游提供的 `Retry-After`。
