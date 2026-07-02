import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { Buffer } from 'node:buffer';
import ts from 'typescript';

const source = await readFile(new URL('./structuredChartMarkdown.ts', import.meta.url), 'utf8');
const transpiled = ts.transpileModule(source, {
  compilerOptions: {
    module: ts.ModuleKind.ESNext,
    target: ts.ScriptTarget.ES2022,
  },
}).outputText;
const moduleUrl = `data:text/javascript;base64,${Buffer.from(transpiled).toString('base64')}`;

const {
  countStructuredChartPlaceholders,
  splitStructuredChartMarkdown,
} = await import(moduleUrl);

test('splitStructuredChartMarkdown places explicit chart id anchors next to matching text', () => {
  const input = [
    '销售额和订单数变化不同步，适合看双轴组合图。',
    '{{chart:chart_dual}}',
    '',
    '区域层级树图如下。',
    '{{chart:chart_tree}}',
  ].join('\n');

  const chartInfos = [
    { id: 'chart_tree', chartType: 'treemap', x: 'region', y: ['value'] },
    { id: 'chart_dual', chartType: 'dual_axis_combo', x: 'day', y: ['revenue'] },
  ];

  const { segments, usedResultCount, usedResultIndexes } = splitStructuredChartMarkdown(input, 2, chartInfos);

  assert.equal(usedResultCount, 2);
  assert.deepEqual(usedResultIndexes, [1, 0]);
  assert.deepEqual(segments.filter(segment => segment.kind === 'chart'), [
    { kind: 'chart', resultIndex: 1 },
    { kind: 'chart', resultIndex: 0 },
  ]);
  assert.doesNotMatch(
    segments
      .filter(segment => segment.kind === 'markdown')
      .map(segment => segment.content)
      .join(''),
    /\{\{\s*chart:/,
  );
});

test('splitStructuredChartMarkdown uses latest result for repeated explicit chart id', () => {
  const input = '最终采用最新散点图。\n{{chart:chart_repeat}}';
  const chartInfos = [
    { id: 'chart_repeat', chartType: 'scatter', x: 'old_x', y: ['old_y'] },
    { id: 'chart_other', chartType: 'pie', x: 'region', y: ['amount'] },
    { id: 'chart_repeat', chartType: 'scatter', x: 'new_x', y: ['new_y'] },
  ];

  const { segments, usedResultIndexes } = splitStructuredChartMarkdown(input, 3, chartInfos);

  assert.deepEqual(usedResultIndexes, [2]);
  assert.deepEqual(segments.filter(segment => segment.kind === 'chart'), [
    { kind: 'chart', resultIndex: 2 },
  ]);
});

test('splitStructuredChartMarkdown replaces invalid chart image links with chart segments', () => {
  const input = [
    '### 饼图① 整体商品品类偏好',
    '',
    '![商品品类占比](各品类的消费占比)',
    '',
    '| 品类 | 消费金额 |',
    '| --- | --- |',
    '| 家电 | 1900 |',
    '',
    '![客户分层占比](各分层客户的消费贡献)',
  ].join('\n');

  const { segments, usedResultCount } = splitStructuredChartMarkdown(input, 2);

  assert.equal(usedResultCount, 2);
  assert.equal(segments.filter(segment => segment.kind === 'chart').length, 2);
  assert.deepEqual(segments[1], { kind: 'chart', resultIndex: 0 });
  assert.deepEqual(segments[3], { kind: 'chart', resultIndex: 1 });
  assert.equal(
    segments
      .filter(segment => segment.kind === 'markdown')
      .map(segment => segment.content)
      .join(''),
    [
      '### 饼图① 整体商品品类偏好',
      '',
      '',
      '',
      '| 品类 | 消费金额 |',
      '| --- | --- |',
      '| 家电 | 1900 |',
      '',
      '',
    ].join('\n'),
  );
});

test('splitStructuredChartMarkdown removes bare chart placeholders when chart data exists', () => {
  const { segments, usedResultCount } = splitStructuredChartMarkdown(
    '### VIP\n\n![VIP客户商品偏好]\n\n| 品类 | 金额 |\n| --- | --- |',
    1,
  );

  assert.equal(usedResultCount, 1);
  assert.deepEqual(segments[1], { kind: 'chart', resultIndex: 0 });
  assert.doesNotMatch(
    segments
      .filter(segment => segment.kind === 'markdown')
      .map(segment => segment.content)
      .join(''),
    /!\[/,
  );
});

test('splitStructuredChartMarkdown preserves real image URLs', () => {
  const input = '![真实图表](https://example.com/chart.png)';
  const { segments, usedResultCount } = splitStructuredChartMarkdown(input, 1);

  assert.equal(usedResultCount, 0);
  assert.deepEqual(segments, [{ kind: 'markdown', content: input }]);
});

test('splitStructuredChartMarkdown removes invalid chart images even without chart data', () => {
  const input = '结论\n\n![饼图占比](各分层客户的消费贡献)\n\n后续说明';
  const { segments, usedResultCount } = splitStructuredChartMarkdown(input, 0);

  assert.equal(usedResultCount, 0);
  assert.equal(segments.length, 2);
  assert.equal(
    segments
      .filter(segment => segment.kind === 'markdown')
      .map(segment => segment.content)
      .join(''),
    '结论\n\n\n\n后续说明',
  );
});

test('countStructuredChartPlaceholders caps by available results', () => {
  const input = '![饼图A](占比A)\n\n![饼图B](占比B)';

  assert.equal(countStructuredChartPlaceholders(input, 1), 1);
  assert.equal(countStructuredChartPlaceholders(input, 4), 2);
});

test('splitStructuredChartMarkdown inserts unreferenced charts before completion summary', () => {
  const input = [
    '## 核心洞察',
    '',
    '1. App 渠道销售额占比最高。',
    '2. Web 渠道客单价偏低。',
    '',
    '以上图表已全部生成，您可以直接查看可视化效果！',
  ].join('\n');

  const { segments, usedResultCount } = splitStructuredChartMarkdown(input, 2);

  assert.equal(usedResultCount, 2);
  assert.deepEqual(segments[1], { kind: 'chart', resultIndex: 0 });
  assert.deepEqual(segments[2], { kind: 'chart', resultIndex: 1 });
  assert.match(segments[0].content, /Web 渠道客单价偏低。/);
  assert.match(segments[3].content, /^以上图表已全部生成/);
});

test('splitStructuredChartMarkdown inserts unreferenced charts after chart section anchors', () => {
  const input = [
    '### 图表 1：渠道销售额对比',
    '',
    'App 明显领先。',
    '',
    '### 图表 2：渠道订单趋势',
    '',
    'Web 起伏较大。',
  ].join('\n');

  const { segments, usedResultCount } = splitStructuredChartMarkdown(input, 2);

  assert.equal(usedResultCount, 2);
  assert.deepEqual(segments[1], { kind: 'chart', resultIndex: 0 });
  assert.deepEqual(segments[3], { kind: 'chart', resultIndex: 1 });
  assert.match(segments[0].content, /^### 图表 1/);
  assert.match(segments[2].content, /App 明显领先。[\s\S]*### 图表 2/);
});

test('splitStructuredChartMarkdown groups heading and generated-line as one chart section', () => {
  const input = [
    '## 各渠道销售情况综合分析',
    '',
    '### 一、各渠道销售金额排名（柱状图）',
    '',
    '已生成 **柱状图**，各渠道销售额（pay_amt）排序如下：',
    '',
    '| 渠道 | 销售额 |',
    '| --- | --- |',
    '| app | 1829 |',
    '',
    '### 二、各渠道订单量占比（饼图）',
    '',
    '已生成 **饼图**，订单量占比：',
    '',
    '- App 3 单',
    '',
    '### 三、各渠道每日销售趋势（折线图）',
    '',
    '已生成 **折线图**，5月1日 ~ 5月8日逐日趋势：',
  ].join('\n');

  const chartInfos = [
    { chartType: 'bar', x: 'channel', y: ['total_pay_amt', 'order_cnt'] },
    { chartType: 'pie', x: 'channel', y: ['order_cnt'] },
    { chartType: 'line', x: 'order_date', y: ['daily_pay_amt', 'daily_orders'] },
  ];

  const { segments, usedResultCount, usedResultIndexes } = splitStructuredChartMarkdown(input, 3, chartInfos);

  assert.equal(usedResultCount, 3);
  assert.deepEqual(usedResultIndexes, [0, 1, 2]);
  assert.equal(segments.filter(segment => segment.kind === 'chart').length, 3);
  assert.deepEqual(segments.filter(segment => segment.kind === 'chart'), [
    { kind: 'chart', resultIndex: 0 },
    { kind: 'chart', resultIndex: 1 },
    { kind: 'chart', resultIndex: 2 },
  ]);
  assert.match(segments[0].content, /已生成 \*\*柱状图\*\*/);
  assert.match(segments[2].content, /已生成 \*\*饼图\*\*/);
  assert.match(segments[4].content, /已生成 \*\*折线图\*\*/);
});

test('splitStructuredChartMarkdown matches chart sections by metadata when result order differs', () => {
  const input = [
    '### Pie chart: order share by channel',
    'Generated pie chart for order count share.',
    '',
    '### Bar chart: paid amount by channel',
    'Generated bar chart for pay_amt ranking.',
  ].join('\n');

  const chartInfos = [
    { chartType: 'bar', x: 'channel', y: ['total_pay_amt'] },
    { chartType: 'pie', x: 'channel', y: ['order_cnt'] },
  ];

  const { segments, usedResultIndexes } = splitStructuredChartMarkdown(input, 2, chartInfos);

  assert.deepEqual(usedResultIndexes, [1, 0]);
  assert.deepEqual(segments.filter(segment => segment.kind === 'chart'), [
    { kind: 'chart', resultIndex: 1 },
    { kind: 'chart', resultIndex: 0 },
  ]);
});

test('splitStructuredChartMarkdown only inlines referenced chart sections and matches business dimensions', () => {
  const input = [
    '## 二、📉 销售趋势分析（折线图）',
    '从 **5月1日~5月8日** 的日销售走势看：',
    '| 日期 | 日收入 | 订单数 |',
    '|:--|--:|--:|',
    '| 5/1 | ¥478.20 | 2 |',
    '',
    '## 三、📊 订单状态分布（饼图）',
    '| 订单状态 | 订单数 | 实付金额 |',
    '| paid | 5 | ¥2,377.90 |',
    '',
    '4.1 按销售渠道（柱状图）',
    '| 渠道 | 订单数 | 实付总额 |',
    '| app | 4 | ¥2,258.70 |',
    '',
    '4.2 渠道 × 来源交叉分析（堆叠柱状图）',
    '| 渠道 | 来源 | 订单数 | 实付 |',
    '| app | push | 2 | ¥1,338.70 |',
    '',
    '4.3 折扣分布（饼图）',
    '| 渠道 | 总让利 | 平均折扣 |',
    '| app | ¥140 | ¥46.67 |',
    '',
    '5.1 客户分层（饼图）',
    '| 客户等级 | 客户数 | 订单数 | 消费总额 |',
    '| VIP | 2 | 4 | ¥1,638.70 |',
    '',
    '5.2 区域分布（柱状图）',
    '| 区域 | 客户数 | 订单数 | 总消费 |',
    '| E | 2 | 4 | ¥1,638.70 |',
    '',
    '5.3 VIP vs 非VIP（饼图）',
    '| 类型 | 客户数 | 订单数 | 总消费 |',
    '| VIP | 2人 | 4单 | ¥1,638.70 |',
    '',
    '5.4 Top客户排名（柱状图）',
    '| 排名 | 客户 | 等级 | 区域 | 订单数 | 总消费 |',
    '| 🥇 | **Mia Zhang** | wholesale | S | 2 | **¥1,202** |',
    '',
    '以上所有分析都已在对应的折线图（销售趋势）、饼图（订单状态/客户分层）、柱状图（渠道/Top客户）中可视化展示。',
  ].join('\n');

  const chartInfos = [
    { chartType: 'pie', x: 'order_status', y: ['order_count', 'total_paid', 'avg_paid'], columns: ['order_status', 'order_count', 'total_paid', 'avg_paid'] },
    { chartType: 'bar', x: 'channel', y: ['order_count', 'total_paid', 'avg_paid', 'customer_count'], columns: ['channel', 'order_count', 'total_paid', 'avg_paid', 'customer_count'] },
    { chartType: 'bar', x: 'source_code', y: ['order_count', 'total_paid', 'avg_paid'], columns: ['source_code', 'order_count', 'total_paid', 'avg_paid'] },
    { chartType: 'line', x: 'month', y: ['order_count', 'gross_amount', 'discount_amount', 'paid_amount'], columns: ['month', 'order_count', 'gross_amount', 'discount_amount', 'paid_amount'] },
    { chartType: 'pie', x: 'customer_segment', y: ['customer_count', 'order_count', 'total_paid'], columns: ['customer_segment', 'customer_count', 'order_count', 'total_paid'] },
    { chartType: 'bar', x: 'region_code', y: ['customer_count', 'order_count', 'total_paid'], columns: ['region_code', 'customer_count', 'order_count', 'total_paid'] },
    { chartType: 'pie', x: 'vip_status', y: ['customer_count', 'order_count', 'total_paid', 'avg_order_value'], columns: ['vip_status', 'customer_count', 'order_count', 'total_paid', 'avg_order_value'] },
    { chartType: 'line', x: 'order_date', y: ['order_count', 'daily_revenue'], columns: ['order_date', 'order_count', 'daily_revenue'] },
    { chartType: 'bar', x: 'channel', y: ['order_count', 'total_paid'], columns: ['channel', 'source_code', 'order_count', 'total_paid'] },
    { chartType: 'bar', x: 'customer_name', y: ['order_count', 'total_spent', 'avg_order_value'], columns: ['customer_name', 'customer_segment', 'region', 'order_count', 'total_spent', 'avg_order_value'] },
    { chartType: 'pie', x: 'channel', y: ['total_discount', 'avg_discount', 'order_count'], columns: ['channel', 'total_discount', 'avg_discount', 'order_count'] },
  ];

  const { segments, usedResultCount, usedResultIndexes } = splitStructuredChartMarkdown(input, 11, chartInfos);

  assert.equal(usedResultCount, 9);
  assert.deepEqual(usedResultIndexes, [7, 0, 1, 8, 10, 4, 5, 6, 9]);
  assert.equal(segments.filter(segment => segment.kind === 'chart').length, 9);
  assert.equal(segments[segments.length - 2].kind, 'chart');
  assert.equal(segments[segments.length - 2].resultIndex, 9);
  assert.equal(segments[segments.length - 1].kind, 'markdown');
  assert.match(segments[segments.length - 1].content, /以上所有分析都已在对应的折线图（销售趋势）、饼图（订单状态\/客户分层）、柱状图（渠道\/Top客户）中可视化展示。$/);
});

test('splitStructuredChartMarkdown leaves ordinary markdown unchanged when no chart results exist', () => {
  const input = '## 核心洞察\n\n普通回答，没有任何结构化图表。';
  const { segments, usedResultCount } = splitStructuredChartMarkdown(input, 0);

  assert.equal(usedResultCount, 0);
  assert.deepEqual(segments, [{ kind: 'markdown', content: input }]);
});
