# 服务端架构

服务端由协议适配、应用服务、游戏规则和持久化组成。启动器负责进程管理和客户端配置，AI 推理通过战斗引擎参与对局。

## 模块入口

| 模块 | 职责 |
| --- | --- |
| [cmd/qqt-server](../cmd/qqt-server) | 服务端启动入口 |
| [internal/server/probe](../internal/server/probe) | TCP/UDP 接入、身份校验、消息分发与客户端协议投影 |
| [internal/server/application](../internal/server/application) | 账号、库存、社交、房间和结算用例 |
| [internal/server/persistence](../internal/server/persistence) | SQLite 存储、迁移与事务 |
| [internal/server/gm](../internal/server/gm) | GM 管理接口和认证 |
| [internal/server/networksetup](../internal/server/networksetup) | 网络配置、地址解析与客户端地址写入 |
| [internal/protocol](../internal/protocol) | 协议类型、编解码、包络与加密 |
| [internal/game](../internal/game) | 大厅、房间、对局状态和游戏规则 |
| [internal/clientdata](../internal/clientdata) | 客户端静态数据语义 |
| [internal/tooling](../internal/tooling) | 启动、资源处理与客户端补丁工具 |

协议适配层将请求转换为应用操作，并将结果编码给客户端。业务状态与事务由应用层和游戏模块处理；协议编解码集中在相应协议包中。

## 账号与房间状态

[PlayerService](../internal/server/application/player_service.go) 组织人物、库存和结算操作，通过仓储访问 SQLite。人物、装备、宠物及社交关系保存在数据库中，客户端消息是这些状态的协议投影。

[World](../internal/server/application/world.go) 管理大厅和房间，包括成员、座位、队伍、准备状态以及开局和返回房间。TCP 会话保留连接身份和路由信息，房间成员关系由 World 管理。

[internal/game/match](../internal/game/match) 管理竞技和探险对局状态。结算通过应用层写入数据库；重复消息不能造成重复发奖或重复扣除库存。正常结算结束当前对局并保留房间。

## 对局同步与 AI

客户端负责场景演算和画面表现。服务端校验玩家身份、房间和对局归属，处理中继、规则事件与结算。实时数据和业务通知有各自的协议通道，见 [协议说明](protocol-overview.md)。

普通规则 1 的 AI 使用以下模块：

| 模块 | 职责 |
| --- | --- |
| [battleengine](../internal/game/battleengine) | 移动、碰撞、糖泡、拾取、道具效果和受击规则 |
| [battleenv](../internal/game/battleenv) | 将引擎状态编码为模型观察 |
| [battleai](../internal/game/battleai) | 模型加载、推理和角色记忆 |
| [competitive_ai_runtime.go](../internal/server/probe/competitive_ai_runtime.go) | 对接真实对局，接收客户端事件并投影 AI 移动与动作 |

同一房间的 AI 运行时维护共享场景状态，各 AI 根据自身观察选择动作。引擎推进、模型决策和网络发送分别调度；引擎帧率不等于模型推理频率。移动投影保留客户端使用的方向、动作时间和位置语义，模型只负责选择动作。

模型文件及更新方法见 [configs/models/README.md](../configs/models/README.md)。

## 配置与静态数据

[configs](../configs) 保存运行参数、玩法配置和模型；[data](../data) 保存物品、配方等结构化数据。服务端还会从发布包的 `runtime/client-patched` 读取客户端静态配置，因此部署时需要保留完整发布包结构。

网络配置和存档位置见 [部署说明](network-deployment.md)。

## 开发验证

在仓库根目录运行：

```sh
go test ./... -count=1
go vet ./...
go build ./cmd/...
```

协议变更应覆盖编解码和消息路由；状态变更应覆盖持久化及重复事件；AI 适配变更应同时检查引擎行为与客户端投影。
