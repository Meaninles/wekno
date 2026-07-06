<template>
  <div class="structured-analysis" :class="{ 'structured-analysis--mobile': mobileMode }">
    <div v-if="renderChartArea" class="analysis-chart" :class="{ 'analysis-chart--error': chartError }">
      <div ref="chartEl" class="analysis-chart__canvas" />
      <div v-if="chartError" class="analysis-chart__error">{{ chartError }}</div>
    </div>

    <div class="analysis-table-wrap" v-if="showTable && rows.length > 0">
      <table class="analysis-table">
        <thead>
          <tr>
            <th v-for="column in columnNames" :key="column">{{ formatFieldLabel(column) }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, rowIndex) in rows" :key="rowIndex">
            <td v-for="column in columnNames" :key="column">
              {{ formatTableValue(row[column], column) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-else-if="showTable" class="empty-result">暂无结果</div>
    <div v-else-if="!renderChartArea" class="empty-result compact">查询结果已用于回答</div>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import * as echarts from 'echarts/core';
import {
  BarChart,
  BoxplotChart,
  FunnelChart,
  HeatmapChart,
  LineChart,
  PieChart,
  RadarChart,
  ScatterChart,
  TreemapChart,
} from 'echarts/charts';
import {
  DataZoomComponent,
  GridComponent,
  LegendComponent,
  RadarComponent,
  TitleComponent,
  TooltipComponent,
  VisualMapComponent,
} from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';
import type { EChartsType } from 'echarts/core';
import type { StructuredAnalysisData } from '@/types/tool-results';

echarts.use([
  BarChart,
  BoxplotChart,
  FunnelChart,
  HeatmapChart,
  LineChart,
  PieChart,
  RadarChart,
  ScatterChart,
  TreemapChart,
  DataZoomComponent,
  GridComponent,
  LegendComponent,
  RadarComponent,
  TitleComponent,
  TooltipComponent,
  VisualMapComponent,
  CanvasRenderer,
]);

const props = withDefaults(defineProps<{
  data: StructuredAnalysisData;
  mobileMode?: boolean;
}>(), {
  mobileMode: false,
});

const chartEl = ref<HTMLDivElement | null>(null);
const chartError = ref('');
const chartUnavailable = ref(false);
let chart: EChartsType | null = null;
let resizeObserver: ResizeObserver | null = null;
let resizeFrame = 0;
let outsideChartPointerEvent = '';

const rows = computed(() => props.data.rows || []);
const columnNames = computed(() => {
  if (props.data.columns?.length) {
    return props.data.columns.map(column => column.name).filter(Boolean);
  }
  const first = rows.value[0];
  return first ? Object.keys(first) : [];
});

const showChart = computed(() => (
  props.data.chart_requested === true &&
  props.data.chart?.eligible === true
));

const renderChartArea = computed(() => showChart.value && !chartUnavailable.value);

const showTable = computed(() => (
  props.data.table_visible === true ||
  props.data.table_requested === true ||
  props.data.display_mode === 'table' ||
  props.data.display_mode === 'chart_and_table'
));

function numericValue(value: any): number {
  if (typeof value === 'number') return value;
  const parsed = Number(String(value ?? '').replace(/,/g, ''));
  return Number.isFinite(parsed) ? parsed : 0;
}

function formatNumber(value: number): string {
  return new Intl.NumberFormat('zh-CN', {
    maximumFractionDigits: Math.abs(value) >= 100 ? 0 : 2,
  }).format(value);
}

const FIELD_LABELS: Record<string, string> = {
  avg_discount: '平均折扣',
  avg_gross_amt: '平均总金额',
  avg_order_value: '平均客单价',
  avg_order_val: '平均客单价',
  avg_paid: '平均实付金额',
  avg_pay_amt: '平均客单价',
  category: '类别',
  channel: '渠道',
  count: '数量',
  customer_count: '客户数',
  customer_name: '客户',
  customer_segment: '客户分层',
  daily_orders: '日订单数',
  daily_discount: '日折扣',
  daily_pay_amt: '日销售额',
  daily_revenue: '日收入',
  date: '日期',
  day: '日期',
  discount_amount: '折扣金额',
  flow: '流量',
  from_node: '来源',
  gross_amount: '销售总额',
  max: '最大值',
  median: '中位数',
  min: '最小值',
  month: '月份',
  name: '名称',
  order_count: '订单数',
  order_cnt: '订单数',
  order_date: '日期',
  order_status: '订单状态',
  paid_amount: '实付金额',
  pay_amt: '实付金额',
  q1: '第一四分位数',
  q3: '第三四分位数',
  region: '区域',
  region_code: '区域',
  segment: '客户分层',
  source: '来源',
  source_code: '来源',
  src_cd: '来源',
  target: '去向',
  to_node: '去向',
  total_discount: '总折扣',
  total_gross_amt: '总金额',
  total_paid: '实付总额',
  total_pay_amt: '实付总额',
  total_spent: '总消费',
  value: '数值',
  vip_status: 'VIP状态',
};

const FIELD_TOKEN_LABELS: Record<string, string> = {
  amount: '金额',
  avg: '平均',
  average: '平均',
  category: '类别',
  channel: '渠道',
  count: '数量',
  customer: '客户',
  date: '日期',
  day: '日期',
  discount: '折扣',
  flow: '流量',
  gross: '总',
  max: '最大值',
  median: '中位数',
  min: '最小值',
  month: '月份',
  name: '名称',
  order: '订单',
  paid: '实付',
  pay: '支付',
  price: '价格',
  profit: '利润',
  q1: '第一四分位数',
  q3: '第三四分位数',
  quantity: '数量',
  rate: '率',
  ratio: '比例',
  region: '区域',
  revenue: '收入',
  sales: '销售',
  segment: '分层',
  source: '来源',
  status: '状态',
  target: '去向',
  total: '总',
  type: '类型',
  value: '数值',
  week: '周',
  year: '年份',
};

const CATEGORY_LABELS: Record<string, Record<string, string>> = {
  channel: {
    app: 'APP',
    mini: '小程序',
    sales: '线下销售',
    web: 'Web',
    unknown: '未知渠道',
    null: '未知渠道',
  },
  customer_segment: {
    new: '新客',
    normal: '普通客户',
    trial: '试用客户',
    vip: 'VIP',
    wholesale: '批发客户',
  },
  segment: {
    new: '新客',
    normal: '普通客户',
    trial: '试用客户',
    vip: 'VIP',
    wholesale: '批发客户',
  },
  order_status: {
    bad_status: '异常状态',
    cancel: '已取消',
    p: 'P（处理中）',
    paid: '已支付',
    refunding: '退款中',
  },
  region: {
    e: '东部',
    n: '北部',
    s: '南部',
    w: '西部',
  },
  region_code: {
    e: '东部',
    n: '北部',
    s: '南部',
    w: '西部',
  },
  source_code: {
    ad: '广告',
    kol: 'KOL',
    offline: '线下',
    push: '推送',
    seo: '搜索',
    unknown: '未知来源',
  },
  source: {
    ad: '广告',
    kol: 'KOL',
    offline: '线下',
    push: '推送',
    seo: '搜索',
    unknown: '未知来源',
  },
  src_cd: {
    ad: '广告',
    kol: 'KOL',
    offline: '线下',
    push: '推送',
    seo: '搜索',
    unknown: '未知来源',
  },
  vip_status: {
    'non-vip': '非VIP',
    vip: 'VIP',
  },
};

function normalizeFieldName(field: string): string {
  return cleanFieldName(field).toLowerCase();
}

function hasChineseText(value: string): boolean {
  return /[\u4e00-\u9fff]/.test(value);
}

function cleanFieldName(field: string): string {
  let raw = String(field || '').trim();
  if (!raw) return '';
  const nameMatch = raw.match(/(?:^|\s|[{\[,])name[:=]([^\s,\]}]+)/i);
  if (nameMatch?.[1]) raw = nameMatch[1];
  raw = raw
    .replace(/\s+semantic_type[:=].*$/i, '')
    .replace(/\s+type[:=].*$/i, '')
    .replace(/\s+role[:=].*$/i, '')
    .trim();
  return raw;
}

function formatFieldLabel(field: string): string {
  const raw = cleanFieldName(field);
  const normalized = normalizeFieldName(raw);
  if (!raw) return '';
  if (FIELD_LABELS[normalized]) return FIELD_LABELS[normalized];
  if (hasChineseText(raw)) return raw;

  const tokens = normalized.split(/[\s_-]+/).filter(Boolean);
  if (tokens.length > 0 && tokens.every(token => FIELD_TOKEN_LABELS[token])) {
    return tokens.map(token => FIELD_TOKEN_LABELS[token]).join('');
  }
  return raw;
}

function formatCategoryLabel(value: any, field: string): string {
  const raw = String(value ?? '').trim();
  if (!raw) return '未知';
  const labels = CATEGORY_LABELS[normalizeFieldName(field)];
  const translated = labels?.[raw.toLowerCase()];
  if (!translated) return raw;
  if (/^[A-Z]{1,5}$/.test(raw) || raw.toLowerCase() === translated.toLowerCase()) return translated;
  return `${translated}（${raw}）`;
}

function hasColumn(name: string): boolean {
  const normalized = normalizeFieldName(name);
  return Boolean(columnNameFor(normalized));
}

function columnNameFor(field: unknown): string {
  const cleaned = cleanFieldName(String(field || ''));
  if (!cleaned) return '';

  const normalized = normalizeFieldName(cleaned);
  const labelNormalized = normalizeFieldName(formatFieldLabel(cleaned));
  return columnNames.value.find(column => {
    const columnNormalized = normalizeFieldName(column);
    const columnLabelNormalized = normalizeFieldName(formatFieldLabel(column));
    return (
      columnNormalized === normalized ||
      columnLabelNormalized === normalized ||
      columnNormalized === labelNormalized ||
      columnLabelNormalized === labelNormalized
    );
  }) || '';
}

function resolvedColumnFields(fields: unknown[]): string[] {
  const seen = new Set<string>();
  const resolved: string[] = [];
  for (const field of fields) {
    const name = columnNameFor(field);
    const key = normalizeFieldName(name);
    if (!name || seen.has(key)) continue;
    seen.add(key);
    resolved.push(name);
  }
  return resolved;
}

function isNumericLike(value: any): boolean {
  if (typeof value === 'number') return Number.isFinite(value);
  const raw = String(value ?? '').replace(/,/g, '').trim();
  if (!raw) return false;
  return Number.isFinite(Number(raw));
}

function numericColumnFields(): string[] {
  return columnNames.value.filter(field => rows.value.some(row => isNumericLike(row[field])));
}

function fallbackMetricFields(excluded: string[] = []): string[] {
  const excludedSet = new Set(excluded.map(normalizeFieldName).filter(Boolean));
  const semanticMetrics = resolvedColumnFields(semanticFields(['metric']));
  const metrics = semanticMetrics.length ? semanticMetrics : numericColumnFields();
  return metrics.filter(field => !excludedSet.has(normalizeFieldName(field))).slice(0, 4);
}

function resolvedMetricFields(fields: string[], excluded: string[] = []): string[] {
  const excludedSet = new Set(excluded.map(normalizeFieldName).filter(Boolean));
  const resolved = resolvedColumnFields(fields).filter(field => !excludedSet.has(normalizeFieldName(field)));
  return resolved.length ? resolved : fallbackMetricFields(excluded);
}

function hasAnyMetric(fields: string[], names: string[]): boolean {
  const normalizedFields = fields.map(normalizeFieldName);
  return names.some(name => normalizedFields.includes(normalizeFieldName(name)));
}

function semanticFields(types: string[]): string[] {
  const wanted = new Set(types.map(type => normalizeFieldName(type)));
  return (props.data.columns || [])
    .filter(column => wanted.has(normalizeFieldName(column.semantic_type || '')))
    .map(column => column.name)
    .filter(Boolean);
}

function fallbackDimensionField(excluded: string[] = []): string {
  const excludedSet = new Set(excluded.map(normalizeFieldName).filter(Boolean));
  const dimensions = resolvedColumnFields(semanticFields(['dimension', 'time']))
    .filter(field => !excludedSet.has(normalizeFieldName(field)));
  if (dimensions.length > 0) return dimensions[0];

  const metricSet = new Set([...resolvedColumnFields(semanticFields(['metric'])), ...numericColumnFields()].map(normalizeFieldName));
  return columnNames.value.find(field => !excludedSet.has(normalizeFieldName(field)) && !metricSet.has(normalizeFieldName(field)))
    || columnNames.value.find(field => !excludedSet.has(normalizeFieldName(field)))
    || '';
}

function resolvedDimensionField(preferred: string, excluded: string[] = []): string {
  const excludedSet = new Set(excluded.map(normalizeFieldName).filter(Boolean));
  const resolved = columnNameFor(preferred);
  if (resolved && !excludedSet.has(normalizeFieldName(resolved))) return resolved;
  return fallbackDimensionField(excluded);
}

function fallbackSeriesField(xField: string, valueFields: string[]): string {
  return fallbackDimensionField([xField, ...valueFields]);
}

function normalizeChartType(value: unknown): string {
  const chartType = String(value || 'bar').trim().toLowerCase().replace(/[\s-]+/g, '_');
  if (chartType === 'combo' || chartType === 'dual_axis' || chartType === 'bar_line') return 'dual_axis_combo';
  if (chartType === 'stackedbar') return 'stacked_bar';
  if (chartType === 'tree_map') return 'treemap';
  return chartType || 'bar';
}

function chartYFields(): string[] {
  const valueFields = contractFields('values');
  if (valueFields.length > 0) return resolvedMetricFields(valueFields);
  const valueField = contractField('value');
  const secondaryValue = contractField('secondary_value');
  if (valueField) {
    const contractValues = [valueField, secondaryValue].filter(Boolean);
    const legacyY = props.data.chart?.y;
    const legacyFields = Array.isArray(legacyY) ? legacyY.filter(Boolean) : [];
    if (legacyFields.length > contractValues.length && legacyFields[0] === valueField) {
      return resolvedMetricFields(legacyFields);
    }
    return resolvedMetricFields(contractValues);
  }
  const y = props.data.chart?.y;
  return Array.isArray(y) ? resolvedMetricFields(y.filter(Boolean)) : [];
}

function secondaryYFields(): string[] {
  const secondaryValue = contractField('secondary_value');
  if (secondaryValue) return resolvedMetricFields([secondaryValue]);
  const y = props.data.chart?.secondary_y;
  return Array.isArray(y) ? resolvedMetricFields(y.filter(Boolean)) : [];
}

function chartContract() {
  return props.data.chart?.contract || null;
}

function contractField(key: string): string {
  const field = chartContract()?.encoding?.[key];
  if (Array.isArray(field)) return '';
  return typeof field?.field === 'string' ? field.field : '';
}

function contractFields(key: string): string[] {
  const field = chartContract()?.encoding?.[key];
  if (!Array.isArray(field)) return [];
  return field.map(item => item?.field).filter((name): name is string => typeof name === 'string' && Boolean(name));
}

function contractHierarchyFields(): string[] {
  const hierarchy = chartContract()?.encoding?.hierarchy;
  const fields = Array.isArray(hierarchy)
    ? hierarchy.map(item => item?.field).filter((field): field is string => typeof field === 'string' && Boolean(field))
    : [];
  return resolvedColumnFields(fields);
}

function contractChartType(): string {
  return normalizeChartType(chartContract()?.type || props.data.chart?.type || props.data.chart?.default_type);
}

function contractXField(): string {
  return columnNameFor(contractField('x')) || columnNameFor(props.data.chart?.x) || '';
}

function contractYField(): string {
  return columnNameFor(contractField('y')) || '';
}

function contractSeriesField(): string {
  return columnNameFor(contractField('series')) || columnNameFor(contractField('stack')) || columnNameFor(props.data.chart?.group) || '';
}

function contractDisplayLabel(key: 'title' | 'x_label' | 'y_label' | 'value_label' | 'legend_title'): string {
  const value = chartContract()?.display?.[key];
  return typeof value === 'string' ? value : '';
}

function buildChartTitle(chartType: string, xField: string, yFields: string[]): string {
  const x = normalizeFieldName(xField);
  const y = yFields.map(normalizeFieldName);

  if (chartType === 'histogram') return `${formatFieldLabel(xField)}分布`;
  if (chartType === 'heatmap') return '交叉热力分析';
  if (chartType === 'funnel') return `${formatFieldLabel(xField)}漏斗分析`;
  if (chartType === 'dual_axis_combo') return `${formatFieldLabel(xField)}双轴趋势`;
  if (chartType === 'area') return `${formatFieldLabel(xField)}面积趋势`;
  if (chartType === 'radar') return `${xField ? formatFieldLabel(xField) : '指标'}雷达对比`;
  if (chartType === 'treemap') return `${formatFieldLabel(xField)}树图`;
  if (chartType === 'boxplot') return `${formatFieldLabel(yFields[0] || xField)}箱线图`;

  if (x === 'customer_name') return '客户消费排名';
  if (x === 'order_status') return '订单状态分布';
  if (x === 'customer_segment') return '客户分层分布';
  if (x === 'vip_status') return 'VIP与非VIP客户对比';
  if (x === 'region_code' || x === 'region') return '区域销售分布';
  if (x === 'source_code' || x === 'source' || x === 'src_cd') return '来源销售分析';
  if (x === 'order_date') return '每日销售趋势';
  if (x === 'month') return '月度销售趋势';
  if (x === 'channel' && (hasColumn('source_code') || hasColumn('source') || hasColumn('src_cd'))) return '渠道来源交叉分析';
  if (x === 'channel' && hasAnyMetric(y, ['total_discount', 'avg_discount', 'discount_amount'])) return '渠道折扣分布';
  if (x === 'channel') return '渠道销售分析';

  const dimension = formatFieldLabel(xField);
  if (chartType === 'pie') return `${dimension}占比`;
  if (chartType === 'line') return `${dimension}趋势`;
  if (chartType === 'scatter') return `${dimension}散点分析`;
  if (chartType === 'stacked_bar') return `${dimension}分组对比`;
  return `${dimension}对比`;
}

function baseTitle(chartType: string, xField: string, yFields: string[]) {
  const widthPadding = props.mobileMode ? 32 : 64;
  const width = Math.max(props.mobileMode ? 120 : 160, (chartEl.value?.clientWidth || 800) - widthPadding);
  return {
    text: contractDisplayLabel('title') || buildChartTitle(chartType, xField, yFields),
    top: props.mobileMode ? 4 : 6,
    left: 'center',
    textStyle: { fontSize: props.mobileMode ? 13 : 15, fontWeight: 600, width, overflow: 'truncate' },
  };
}

function commonGrid(top = 76, bottom = 56, left = 68, right = 40) {
  if (props.mobileMode) {
    return {
      left: 30,
      right: 30,
      top,
      bottom: Math.max(bottom, 54),
      containLabel: false,
    };
  }
  return { left, right, top, bottom, containLabel: true };
}

function categoryAxisName(field: string, fallback = '') {
  return contractDisplayLabel('x_label') || displayAxisLabel(field, fallback);
}

function valueAxisName(field: string, fallback = '数值') {
  return contractDisplayLabel('y_label') || displayAxisLabel(field, fallback);
}

function categoryAxisLabel(maxWidth = 88) {
  return { hideOverlap: true, overflow: 'truncate', width: props.mobileMode ? Math.min(maxWidth, 64) : maxWidth };
}

function axisNameOption(name: string, rotate = 0, gap = 32) {
  return {
    name,
    nameLocation: 'middle',
    nameGap: props.mobileMode ? Math.min(gap, 26) : gap,
    nameRotate: rotate,
    nameTextStyle: {
      align: 'center',
      verticalAlign: 'middle',
      overflow: 'truncate',
      width: props.mobileMode ? 84 : 120,
    },
  };
}

function uniqueLabels(field: string): string[] {
  const seen = new Set<string>();
  const labels: string[] = [];
  for (const row of rows.value) {
    const label = formatCategoryLabel(row[field], field);
    if (seen.has(label)) continue;
    seen.add(label);
    labels.push(label);
  }
  return labels;
}

function sumByCategory(categoryField: string, valueField: string) {
  const values = new Map<string, number>();
  for (const row of rows.value) {
    const label = formatCategoryLabel(row[categoryField], categoryField);
    values.set(label, (values.get(label) || 0) + numericValue(row[valueField]));
  }
  return Array.from(values.entries()).map(([name, value]) => ({ name, value }));
}

function sumByCategoryFields(categoryField: string, valueFields: string[]) {
  const labels: string[] = [];
  const seen = new Set<string>();
  const values = new Map<string, Map<string, number>>();
  for (const row of rows.value) {
    const label = formatCategoryLabel(row[categoryField], categoryField);
    if (!seen.has(label)) {
      seen.add(label);
      labels.push(label);
    }
    if (!values.has(label)) values.set(label, new Map<string, number>());
    const item = values.get(label)!;
    for (const field of valueFields) {
      item.set(field, (item.get(field) || 0) + numericValue(row[field]));
    }
  }
  return { labels, values };
}

function displayAxisLabel(field: string, fallback = ''): string {
  if (!field) return fallback;
  return formatFieldLabel(field);
}

function buildPieOption(chartType: string, xField: string, yFields: string[]) {
  const y = yFields[0];
  if (!xField || !y) return null;
  const data = sumByCategory(xField, y);
  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { trigger: 'item' },
    legend: { top: 34, type: 'scroll' },
    series: [{
      name: contractDisplayLabel('value_label') || formatFieldLabel(y),
      type: 'pie',
      radius: ['35%', '68%'],
      top: 48,
      data,
    }],
  };
}

function buildScatterOption(chartType: string, xField: string, yFields: string[]) {
  const y = yFields[0];
  if (!xField || !y) return null;
  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { trigger: 'item' },
    grid: commonGrid(64, 60, 72, 48),
    xAxis: { type: 'value', ...axisNameOption(formatFieldLabel(xField), 0, 34) },
    yAxis: { type: 'value', ...axisNameOption(formatFieldLabel(y), 90, 52) },
    series: [{
      name: formatFieldLabel(y),
      type: 'scatter',
      data: rows.value.map(row => [numericValue(row[xField]), numericValue(row[y])]),
    }],
  };
}

function buildCategoryMetricOption(chartType: string, xField: string, yFields: string[]) {
  if (!xField || yFields.length === 0) return null;
  const isLine = chartType === 'line' || chartType === 'area';
  const aggregated = sumByCategoryFields(xField, yFields);
  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { trigger: 'axis' },
    legend: { top: 34, type: 'scroll' },
    grid: commonGrid(),
    xAxis: {
      type: 'category',
      ...axisNameOption(categoryAxisName(xField), 0, 34),
      data: aggregated.labels,
      axisLabel: categoryAxisLabel(),
    },
    yAxis: { type: 'value', ...axisNameOption(contractDisplayLabel('y_label') || '数值', 90, 54) },
    series: yFields.map(field => ({
      name: yFields.length === 1 ? (contractDisplayLabel('value_label') || formatFieldLabel(field)) : formatFieldLabel(field),
      type: isLine ? 'line' : 'bar',
      smooth: isLine,
      areaStyle: chartType === 'area' ? {} : undefined,
      data: aggregated.labels.map(label => aggregated.values.get(label)?.get(field) || 0),
    })),
  };
}

function buildStackedBarOption(chartType: string, xField: string, yFields: string[]) {
  const groupField = contractSeriesField() || fallbackSeriesField(xField, yFields);
  const valueField = yFields[0];
  if (!xField || !groupField || !valueField) return buildCategoryMetricOption('bar', xField, yFields);

  const xLabels = uniqueLabels(xField);
  const groupLabels = uniqueLabels(groupField);
  const values = new Map<string, Map<string, number>>();

  for (const row of rows.value) {
    const xLabel = formatCategoryLabel(row[xField], xField);
    const groupLabel = formatCategoryLabel(row[groupField], groupField);
    if (!values.has(groupLabel)) values.set(groupLabel, new Map<string, number>());
    const groupMap = values.get(groupLabel)!;
    groupMap.set(xLabel, (groupMap.get(xLabel) || 0) + numericValue(row[valueField]));
  }

  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    legend: { top: 34, type: 'scroll' },
    grid: commonGrid(),
    xAxis: { type: 'category', ...axisNameOption(categoryAxisName(xField), 0, 34), data: xLabels, axisLabel: categoryAxisLabel() },
    yAxis: { type: 'value', ...axisNameOption(valueAxisName(valueField), 90, 54) },
    series: groupLabels.map(groupLabel => ({
      name: groupLabel,
      type: 'bar',
      stack: '总量',
      emphasis: { focus: 'series' },
      data: xLabels.map(xLabel => values.get(groupLabel)?.get(xLabel) || 0),
    })),
  };
}

function buildHistogramOption(chartType: string, xField: string, yFields: string[]) {
  if (!xField) return null;
  const values = rows.value.map(row => numericValue(row[xField])).filter(Number.isFinite);
  if (values.length === 0) return null;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const binCount = min === max ? 1 : Math.min(10, Math.max(4, Math.ceil(Math.sqrt(values.length))));
  const width = binCount === 1 ? 1 : (max - min) / binCount;
  const counts = Array.from({ length: binCount }, () => 0);
  for (const value of values) {
    const index = width === 0 ? 0 : Math.min(binCount - 1, Math.floor((value - min) / width));
    counts[index] += 1;
  }
  const labels = counts.map((_, index) => {
    if (binCount === 1) return formatNumber(min);
    const start = min + index * width;
    const end = index === binCount - 1 ? max : start + width;
    return `${formatNumber(start)}-${formatNumber(end)}`;
  });

  return {
    title: baseTitle(chartType, xField, yFields.length ? yFields : [xField]),
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    grid: commonGrid(64, 70, 72, 42),
    xAxis: { type: 'category', ...axisNameOption(formatFieldLabel(xField), 0, 36), data: labels, axisLabel: { ...categoryAxisLabel(96), rotate: labels.length > 6 ? 24 : 0 } },
    yAxis: { type: 'value', ...axisNameOption('频数', 90, 52) },
    series: [{ name: '频数', type: 'bar', data: counts }],
  };
}

function buildHeatmapOption(chartType: string, xField: string, yFields: string[]) {
  const valueField = yFields[0];
  const groupField = contractYField() || contractSeriesField() || fallbackSeriesField(xField, valueField ? [valueField] : yFields);
  if (!xField || !groupField || !valueField) return null;
  const compact = (chartEl.value?.clientWidth || 800) < 640;

  const xLabels = uniqueLabels(xField);
  const yLabels = uniqueLabels(groupField);
  const valueMap = new Map<string, number>();
  for (const row of rows.value) {
    const x = xLabels.indexOf(formatCategoryLabel(row[xField], xField));
    const y = yLabels.indexOf(formatCategoryLabel(row[groupField], groupField));
    if (x < 0 || y < 0) continue;
    const key = `${x}\u0000${y}`;
    valueMap.set(key, (valueMap.get(key) || 0) + numericValue(row[valueField]));
  }
  const data = Array.from(valueMap.entries()).map(([key, value]) => {
    const [x, y] = key.split('\u0000').map(Number);
    return [x, y, value];
  });
  const values = data.map(item => item[2]);
  const min = Math.min(0, ...values);
  const max = Math.max(1, ...values);

  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { position: 'top' },
    grid: compact ? commonGrid(74, 74, 82, 42) : commonGrid(74, 78, 92, 96),
    xAxis: {
      type: 'category',
      ...axisNameOption(categoryAxisName(xField), 0, 34),
      data: xLabels,
      splitArea: { show: true },
      axisLabel: categoryAxisLabel(92),
    },
    yAxis: {
      type: 'category',
      ...axisNameOption(contractDisplayLabel('y_label') || formatFieldLabel(groupField), 90, 68),
      data: yLabels,
      splitArea: { show: true },
      axisLabel: categoryAxisLabel(96),
    },
    visualMap: {
      show: !compact,
      min,
      max,
      calculable: false,
      orient: 'vertical',
      right: 16,
      top: 'middle',
      itemWidth: 10,
      itemHeight: 150,
      textGap: 8,
      text: ['高', '低'],
    },
    series: [{
      name: formatFieldLabel(valueField),
      type: 'heatmap',
      data,
      emphasis: { itemStyle: { shadowBlur: 10, shadowColor: 'rgba(0, 0, 0, 0.25)' } },
    }],
  };
}

function buildFunnelOption(chartType: string, xField: string, yFields: string[]) {
  const valueField = yFields[0];
  if (!xField || !valueField) return null;
  const data = sumByCategory(xField, valueField);
  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { trigger: 'item' },
    legend: { top: 34, type: 'scroll' },
    series: [{
      name: formatFieldLabel(valueField),
      type: 'funnel',
      top: 72,
      bottom: 18,
      left: '10%',
      width: '80%',
      sort: 'descending',
      label: { show: true, formatter: '{b}' },
      data,
    }],
  };
}

function buildDualAxisOption(chartType: string, xField: string, yFields: string[]) {
  const primaryField = yFields[0];
  const secondaryFields = secondaryYFields().length ? secondaryYFields() : yFields.slice(1, 2);
  const secondaryField = secondaryFields[0];
  if (!xField || !primaryField || !secondaryField) return null;
  const aggregated = sumByCategoryFields(xField, [primaryField, secondaryField]);

  return {
    title: baseTitle(chartType, xField, [primaryField, secondaryField]),
    tooltip: { trigger: 'axis' },
    legend: { top: 34, type: 'scroll' },
    grid: commonGrid(76, 62, 72, 54),
    xAxis: {
      type: 'category',
      ...axisNameOption(categoryAxisName(xField), 0, 34),
      data: aggregated.labels,
      axisLabel: categoryAxisLabel(),
    },
    yAxis: [
      { type: 'value', ...axisNameOption(formatFieldLabel(primaryField), 90, 52) },
      { type: 'value', ...axisNameOption(formatFieldLabel(secondaryField), 90, 52) },
    ],
    series: [
      {
        name: formatFieldLabel(primaryField),
        type: 'bar',
        yAxisIndex: 0,
        data: aggregated.labels.map(label => aggregated.values.get(label)?.get(primaryField) || 0),
      },
      {
        name: formatFieldLabel(secondaryField),
        type: 'line',
        smooth: true,
        yAxisIndex: 1,
        data: aggregated.labels.map(label => aggregated.values.get(label)?.get(secondaryField) || 0),
      },
    ],
  };
}

function buildRadarOption(chartType: string, xField: string, yFields: string[]) {
  if (yFields.length < 3) return null;
  const indicators = yFields.map(field => {
    const maxValue = Math.max(1, ...rows.value.map(row => numericValue(row[field])));
    return { name: formatFieldLabel(field), max: maxValue * 1.2 };
  });
  const data = rows.value.slice(0, 8).map((row, index) => ({
    name: xField ? formatCategoryLabel(row[xField], xField) : `样本${index + 1}`,
    value: yFields.map(field => numericValue(row[field])),
  }));

  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { trigger: 'item' },
    legend: { top: 34, type: 'scroll' },
    radar: {
      top: 72,
      bottom: 24,
      radius: '62%',
      indicator: indicators,
    },
    series: [{
      name: '指标对比',
      type: 'radar',
      data,
    }],
  };
}

function buildTreemapOption(chartType: string, xField: string, yFields: string[]) {
  const hierarchy = contractHierarchyFields();
  const valueField = yFields[0];
  const groupField = hierarchy.find(field => field !== xField) || contractSeriesField() || fallbackSeriesField(xField, valueField ? [valueField] : yFields);
  if (!xField || !valueField) return null;

  let data: any[];
  if (groupField) {
    const groups = new Map<string, any[]>();
    for (const row of rows.value) {
      const groupName = formatCategoryLabel(row[groupField], groupField);
      if (!groups.has(groupName)) groups.set(groupName, []);
      const children = groups.get(groupName)!;
      const name = formatCategoryLabel(row[xField], xField);
      const existing = children.find(item => item.name === name);
      if (existing) {
        existing.value += numericValue(row[valueField]);
      } else {
        children.push({ name, value: numericValue(row[valueField]) });
      }
    }
    data = Array.from(groups.entries()).map(([name, children]) => ({
      name,
      value: children.reduce((sum, item) => sum + numericValue(item.value), 0),
      children,
    }));
  } else {
    data = sumByCategory(xField, valueField);
  }

  return {
    title: baseTitle(chartType, xField, yFields),
    tooltip: { trigger: 'item' },
    series: [{
      name: formatFieldLabel(valueField),
      type: 'treemap',
      top: 64,
      bottom: 16,
      left: 12,
      right: 12,
      roam: false,
      breadcrumb: { show: false },
      label: { show: true, formatter: '{b}' },
      data,
    }],
  };
}

function quantile(sorted: number[], q: number): number {
  if (sorted.length === 0) return 0;
  const position = (sorted.length - 1) * q;
  const base = Math.floor(position);
  const rest = position - base;
  const next = sorted[base + 1];
  return next === undefined ? sorted[base] : sorted[base] + rest * (next - sorted[base]);
}

function boxStats(values: number[]): number[] {
  const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
  if (sorted.length === 0) return [0, 0, 0, 0, 0];
  return [
    sorted[0],
    quantile(sorted, 0.25),
    quantile(sorted, 0.5),
    quantile(sorted, 0.75),
    sorted[sorted.length - 1],
  ];
}

function buildBoxplotOption(chartType: string, xField: string, yFields: string[]) {
  if (yFields.length >= 5) {
    const labels = rows.value.map((row, index) => (xField ? formatCategoryLabel(row[xField], xField) : `样本${index + 1}`));
    return {
      title: baseTitle(chartType, xField, yFields),
      tooltip: { trigger: 'item' },
      grid: commonGrid(64, 54),
      xAxis: { type: 'category', data: labels, ...axisNameOption(xField ? formatFieldLabel(xField) : '样本', 0, 34), axisLabel: categoryAxisLabel() },
      yAxis: { type: 'value', ...axisNameOption('数值', 90, 52) },
      series: [{
        name: '箱线图',
        type: 'boxplot',
        data: rows.value.map(row => yFields.slice(0, 5).map(field => numericValue(row[field]))),
      }],
    };
  }

  const valueField = yFields[0];
  if (!valueField) return null;
  const groups = new Map<string, number[]>();
  for (const row of rows.value) {
    const label = xField ? formatCategoryLabel(row[xField], xField) : '总体';
    if (!groups.has(label)) groups.set(label, []);
    groups.get(label)!.push(numericValue(row[valueField]));
  }
  const labels = Array.from(groups.keys());
  return {
    title: baseTitle(chartType, xField, [valueField]),
    tooltip: { trigger: 'item' },
    grid: commonGrid(64, 54),
    xAxis: { type: 'category', data: labels, ...axisNameOption(xField ? formatFieldLabel(xField) : '分组', 0, 34), axisLabel: categoryAxisLabel() },
    yAxis: { type: 'value', ...axisNameOption(formatFieldLabel(valueField), 90, 52) },
    series: [{
      name: formatFieldLabel(valueField),
      type: 'boxplot',
      data: labels.map(label => boxStats(groups.get(label) || [])),
    }],
  };
}

function asOptionArray(value: any): any[] {
  if (!value) return [];
  return Array.isArray(value) ? value : [value];
}

function mobileLegendOption(legend: any = {}) {
  return {
    ...legend,
    type: 'scroll',
    left: 8,
    right: 8,
    top: legend.top ?? 30,
    itemWidth: 10,
    itemHeight: 8,
    pageIconSize: 10,
    pageButtonItemGap: 4,
    textStyle: {
      ...(legend.textStyle || {}),
      fontSize: 11,
    },
  };
}

function tuneMobileAxis(axis: any, isCategory = false, orientation: 'x' | 'y' = 'x') {
  if (!axis || typeof axis !== 'object') return axis;
  const isValueYAxis = orientation === 'y' && !isCategory;
  return {
    ...axis,
    name: '',
    nameGap: 0,
    nameTextStyle: {
      ...(axis.nameTextStyle || {}),
      fontSize: 10,
      width: 76,
      overflow: 'truncate',
    },
    axisLabel: {
      ...(axis.axisLabel || {}),
      hideOverlap: true,
      inside: isValueYAxis ? false : axis.axisLabel?.inside,
      width: isCategory ? 58 : 32,
      overflow: 'truncate',
      fontSize: 10,
      margin: isValueYAxis ? 3 : 4,
      rotate: isCategory && Array.isArray(axis.data) && axis.data.length > 6
        ? Math.max(Number(axis.axisLabel?.rotate || 0), 28)
        : axis.axisLabel?.rotate,
    },
  };
}

function categoryAxisIndexes(option: any): number[] {
  return asOptionArray(option.xAxis)
    .map((axis, index) => ({ axis, index }))
    .filter(({ axis }) => axis?.type === 'category')
    .map(({ index }) => index);
}

function maxCategoryCount(option: any): number {
  return Math.max(
    0,
    ...asOptionArray(option.xAxis)
      .filter(axis => axis?.type === 'category')
      .map(axis => Array.isArray(axis.data) ? axis.data.length : 0),
  );
}

function ensureGridBottom(option: any, minBottom: number) {
  const grids = asOptionArray(option.grid);
  if (!grids.length) return;
  const nextGrids = grids.map((grid) => ({
    ...grid,
    bottom: Math.max(Number(grid?.bottom || 0), minBottom),
  }));
  option.grid = Array.isArray(option.grid) ? nextGrids : nextGrids[0];
}

function appendMobileDataZoom(option: any, chartType: string) {
  const zoomableTypes = new Set(['bar', 'line', 'area', 'stacked_bar', 'histogram', 'dual_axis_combo', 'boxplot']);
  const xAxisIndexes = categoryAxisIndexes(option);
  const pointCount = maxCategoryCount(option);
  if (!zoomableTypes.has(chartType) || xAxisIndexes.length === 0 || pointCount <= 6) return;

  const existing = asOptionArray(option.dataZoom);
  option.dataZoom = [
    ...existing,
    {
      type: 'inside',
      xAxisIndex: xAxisIndexes,
      zoomOnMouseWheel: false,
      moveOnMouseMove: true,
      moveOnMouseWheel: true,
      preventDefaultMouseMove: true,
    },
    {
      type: 'slider',
      xAxisIndex: xAxisIndexes,
      height: 16,
      bottom: 8,
      showDetail: false,
      brushSelect: false,
      borderColor: 'transparent',
      fillerColor: 'rgba(7, 193, 96, 0.16)',
      handleSize: 12,
      textStyle: { fontSize: 10 },
    },
  ];
  ensureGridBottom(option, 74);
}

function tuneMobileSeries(option: any, chartType: string) {
  const series = asOptionArray(option.series);
  for (const item of series) {
    if (!item || typeof item !== 'object') continue;
    item.label = {
      ...(item.label || {}),
      fontSize: 10,
      overflow: 'truncate',
      width: chartType === 'treemap' ? 72 : item.label?.width,
    };

    if (chartType === 'pie') {
      item.radius = ['32%', '60%'];
      item.center = ['50%', '54%'];
      item.top = 38;
      item.bottom = 28;
    } else if (chartType === 'funnel') {
      item.top = 68;
      item.bottom = 32;
      item.left = '7%';
      item.width = '86%';
    } else if (chartType === 'treemap') {
      item.top = 52;
      item.bottom = 8;
      item.left = 4;
      item.right = 4;
    }
  }
  option.series = Array.isArray(option.series) ? series : series[0];
}

function tuneMobileSpecialLayouts(option: any, chartType: string) {
  if (chartType === 'pie' || chartType === 'funnel') {
    option.legend = mobileLegendOption({
      ...(option.legend || {}),
      top: 'auto',
      bottom: 0,
    });
  }

  if (chartType === 'radar') {
    option.legend = mobileLegendOption({
      ...(option.legend || {}),
      top: 'auto',
      bottom: 0,
    });
    if (option.radar) {
      option.radar = {
        ...option.radar,
        top: 64,
        bottom: 42,
        radius: '54%',
        axisName: {
          ...(option.radar.axisName || {}),
          fontSize: 10,
          overflow: 'truncate',
          width: 64,
        },
      };
    }
  }

  if (chartType === 'heatmap') {
    option.visualMap = {
      ...(option.visualMap || {}),
      show: false,
    };
    ensureGridBottom(option, 64);
  }
}

function applyMobileChartOption(option: any, chartType: string) {
  if (!props.mobileMode || !option) return option;

  option.tooltip = {
    ...(option.tooltip || {}),
    alwaysShowContent: false,
    appendToBody: false,
    confine: true,
    enterable: false,
    hideDelay: 60,
    triggerOn: 'click',
    textStyle: {
      ...(option.tooltip?.textStyle || {}),
      fontSize: 12,
    },
  };

  if (option.legend) {
    option.legend = mobileLegendOption(option.legend);
  }

  const xAxes = asOptionArray(option.xAxis).map(axis => tuneMobileAxis(axis, axis?.type === 'category', 'x'));
  const yAxes = asOptionArray(option.yAxis).map(axis => tuneMobileAxis(axis, axis?.type === 'category', 'y'));
  if (xAxes.length) option.xAxis = Array.isArray(option.xAxis) ? xAxes : xAxes[0];
  if (yAxes.length) option.yAxis = Array.isArray(option.yAxis) ? yAxes : yAxes[0];

  tuneMobileSeries(option, chartType);
  tuneMobileSpecialLayouts(option, chartType);
  appendMobileDataZoom(option, chartType);

  return option;
}

function buildChartOption() {
  const spec = props.data.chart;
  if (!spec || rows.value.length === 0) return null;
  const chartType = contractChartType();
  const yFields = chartYFields();
  const xField = resolvedDimensionField(contractXField(), yFields);
  const resolvedYFields = resolvedMetricFields(yFields, [xField]);
  let option: any = null;

  switch (chartType) {
    case 'pie':
      option = buildPieOption(chartType, xField, resolvedYFields);
      break;
    case 'scatter':
      option = buildScatterOption(chartType, xField, resolvedYFields);
      break;
    case 'stacked_bar':
      option = buildStackedBarOption(chartType, xField, resolvedYFields);
      break;
    case 'histogram':
      option = buildHistogramOption(chartType, xField, resolvedYFields);
      break;
    case 'heatmap':
      option = buildHeatmapOption(chartType, xField, resolvedYFields);
      break;
    case 'funnel':
      option = buildFunnelOption(chartType, xField, resolvedYFields);
      break;
    case 'dual_axis_combo':
      option = buildDualAxisOption(chartType, xField, resolvedYFields);
      break;
    case 'radar':
      option = buildRadarOption(chartType, xField, resolvedYFields);
      break;
    case 'treemap':
      option = buildTreemapOption(chartType, xField, resolvedYFields);
      break;
    case 'boxplot':
      option = buildBoxplotOption(chartType, xField, resolvedYFields);
      break;
    case 'line':
    case 'area':
    case 'bar':
    default:
      option = buildCategoryMetricOption(chartType, xField, resolvedYFields);
      break;
  }
  const fallback = option || (xField && resolvedYFields.length ? buildCategoryMetricOption('bar', xField, resolvedYFields) : null);
  return applyMobileChartOption(fallback, fallback === option ? chartType : 'bar');
}

async function renderChart() {
  await nextTick();
  if (props.mobileMode && typeof window !== 'undefined') {
    await new Promise<void>((resolve) => {
      window.requestAnimationFrame(() => window.requestAnimationFrame(() => resolve()));
    });
  }
  if (!showChart.value || !chartEl.value) {
    chart?.dispose();
    chart = null;
    chartError.value = '';
    return;
  }
  try {
    const option = buildChartOption();
    if (!option) {
      chart?.dispose();
      chart = null;
      chartError.value = '';
      chartUnavailable.value = true;
      return;
    }
    chartError.value = '';
    chartUnavailable.value = false;
    if (!chart) {
      chart = props.mobileMode
        ? echarts.init(chartEl.value, undefined, { renderer: 'canvas', useCoarsePointer: true })
        : echarts.init(chartEl.value);
    }
    chart.setOption(option, true);
    chart.resize();
  } catch (error) {
    chart?.dispose();
    chart = null;
    chartError.value = error instanceof Error ? error.message : '图表渲染失败';
    console.warn('[StructuredAnalysisResult] chart render failed', {
      error,
      chart: props.data.chart,
      columns: props.data.columns,
      rows: rows.value.slice(0, 5),
    });
  }
}

function formatValue(value: any): string {
  if (value === null || value === undefined) return '-';
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

function formatTableValue(value: any, field: string): string {
  if (CATEGORY_LABELS[normalizeFieldName(field)]) {
    return formatCategoryLabel(value, field);
  }
  return formatValue(value);
}

function resizeChart() {
  chart?.resize();
}

function scheduleResizeChart() {
  if (typeof window === 'undefined') {
    resizeChart();
    return;
  }
  window.cancelAnimationFrame(resizeFrame);
  resizeFrame = window.requestAnimationFrame(() => resizeChart());
}

function hideChartInteractionState() {
  if (!chart) return;
  chart.dispatchAction({ type: 'hideTip' });
  chart.dispatchAction({ type: 'downplay' });
}

function handleOutsideChartPointer(event: Event) {
  if (!props.mobileMode || !chartEl.value) return;
  const target = event.target;
  if (target instanceof Node && chartEl.value.contains(target)) return;
  hideChartInteractionState();
}

function addOutsideChartPointerListener() {
  if (!props.mobileMode || typeof document === 'undefined') return;
  outsideChartPointerEvent = typeof window !== 'undefined' && 'PointerEvent' in window ? 'pointerdown' : 'touchstart';
  document.addEventListener(outsideChartPointerEvent, handleOutsideChartPointer, true);
}

function removeOutsideChartPointerListener() {
  if (!outsideChartPointerEvent || typeof document === 'undefined') return;
  document.removeEventListener(outsideChartPointerEvent, handleOutsideChartPointer, true);
  outsideChartPointerEvent = '';
}

onMounted(() => {
  renderChart();
  window.addEventListener('resize', resizeChart);
  addOutsideChartPointerListener();
  if (props.mobileMode && chartEl.value && typeof ResizeObserver !== 'undefined') {
    resizeObserver = new ResizeObserver(scheduleResizeChart);
    resizeObserver.observe(chartEl.value);
  }
});
watch(() => props.data, renderChart, { deep: true });
watch(() => props.data, () => {
  chartUnavailable.value = false;
}, { deep: true, flush: 'sync' });

onBeforeUnmount(() => {
  window.removeEventListener('resize', resizeChart);
  removeOutsideChartPointerListener();
  resizeObserver?.disconnect();
  resizeObserver = null;
  if (typeof window !== 'undefined') window.cancelAnimationFrame(resizeFrame);
  chart?.dispose();
  chart = null;
});
</script>

<style scoped lang="less">
.structured-analysis {
  font-size: 13px;
  color: var(--td-text-color-primary);
}

.analysis-chart {
  width: 100%;
  height: 380px;
  margin: 6px 0 12px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  position: relative;

  &__canvas {
    width: 100%;
    height: 100%;
  }

  &__error {
    position: absolute;
    inset: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    height: 100%;
    padding: 24px;
    color: var(--td-error-color);
    text-align: center;
    background: var(--td-bg-color-secondarycontainer);
  }
}

.analysis-table-wrap {
  overflow-x: auto;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
}

.analysis-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;

  th,
  td {
    padding: 10px 12px;
    border-bottom: 1px solid var(--td-component-stroke);
    text-align: left;
    white-space: nowrap;
  }

  th {
    font-weight: 600;
    background: var(--td-bg-color-secondarycontainer);
  }

  tr:last-child td {
    border-bottom: 0;
  }
}

.empty-result {
  padding: 28px;
  text-align: center;
  color: var(--td-text-color-placeholder);
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);

  &.compact {
    padding: 12px 16px;
  }
}

.structured-analysis--mobile {
  width: 100%;
  min-width: 0;
  color: #1e3027;
  font-size: 13px;

  .analysis-chart {
    height: clamp(260px, 74vw, 340px);
    margin: 4px 0 10px;
    border: 0;
    border-radius: 12px;
    background: #fff;
  }

  .analysis-table-wrap {
    max-width: 100%;
    overflow-x: auto;
    border: 0;
    border-radius: 12px;
    background: #fff;
    -webkit-overflow-scrolling: touch;
  }

  .analysis-table {
    width: max-content;
    min-width: 100%;
    font-size: 12px;
    line-height: 1.45;

    th,
    td {
      padding: 8px 10px;
    }
  }

  .empty-result.compact {
    display: none;
  }
}
</style>
