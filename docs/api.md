# API 与兼容边界

服务提供 OpenAI Responses 和 Anthropic Messages 格式。两种格式都调用 Antigravity 账号可用的 Gemini 模型；使用 `/v1/models` 获取模型 ID。

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

生成请求支持 `stream: true`，以 SSE 返回事件。生成 Messages 时必须提供 `max_tokens`。

## 工具与历史回放

支持普通函数工具及工具结果回传；Responses 另支持 `namespace` 工具组和 `custom` 自由文本工具。工具名、命名空间和类型在客户端接口上保持一致。

`tool_choice` 支持自动、禁止、强制及指定工具；Responses 的 `allowed_tools` 保留允许调用的子集。选择未声明的工具会报错。`parallel_tool_calls=false` 和 `disable_parallel_tool_use` 只能尽力满足。

后续请求应原样携带返回的工具 ID、调用参数、思考块和 `encrypted_content`。客户端省略历史调用的 namespace 时，服务会尝试根据匹配的记录或唯一声明补齐。记录缺失或历史被修改时，不复用不匹配的签名；确实存在工具身份冲突时返回 400。

## 思考与结构化输出

支持 Messages 的 thinking 设置和 Responses 的 reasoning 设置，并转换为上游支持的预算或等级。实际预算、可见思考和 usage 字段由模型决定。

Gemini 无法保证彻底关闭思考；请求关闭时使用模型允许的最低预算。没有可见 thinking 块或思考 token 计数为零，不代表模型没有思考。

支持 JSON 输出和 JSON Schema 转换，但不保证 `strict` 语义。工具和响应 schema 会转换成上游支持的子集。

## 媒体与搜索

支持图片、PDF、音视频文件内容输入，实际接受的媒体类型取决于模型。生成和编辑图片需要选择账号提供的图片模型，通过 Responses 返回；Messages 不返回生成图片。`file_id` 引用和 Files 上传接口不在支持范围内。

两个协议都支持原生搜索及引用。请求的模型不支持搜索时，服务可能改用账号提供的搜索模型。搜索引用随上游结果返回，搜索结果块可能排在回答正文之后。

搜索与函数工具不能同轮发送给上游：同时声明时默认使用函数工具；显式选择搜索工具时使用搜索。`tool_choice: none` 会禁用两者。域名过滤和地理位置限制在上游不生效，会忽略并记录日志。

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
