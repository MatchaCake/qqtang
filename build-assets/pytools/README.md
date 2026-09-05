# Python 字节码构建依赖

本目录是随源码提供的第三方 Python 库，供
`scripts/patch-python23-item-registry.py` 处理原始客户端的 Python 2.3 字节码，
写入单人 BOSS 卡、AI 对战卡的道具名称和说明。
构建需要安装 Python 3.10 或更新版本。这些库仅供构建使用，不随游戏发行包分发。

| 随库版本 | 用途 |
| --- | --- |
| xdis 6.1.7 | 读取旧版字节码、指令和代码对象 |
| Click 8.4.2 | xdis 声明的命令行依赖 |
| six 1.17.0 | xdis 声明的兼容依赖 |
| Colorama 0.4.6 | Click 在 Windows 上的依赖 |

这些库和各自的 `.dist-info` 元数据、许可文本一起进入 Git，使字节码补丁步骤无需临时
联网安装 Python 包。第三方许可见 [THIRD_PARTY_NOTICES.md](../../THIRD_PARTY_NOTICES.md)。

更新依赖时保留对应的元数据和许可文件，检查字节码补丁脚本与新版本兼容后再提交。
