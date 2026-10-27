# 开发与发布

以下命令在仓库根目录执行。需要 Go 1.27.x；Python 脚本需要 Python 3.11 或以上版本。

## 构建

```sh
CGO_ENABLED=0 go build -trimpath -o bin/agy2api ./cmd/agy2api
./bin/agy2api version
```

开发构建的版本显示为 `dev`。发布构建由打包脚本注入版本号。

## 测试

```sh
go vet ./...
go test -race ./...
```

官方 Python SDK 的模拟测试使用本机服务，不需要账号：

```sh
python -m pip install -r tests/requirements.txt
python -m unittest discover -s tests -p 'test_*.py'
python tests/sdk_smoke.py
```

SDK 脚本从正式 CLI 入口编译临时程序，仅在构建时替换上游与版本清单地址。临时程序和数据在结束时清理。

真实账号测试需要已有登录凭据，会消耗订阅额度。脚本复制凭据到临时目录并使用独立状态库：

```sh
python -m pip install -r tests/live-requirements.txt
python tests/live_validate.py --config config.toml
# 仅运行指定场景：
python tests/live_validate.py --config config.toml --only tools/readable
```

报告默认写入 `.cache/validation/live-report.json`，可通过 `--report` 指定其他位置。`fail` 会使脚本非零退出；上游阻挡和覆盖受限单独记录。

测试报告、请求抓取、日志和一次性调试材料属于本地开发产物，放在被忽略的 `.cache/` 中，不提交到版本树，也不写入 README 或发布说明。可复用的测试代码保留在仓库。

## 发布包

从准备发布的干净提交构建：

```sh
python scripts/release.py --version v0.1.0
# 只构建一个目标：
python scripts/release.py --version v0.1.0 --target linux/amd64
```

默认目标为 Linux、macOS 的 amd64/arm64 和 Windows amd64，输出到 `dist/<version>/`。脚本不覆盖已有输出；可用 `--output` 指定其他目录。

每个包包含程序、配置示例、用户文档和许可证。`SHA256SUMS` 用于校验资产，`BUILD.json` 和 `manifest.json` 记录版本、目标平台及来源提交。脚本不会创建 tag 或上传 GitHub Release。

对应版本的发布说明放在 `docs/releases/<version>.md`，只描述用户可见的功能、变更和限制。

## 代码组织

| 目录 | 职责 |
|---|---|
| `cmd/agy2api` | 命令行和登录交互 |
| `internal/auth` | OAuth 凭据及刷新 |
| `internal/config` | TOML 配置及校验 |
| `internal/antigravity` | 上游请求、模型发现、SSE 和 schema 转换 |
| `internal/protocol` | 协议解码、请求规划、历史回放、响应编码和图片结果处理 |
| `internal/server` | HTTP 路由、认证、并发控制与媒体下载 |
| `internal/state` | 有界的 SQLite 状态及密封载体 |
| `internal/securefile` | 私有文件的原子写入 |

Responses / Messages 请求经过 `Decoder → Planner → PreparedRequest`，共享上游流聚合与状态保存，再由各自的编码器生成响应。生成、计数和压缩共用请求规划入口。

Images 请求独立解码为 `ImageIntent`，复用上游客户端和服务端的认证、并发限制及媒体下载。`ImageCollector` 校验完整图片并编码输出，批量调用逐张执行，不使用对话状态库。

## 协议参数的处理原则

服务不实现 Responses / Messages 的全集。新增或评审协议参数时，按对客户端的承诺归入三档：

1. **保证**：能按标准语义实现，或标准不承诺可观察效果（如缓存提示），按标准处理，不告警。
2. **尽力**：忽略或近似处理后响应仍然合法、客户端流程不会出错时接受，并写明哪条标准承诺不成立。承诺实际被打破时告警；无法判断时，参数出现即告警。
3. **拒绝**：忽略会让客户端基于未执行的语义继续行动时，返回 400。

不通过改写提示词模拟参数，只在请求否则无法执行时注入文本；没有客户端使用的能力不实现。[API 与兼容边界](api.md)只记录会影响用法的差异，标准行为和内部机制不写入用户文档。
