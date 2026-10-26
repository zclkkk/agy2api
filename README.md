# agy2api

将一个 Antigravity 账号的 **Gemini 订阅**接入 **OpenAI Responses** 和 **Anthropic Messages** API。

原生单文件部署，支持流式响应、工具调用、思考回放、多模态输入、搜索和上下文压缩。两种 API 格式调用的都是 Gemini 模型。

## 安装

从 [Releases](https://github.com/zclkkk/agy2api/releases) 下载对应平台的发布包并解压。提供 Linux、macOS 的 amd64/arm64 版本，以及 Windows amd64 版本；运行无需 Go、Python 或系统 SQLite。

也可以使用 Go 1.27.x 从源码构建：

```sh
git clone https://github.com/zclkkk/agy2api.git
cd agy2api
CGO_ENABLED=0 go build -trimpath -o agy2api ./cmd/agy2api
```

## 登录与启动

在程序所在目录运行，Windows 使用 `agy2api.exe`：

```sh
cp config.example.toml config.toml
./agy2api login --config config.toml
./agy2api models --config config.toml
./agy2api serve --config config.toml
```

登录时打开终端打印的链接，在浏览器完成授权，再将官方页面显示的授权码粘回终端。本机和 SSH 环境使用相同流程。账号需要已能正常使用 Antigravity，凭据之后会自动刷新。

默认监听 `127.0.0.1:8080`，数据保存在当前目录的 `data/`。对外提供服务或通过域名反向代理访问时，先在配置中设置 `api_key`。完整选项见[配置参考](docs/configuration.md)。

## 调用

将 `MODEL_ID` 替换为 `models` 命令返回的模型 ID。

OpenAI Responses：

```sh
curl -N http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","input":"你好","stream":true}'
```

Anthropic Messages：

```sh
curl -N http://127.0.0.1:8080/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"MODEL_ID","max_tokens":1024,"messages":[{"role":"user","content":"你好"}],"stream":true}'
```

设置了 `api_key` 时，补充 `Authorization: Bearer <api_key>` 请求头；Messages 也接受 `x-api-key`。

接入客户端时，选择对应的 API 格式：Responses 客户端的 Base URL 通常填写 `http://127.0.0.1:8080/v1`，Anthropic SDK 的 Base URL 填写 `http://127.0.0.1:8080`。模型使用服务返回的 Gemini ID。

## 支持范围

- Responses 和 Messages 的流式、非流式生成及 token 计数。
- 函数工具、并行工具回合；Responses 命名空间和自由文本工具。
- 思考内容、签名回放、Responses 历史续聊和上下文压缩。
- 图片、PDF、音视频文件输入；Responses 图片生成与编辑。
- 原生搜索和引用，具体能力取决于账号可用模型。

项目仅面向单账号，不提供 Chat Completions、Files 上传或完整的 OpenAI/Anthropic 平台服务。原生搜索与函数工具不能同轮使用；严格 JSON Schema、精确停止序列元数据等能力受上游限制。详见 [API 与兼容边界](docs/api.md)。

## 更多文档

- [配置参考](docs/configuration.md)
- [API 与兼容边界](docs/api.md)
- [开发与发布](docs/development.md)

上游协议参考 CLIProxyAPI；agy2api 不依赖 CPA 模块。相关来源和许可见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)。
