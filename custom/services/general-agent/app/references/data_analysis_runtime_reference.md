# 数据分析运行参考

此文件会被放入 WeKnora 数据分析运行的 Claude SDK 工作目录。规划 SQL、结构化图表和最终答案时，将它作为执行指导。不要向用户引用此文件。

## 核心原则

1. 先使用 `db_catalog`，再使用 `db_schema`，最后使用 `db_query`。
2. 对话中的图表请使用 `db_query` 的结构化图表输出。除非用户明确要求可下载/报告类 artifact，否则不要创建 PNG、SVG、HTML 或其他图表 artifact 文件。
3. SQL 和图表生成应保持简单、可检查。与其写一段难以调试的长脚本，不如为每个图表或洞察使用一条聚合清晰的查询。
4. ChartContract/spec 和校验提示是措辞与调试的参考事实。它们不是硬性闸门，也不是允许讨论的全部业务洞察白名单。只要查询结果行支持，允许给出额外结论。
5. 每个 `{{chart:<id>}}` 都应紧跟在解释该图表的段落之后。不要把所有图表占位符集中放在末尾。
6. 默认只生成图表，不生成表格。只有当用户要求表格、明细、原始数据或列表输出时，才使用 `table_requested=true`。
7. 默认图表文本应为中文：标题、轴标签、图例、派生标签和 tooltip 标签都使用中文。原始英文数据值可以保留英文。
8. 最终交付前，调用 `final_answer`，在 `content` 中放入完整可见答案；只包含确实已解释的图表 ID。

## 图表规划

当用户要求图表时，应按分析意图和可用字段选择图表，而不是按某个一次性数据集名称选择。

- 时间趋势：`line`；当两个不同单位/尺度的指标需要在同一维度上比较时，使用 `dual_axis_combo`。
- 类别对比/排名：`bar`。
- 类别 + 子类别构成对比：`stacked_bar`。
- 小规模占比/构成：`pie`，通常为 2-8 个类别且没有负值。
- 两个数值指标关系：`scatter`。
- 单个数值指标分布：`histogram`。
- 两个维度加一个强度指标：`heatmap`。
- 有序转化阶段：`funnel`。
- 仅在用户明确点名类型时才使用：`area`、`radar`、`treemap`、`boxplot`。

优先生成更少但更清晰的图表。如果一个宽泛问题需要多个视角，只生成你会在最终答案中解释的图表。

## db_query 图表提示

请求图表输出时，如果意图清晰，请传入可选语义提示：

```json
{
  "chart_requested": true,
  "preferred_chart": "stacked_bar",
  "chart_intent": "比较各区域中不同客户类型的销售额构成",
  "dimension": "区域",
  "series": "客户类型",
  "primary_metric": "销售额",
  "chart_title": "各区域客户类型销售额构成"
}
```

这些提示字段必须使用真实 SQL 结果列名。如果为了兼容性需要使用英文 SQL 别名，请让别名可读且稳定，然后在中文中解释：

```sql
SELECT
  region AS "区域",
  customer_segment AS "客户类型",
  SUM(pay_amount) AS "销售额"
FROM ...
GROUP BY region, customer_segment
ORDER BY region, "销售额" DESC
```

双轴图表示例：

```json
{
  "chart_requested": true,
  "preferred_chart": "dual_axis_combo",
  "dimension": "月份",
  "primary_metric": "销售额",
  "secondary_metric": "订单数",
  "chart_title": "月度销售额与订单数趋势"
}
```

提示会帮助渲染器选择预期编码方式，但查询结果仍然决定能可视化的内容。当查询结果和图表占位符已经能支撑答案时，不要反复花轮次修复 ChartContract/spec 细节。

## 避免长脚本循环

大多数数据库分析不需要本地脚本。如果用户明确要求后，你必须使用 shell/Python 生成 artifact：

1. 将生成的代码放在 SDK 工作目录下的普通文件中，不要写在巨大的 shell heredoc 里。
2. 先做小检查：文件是否存在、导入是否可用、输入行是否已加载、输出是否可打开。
3. 修复第一个明确错误；不要反复重写整段脚本。
4. 不要为了通过视觉校验消耗很多轮次。如果结构化图表已经能满足对话答案，请使用 `db_query`，而不是脚本生成图表文件。

## 最终答案模式

好：

```text
各区域销售额差异明显：东区最高，南区次之，西区最低。该图只展示“区域”和“销售额”的汇总对比，适合判断区域贡献排序。

{{chart:chart_region_sales}}

结合查询结果看，VIP 客户主要集中在东区；这是来自同一查询结果的文字洞察。
```

不好：

```text
下面三张图展示分析结果。

{{chart:chart_a}}
{{chart:chart_b}}
{{chart:chart_c}}
```

不好之处在于图表占位符没有贴近对应解释。

## 修复指导

如果最终校验报告问题：

- 缺失或错误的图表占位符：修改 `final_answer.content`；除非图表本身错误，否则不要重新运行 SQL。
- ChartContract/spec 校验提示：只把它当作参考。只有当可见答案会误导用户，或查询结果无法支撑结论时，才修改措辞或重新运行 `db_query`。
- 额外/被替代的图表：省略它们的占位符，也不要提及它们。
- 表格违规：移除 Markdown 表格，除非用户明确要求表格。
- 仅显式指定才允许的图表违规：除非用户点名了那个精确图表类型，否则改用默认图表类型。
