# QQ堂本地怀旧版开发说明

本仓库包含本地服务端、启动器、协议与游戏规则实现、静态客户端兼容补丁以及发行包构建脚本。原始 QQ堂客户端不进入仓库；普通玩家应直接从 GitHub Releases 下载 `QQTang-Local.zip`。

本项目源码采用 [Apache License 2.0](LICENSE)。原始客户端不在该许可范围内；随仓库提供的第三方组件继续遵循各自许可，详见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 从源码构建

1. 克隆仓库。
2. 自行取得兼容的原始客户端，按 [client/original/README.md](client/original/README.md) 解压到 `client/original/`。
3. 运行 `build-release.cmd`。

仓库已经包含补全资源和各平台运行时。构建脚本从只读原始客户端生成：

```text
release/QQTang-Local/
release/QQTang-Local.zip
```

完整工具链要求见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 源码结构

```text
cmd/                服务端、启动器和发行构建工具入口
internal/           协议、房间、战斗、持久化和启动器核心实现
configs/            默认运行规则与正式 AI 模型
data/               服务端规范数据
deploy/             Windows/Linux 发布启动脚本
scripts/            公开发行构建脚本
client/original/    用户自行放置的原始客户端，不进入 Git
build-assets/       版本化的补全资源与第三方运行时
```

开发参考：[架构说明](docs/server-architecture.md)、[协议说明](docs/protocol-overview.md)、[网络部署](docs/network-deployment.md)、[物品 ID](docs/item-id-catalog.md) 和 [AI 模型](configs/models/README.md)。

## 开发验证

```powershell
go test ./... -count=1
go vet ./...
go build ./cmd/...
```

请勿提交原始客户端、账号数据库、运行日志、抓包或私人配置。
