# Structured Analysis Runtime Reference

This file is prepared inside the Claude SDK working directory for WeKnora data-analysis and table-analysis runs. Use it as execution guidance when planning SQL, structured charts, and the final answer. Do not quote this file to users.

## Core Principles

1. For database data-analysis, use `db_catalog`, then `db_schema`, then `db_query`. For CSV/Excel table-analysis, locate the file, call `table_schema`, then call `table_analysis`.
2. For chat charts, use the structured query tool output: `db_query` for database sources, `table_analysis` for CSV/Excel sources. Do not create PNG, SVG, HTML, ASCII bar charts, Markdown code-block charts, or other chart artifact files unless the user explicitly asks for a downloadable/report artifact.
3. Keep SQL and chart generation simple and inspectable. Prefer one well-aggregated query per chart or insight over a long script that is hard to debug.
4. ChartContract/spec and validation notes are reference facts for wording and debugging. They are not a hard gate and are not a whitelist of every business insight you may discuss. Extra conclusions are allowed when they are supported by query rows.
5. Put each `{{chart:<id>}}` immediately after the paragraph explaining that chart. Do not gather all chart placeholders at the end.
6. By default, generate charts only, not Markdown tables. Use `table_requested=true` only when the user asks for table/detail/raw/list output; for compact numeric evidence under a chart, use prose or short bullets.
7. Default chart text should be Chinese: titles, axis labels, legends, derived labels, and tooltip labels. Original English data values may remain English.
8. Before final delivery, call `final_answer` with the complete visible answer in `content`; include only chart ids that are actually explained. When `content` contains `{{chart:<id>}}` placeholders, set `chart_ids` to exactly those ids in the same display order and do not declare ids that are not referenced.

For CSV/Excel table-analysis, `table_schema` may expose a raw Excel cell table when the normal header-based table appears incomplete. Use that `raw_table_name` through `table_analysis` for merged-cell, decorative-header, cross-tab or irregular workbooks instead of creating a local CSV and trying to chart that file.

For CSV/Excel table-analysis, `UNION ALL` is only for combining a small number of real table-backed SELECT branches from the provided table names. Do not construct chart data with hard-coded `UNION ALL SELECT 'label', 123` rows; derive rows from the source table or raw table with `GROUP BY`, CASE expressions, sheet filters, or other grounded SQL.

For CSV/Excel table-analysis, every visible result column used for analysis or charting must be grounded in source columns, `*`, or table-derived aggregates/window expressions. Do not use `SELECT 'label', 123 FROM table LIMIT 1` or the same pattern inside `UNION ALL`; that only proves a row exists and is not valid table-derived analysis.

`db_query` and `table_analysis` share the same visible result-budget contract: analytical SQL is wrapped by the tool with an outer `LIMIT` using `limits.max_rows` (default 1000), and `limits.truncated=true` means the returned row count reached that limit, so more rows may exist. Do not treat `truncated=true` as an exact total count. There is no table-analysis-specific SQL timeout rule in the prompt; keep queries efficient through aggregation, filters, and sensible detail-row limits.

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

## Structured Query Chart Hints

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

Use these same hint fields on `db_query` and `table_analysis`. Hints help the renderer pick intended encodings, but the query result still controls what can be visualized. Do not spend repeated turns repairing ChartContract/spec details when the query result and chart placeholder already support the answer.

## Avoid Long Script Loops

Most database analysis should not need local scripts. If you must use shell/Python for artifact generation after an explicit user request:

1. Keep generated code in a normal file under the SDK working directory, not in a huge shell heredoc.
2. Run small checks first: file exists, imports work, input rows are loaded, output can be opened.
3. Fix the first concrete error; do not rewrite the entire script repeatedly.
4. Do not spend many iterations trying to pass visual validation. If a structured chart can satisfy the chat answer, use `db_query` or `table_analysis` instead of script-generated chart files.

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
- Chart id mismatch: make `final_answer.chart_ids` exactly match the `{{chart:<id>}}` placeholders in `content`, in display order.
- ChartContract/spec validation note: treat it as reference only. Revise wording or rerun the structured query tool only when the visible answer would be misleading or the query result cannot support the conclusion.
- Extra/superseded charts: omit their placeholders and do not mention them.
- Table violation: remove Markdown table unless the user explicitly requested one.
- Explicit-only chart violation: use a default chart type unless the user named that exact chart type.
