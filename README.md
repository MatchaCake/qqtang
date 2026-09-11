# QQ堂怀旧版使用说明

> **本项目永久免费，请勿付费购买。** 公开获取地址：<https://github.com/kuuhaku1314/qqtang>

## 普通玩家

注：较旧的 Windows 或未安装运行库的 Windows Server 启动时，可能提示 `onnxruntime error` 或 DLL 初始化失败。请安装 [Microsoft Visual C++ x64 运行库](https://aka.ms/vc14/vc_redist.x64.exe)，安装完成后重新启动服务端。

完整解压 `QQTang-Local` 后双击 `QQTang-Launcher.exe`，启动本机服务端和客户端。默认账号为 `1000001`，初始密码为 `123456`；请勿输入真实 QQ 密码。重复点击“启动一个客户端”即可多开，启动器不设置固定客户端数量上限。

## TP 已完全移除

发布版 `Client.exe` 已重建为普通 PE 启动链，不再启动或依赖旧版腾讯 TP。原版 `ClientBase.dll`、`TerSafe.dll`、`TenSLX.dll`、`TP3Helper.exe` 与 `TesSafe.sys` 均未随包分发。包内同名 `TerSafe.dll` 是本项目生成的 2 KB 无保护协议 ABI 兼容层，只补齐老客户端模块之间仍会调用的对象和数据变换接口；它不含原版 TP 代码，不进行安全扫描，也不会启动保护服务。运行时不会安装、注册或加载 TP 内核驱动。

## 无界面服务端脚本

完整发布包现在同时提供：

- `start-server-windows.cmd`：Windows AMD64
- `start-server-linux-amd64.sh`：Linux AMD64 / x86_64
- `start-server-linux-arm64.sh`：Linux ARM64 / aarch64

Linux 使用示例：

```sh
chmod +x start-server-linux-*.sh
./start-server-linux-amd64.sh
```

ARM64 主机运行 `./start-server-linux-arm64.sh`。三个脚本都会在前台运行，按 `Ctrl+C` 停止；也支持由 systemd、Docker 等通过 `SIGTERM` 托管。

脚本读取 `configs/network.json`：`local` 只监听本机，`lan-host` 用于局域网主机，`remote-host` 用于公网服务器。公网模式的 `server_ip` 与 `client_server_ip` 可填写玩家可访问的真实 IPv4 或域名；域名会解析为 IPv4 A 记录后写入客户端配置及原生目录协议。Linux 服务端仍需要完整发布包中的 `configs`、`data` 与 `runtime/client-patched` 静态配置，不能只复制服务端二进制。AMD64 与 ARM64 启动脚本会分别加载包内对应架构的 ONNX Runtime `.so`。

公网或局域网主机需自行放行 TCP `17000/18000/18001/18080/18443` 和 UDP `18000`。脚本不会自动修改防火墙、提权或关闭安全功能。

## 存档、GM 与日志

- 存档：`runtime/data/qqtang.sqlite`，升级或迁移前请自行备份。
- GM：默认 `http://127.0.0.1:18100/gm/`；远程开放前必须设置强密码。
- 服务端日志：`runtime/logs/server-local.jsonl`，图形启动还会写入 `local-server.stdout.log` 与 `local-server.stderr.log`。
- 客户端日志：`runtime/logs/local-client-launch*.json` 与 `local-launcher*.stderr.log`。

不要关闭 Windows Defender、防火墙，也不要从第三方 DLL 网站下载系统文件。

远程 GM 的控制台配置方式：先在 `configs/network.json` 中将 `gm_remote` 设为 `true`，然后首次启动时把密码从标准输入传给服务端（密码不会出现在命令行或明文配置中）：

```sh
read -r -s -p 'GM password: ' QQTANG_GM_PASSWORD; echo
printf '%s\n' "$QQTANG_GM_PASSWORD" | ./start-server-linux-amd64.sh -gm-username admin -gm-password-stdin
unset QQTANG_GM_PASSWORD
```

ARM64 替换启动脚本名。Windows PowerShell 可使用：

```powershell
$credential = Get-Credential -UserName admin
$credential.GetNetworkCredential().Password | .\start-server-windows.cmd -gm-username $credential.UserName -gm-password-stdin
```

凭据保存为 `configs/gm-auth.json` 中的 PBKDF2 加盐校验值；后续直接运行启动脚本即可。
