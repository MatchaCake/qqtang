# 构建资源

这里保存可重建发行包所需的补全资源缓存、项目生成的 TP-free 客户端入口、Python 2.3 字节码处理依赖以及 Windows/Linux ONNX Runtime。

这些文件随源码仓库一同版本化，正常情况下不需要手工填充。Linux ONNX Runtime 仍由 `scripts/ensure-onnxruntime-linux.ps1` 校验；缺失时会从官方发布包恢复。

目录结构：

```text
build-assets/
  client-no-tp/
  resource-cache/
  pytools/
  onnxruntime/
    windows-amd64/
    linux-amd64/
    linux-arm64/
```

原始 QQ堂客户端不属于构建资源，必须由使用者自行放入 `client/original/`。

Python 库的用途、版本和更新方法见 [pytools/README.md](pytools/README.md)。
