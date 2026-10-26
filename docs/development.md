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
| `internal/protocol` | 两种协议的解码、请求规划、历史回放和响应编码 |
| `internal/server` | HTTP 路由、认证、并发控制与媒体下载 |
| `internal/state` | 有界的 SQLite 状态及密封载体 |
| `internal/securefile` | 私有文件的原子写入 |

请求经过 `Decoder → Planner → PreparedRequest`。两个协议共享上游流聚合与状态保存，再由各自的编码器生成响应。生成、计数和压缩共用请求规划入口。
