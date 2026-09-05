# 协议说明

服务端兼容客户端的目录、登录、大厅、房间、商城及对局协议。协议定义位于 [internal/protocol](../internal/protocol)，网络处理入口位于 [internal/server/probe](../internal/server/probe)。

## 协议层次

| 层次 | 内容 | 源码入口 |
| --- | --- | --- |
| 目录 | 区服地址与大厅信息 | [directory](../internal/protocol/directory) |
| 游戏服务包络 | 帧长度、序号、身份、路由和 QQ-TEA 载荷 | [local_packet.go](../internal/protocol/game/local_packet.go) |
| 业务命令 | 登录、房间、商城、社交等请求与响应 | [commands.go](../internal/protocol/game/commands.go) |
| 房间快速包 | 等待房间事件与对局数据的 TCP 上传包络 | [room_fast_packet.go](../internal/protocol/game/room_fast_packet.go) |
| QQTPPP | UDP 会话登记、控制与实时数据转发 | [room_peer_udp.go](../internal/server/probe/room_peer_udp.go) |

游戏服务 TCP 帧以四字节大端总长度开头，长度包含这四个字节。游戏包解密后包含命令、路由头和业务正文。顶层命令号、正文 schema ID 和场景事件 ID 属于不同层次；同号请求与响应也可能使用不同正文结构。

命令号及方向以 `commands.go` 的注册表为准，跨消息枚举位于 [enums.go](../internal/protocol/game/enums.go)，具体消息的布局和长度校验与其编解码实现放在一起。

## 对局数据流

客户端通过 TCP 登录建立身份，再使用 QQTPPP UDP 登记实时端点。服务端按已认证身份和当前房间成员关系处理目标转发。

实时场景数据使用 QQTPPP Type 2。正式对局中的 TCP 快速上传是同批事件的可靠镜像，用于服务端校验、记账和结算；它不会再次生成一份独立场景广播。等待房间事件与正式对局事件按各自阶段处理。实现见 [room_fast_dispatch.go](../internal/server/probe/room_fast_dispatch.go) 和 [room_peer_udp.go](../internal/server/probe/room_peer_udp.go)。

普通业务请求和主动通知使用对应的游戏服务包络。转发时保留场景事件需要的批次、顺序和身份，跨通道重复执行会影响放泡、拾取等有状态操作。

AI 动作由 [competitive_ai_runtime.go](../internal/server/probe/competitive_ai_runtime.go) 转换为客户端可消费的移动和场景事件；客户端仍按自身规则演算和显示。

## 修改协议代码

- 根据消息所属层次修改对应类型和编解码器，业务处理器使用已解析字段。
- 将网络字节序与客户端进程内存布局分开处理，不能用 Go 结构体大小代替协议长度。
- 校验身份、声明长度、数组边界和所属对局；对协议允许的重复或重排保持幂等。
- 为改变的布局保留字节向量与编解码测试，为路由变化检查接收者、消息顺序和重复消费。

端口和联网配置见 [部署说明](network-deployment.md)，服务端模块关系见 [架构说明](server-architecture.md)。
