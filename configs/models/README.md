# 普通规则 1 推理模型

发布文件按玩法和用途命名，不把训练轮次或“最强”判断写进固定路径：

| 文件 | 用途 |
| --- | --- |
| `qqtang-rule1.onnx` | 当前默认部署模型，使用 ONNX Runtime CPU 推理 |
| `qqtang-rule1.onnx.json` | 配套元数据：接口版本、张量规格、训练版本、来源 checkpoint 与 SHA256 |

当前 ONNX 版本为 **V255-U32**，以元数据的 `checkpoint_label` 和 `onnx_sha256` 确认身份。
发布版统一使用随包的 ONNX Runtime 在 CPU 上推理，无需安装 Python 或启用 GPU。
不再分发旧 `.qtai`；它已不匹配当前网络结构，不能作为当前模型的等价兜底。
更新默认模型时同时替换 ONNX 和配套 JSON，保持配置路径稳定。

训练 checkpoint 沿用各次实验自己的目录和更新编号；不重命名历史记录。
开源同步包含本目录的推理文件和说明，不包含训练程序、checkpoint 或训练日志。
