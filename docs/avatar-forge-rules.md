# 锻造概率配置

服务端从 `data/qqt_forge_rules.json` 读取每颗水晶的锻造概率。

```json
{
  "item_id": 20051,
  "success_percent": 50,
  "level_weights": {
    "1": 70,
    "2": 30
  },
  "color_weights": {
    "red": 60,
    "blue": 40
  },
  "black_percent": 5
}
```

- `success_percent` 是 0 到 100 的成功率。
- `level_weights` 是等级相对权重，不要求总和为 100。
- `color_weights` 是普通颜色相对权重，不要求总和为 100。
- 普通颜色支持 `blue`、`red`、`green` 和 `purple`。客户端把 `red` 索引用于部分橙色外观。
- `black_percent` 在普通颜色抽取后独立判定。命中时覆盖为黑色。
- 权重可以为 0，但同一组必须至少有一个正权重。
- 抽到的等级低于装备现有等级时，装备不会降级。

规则文件必须完整覆盖 `avatarforge.ini` 中列出的全部锻造水晶。配置缺失或无效时，服务器拒绝启动。
