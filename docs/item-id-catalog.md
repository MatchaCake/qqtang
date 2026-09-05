# 物品 ID 参考

[item-id-catalog.json](item-id-catalog.json) 和 [item-id-catalog.csv](item-id-catalog.csv) 提供客户端配置中的物品编号、名称和分类，便于查询和编辑玩法配置。

| 字段 | 含义 |
| --- | --- |
| `id`、`name`、`description` | 物品编号、名称和描述 |
| `kind`、`categories` | 规范类别与客户端分类 |
| `source_files` | 信息来源在客户端目录内的相对路径 |
| `confidence` | 来源标记 |
| `client_scene_factory_supported` | 该编号是否存在场景对象工厂构造入口 |

账号物品编号与场景对象编号有不同用途。`client_scene_factory_supported` 仅描述构造入口，不代表该物品可以被拾取、已经实现效果或具有特定掉落概率。

掉落和奖励规则由 [configs](../configs) 中的相应配置控制；物品目录解析代码位于 [internal/game/itemcatalog](../internal/game/itemcatalog)。
