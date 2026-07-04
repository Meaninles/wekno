# Data Analysis Runtime Reference

This file is prepared inside the Claude SDK working directory for WeKnora data-analysis runs. Use it as execution guidance when planning SQL, structured charts, and the final answer. Do not quote this file to users.

## Core Principles

1. Use `db_catalog`, then `db_schema`, then `db_query`.
2. For chat charts, use `db_query` structured chart output. Do not create PNG, SVG, HTML, or other chart artifact files unless the user explicitly asks for a downloadable/report artifact.
3. Keep SQL and chart generation simple and inspectable. Prefer one well-aggregated query per chart or insight over a long script that is hard to debug.
4. ChartContract/spec and validation notes are reference facts for wording and debugging. They are not a hard gate and are not a whitelist of every business insight you may discuss. Extra conclusions are allowed when they are supported by query rows.
5. Put each `{{chart:<id>}}` immediately after the paragraph explaining that chart. Do not gather all chart placeholders at the end.
6. By default, generate charts only, not tables. Use `table_requested=true` only when the user asks for table/detail/raw/list output.
7. Default chart text should be Chinese: titles, axis labels, legends, derived labels, and tooltip labels. Original English data values may remain English.
8. Before final delivery, call `final_answer` with the complete visible answer in `content`; include only chart ids that are actually explained.

## Chart Planning

When the user asks for charts, choose the chart by analytical intent and available fields, not by one-off dataset names.

- Time trend: `line`; use `dual_axis_combo` when two metrics with different units/scales need to be compared on the same dimension.
- Category comparison/ranking: `bar`.
- Category + subcategory composition comparison: `stacked_bar`.
- Small share/composition: `pie`, usually 2-8 categories and no negative values.
- Two numeric metric relationship: `scatter`.
- One numeric metric distribution: `histogram`.
- Two dimensions plus one intensity metric: `heatmap`.
- Ordered conversion stages: `funnel`.
- Explicit-only types require the user to name the type: `area`, `radar`, `treemap`, `boxplot`.

Prefer fewer, clearer charts. If a broad question needs multiple views, generate only charts that you will explain in the final answer.

## db_query Chart Hints

When chart output is requested, pass optional semantic hints when you have clear intent:

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

Use the real SQL result column names in these hint fields. If the SQL aliases are English for compatibility, make them readable and stable, then explain them in Chinese:

```sql
SELECT
  region AS "区域",
  customer_segment AS "客户类型",
  SUM(pay_amount) AS "销售额"
FROM ...
GROUP BY region, customer_segment
ORDER BY region, "销售额" DESC
```

For a dual-axis chart:

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

Hints help the renderer pick intended encodings, but the query result still controls what can be visualized. Do not spend repeated turns repairing ChartContract/spec details when the query result and chart placeholder already support the answer.

## Avoid Long Script Loops

Most database analysis should not need local scripts. If you must use shell/Python for artifact generation after an explicit user request:

1. Keep generated code in a normal file under the SDK working directory, not in a huge shell heredoc.
2. Run small checks first: file exists, imports work, input rows are loaded, output can be opened.
3. Fix the first concrete error; do not rewrite the entire script repeatedly.
4. Do not spend many iterations trying to pass visual validation. If a structured chart can satisfy the chat answer, use `db_query` instead of script-generated chart files.

## Final Answer Pattern

Good:

```text
各区域销售额差异明显：东区最高，南区次之，西区最低。该图只展示“区域”和“销售额”的汇总对比，适合判断区域贡献排序。

{{chart:chart_region_sales}}

结合查询结果看，VIP 客户主要集中在东区；这是来自同一查询结果的文字洞察。
```

Bad:

```text
下面三张图展示分析结果。

{{chart:chart_a}}
{{chart:chart_b}}
{{chart:chart_c}}
```

Bad because the chart placeholders are not close to their explanations.

## Repair Guidance

If final validation reports an issue:

- Missing or wrong chart placeholder: revise `final_answer.content`; do not rerun SQL unless the chart itself is wrong.
- ChartContract/spec validation note: treat it as reference only. Revise wording or rerun `db_query` only when the visible answer would be misleading or the query result cannot support the conclusion.
- Extra/superseded charts: omit their placeholders and do not mention them.
- Table violation: remove Markdown table unless the user explicitly requested one.
- Explicit-only chart violation: use a default chart type unless the user named that exact chart type.
