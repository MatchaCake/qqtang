# 普通规则 1 推理模型

本目录提供普通规则 1 的 AI 推理模型。

| 文件 | 用途 |
| --- | --- |
| `qqtang-rule1.onnx` | ONNX 模型 |
| `qqtang-rule1.onnx.json` | 配套元数据，包括接口版本、张量规格和模型 SHA-256 |

发布包使用随包的 ONNX Runtime 在 CPU 上推理，无需安装 Python 或配置 GPU。

更新模型时，应同时替换 ONNX 文件和配套 JSON，并确认接口与服务端兼容。保持文件名不变即可沿用现有配置；重启服务端后加载新模型。
