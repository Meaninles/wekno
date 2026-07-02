export type StructuredChartMarkdownSegment =
  | { kind: 'markdown'; content: string }
  | { kind: 'chart'; resultIndex: number };

export type StructuredChartInfo = {
  id?: string;
  chartType?: string;
  x?: string;
  y?: string[];
  columns?: string[];
  query?: string;
};

type SplitStructuredChartMarkdownResult = {
  segments: StructuredChartMarkdownSegment[];
  usedResultCount: number;
  usedResultIndexes: number[];
};

type PlaceholderMatch = {
  start: number;
  end: number;
  chartRef?: string;
};

type ChartAnchor = {
  insertAt: number;
  insertBefore: boolean;
  text: string;
  kind: 'section' | 'completion';
};

type PlannedChartAnchor = ChartAnchor & {
  resultIndexes: number[];
};

type LineMatch = {
  text: string;
  start: number;
  end: number;
};

const CHART_PLACEHOLDER_HINT_RE =
  /(图表|图|饼图|柱状|条形|折线|散点|面积图|雷达图|树图|箱线图|热力图|漏斗图|双轴|组合图|占比|分布|趋势|偏好|贡献|chart|graph|plot|pie|bar|line|scatter|area|radar|treemap|boxplot|heatmap|funnel|combo)/i;
const CHART_SECTION_RE =
  /(图表|图\s*\d+|饼图|柱状图|条形图|折线图|散点图|面积图|雷达图|树图|箱线图|热力图|漏斗图|双轴图|组合图|可视化|chart|graph|plot|pie chart|bar chart|line chart|scatter plot|area chart|radar chart|treemap|boxplot|heatmap|funnel|combo chart)/i;
const CHART_COMPLETION_RE =
  /(以上|上述|所有|全部|这些|the above|all).*(图表|图|可视化|chart|graph|plot).*(生成|完成|查看|效果|created|generated|ready|shown)/i;
const CHART_INTRO_RE =
  /(已生成|生成了|生成|如下|排序如下|展示如下|占比|分布|趋势|created|generated|shown below|as follows)/i;
const CHART_HEADING_PREFIX_RE =
  /^(#{1,6}\s+|(\d+(?:\.\d+)*[\.)、]?|[-*+])\s+|[一二三四五六七八九十]+[、.．]\s*)/;

const COMPLETE_IMAGE_RE = /!\[([^\]\n]{0,160})\]\(([^)\n]{0,300})\)/g;
const BARE_IMAGE_LABEL_RE = /!\[([^\]\n]{1,160})\](?!\()/g;
const EXPLICIT_CHART_PLACEHOLDER_RE = /\{\{\s*chart\s*:\s*([a-zA-Z0-9_-]+|\d+)\s*\}\}/g;
const PROVIDER_FILE_SCHEME_RE = /^(local|minio|cos|tos|s3|oss|ks3|obs):\/\/\S+$/i;
const LINE_RE = /[^\n]*(?:\n|$)/g;

function isChartLikeText(text: string): boolean {
  return CHART_PLACEHOLDER_HINT_RE.test(text.trim());
}

function isValidRenderableImageUrl(url: string): boolean {
  const trimmed = url.trim();
  if (!trimmed) return false;
  if (trimmed.startsWith('/') && !trimmed.startsWith('//')) return true;
  if (PROVIDER_FILE_SCHEME_RE.test(trimmed)) return true;

  try {
    const parsed = new URL(trimmed);
    return parsed.protocol === 'http:' || parsed.protocol === 'https:';
  } catch {
    return false;
  }
}

function overlapsExisting(matches: PlaceholderMatch[], start: number, end: number): boolean {
  return matches.some(match => start < match.end && end > match.start);
}

function collectChartPlaceholders(markdown: string): PlaceholderMatch[] {
  const matches: PlaceholderMatch[] = [];

  COMPLETE_IMAGE_RE.lastIndex = 0;
  let complete: RegExpExecArray | null;
  while ((complete = COMPLETE_IMAGE_RE.exec(markdown)) !== null) {
    const alt = complete[1] || '';
    const href = (complete[2] || '').trim();
    const label = `${alt} ${href}`;
    if (!isValidRenderableImageUrl(href) && isChartLikeText(label)) {
      matches.push({
        start: complete.index,
        end: complete.index + complete[0].length,
      });
    }
  }

  BARE_IMAGE_LABEL_RE.lastIndex = 0;
  let bare: RegExpExecArray | null;
  while ((bare = BARE_IMAGE_LABEL_RE.exec(markdown)) !== null) {
    const start = bare.index;
    const end = bare.index + bare[0].length;
    if (!isChartLikeText(bare[1] || '')) continue;
    if (overlapsExisting(matches, start, end)) continue;
    matches.push({ start, end });
  }

  return matches.sort((a, b) => a.start - b.start);
}

function collectExplicitChartPlaceholders(markdown: string): PlaceholderMatch[] {
  const matches: PlaceholderMatch[] = [];
  EXPLICIT_CHART_PLACEHOLDER_RE.lastIndex = 0;

  let match: RegExpExecArray | null;
  while ((match = EXPLICIT_CHART_PLACEHOLDER_RE.exec(markdown)) !== null) {
    matches.push({
      start: match.index,
      end: match.index + match[0].length,
      chartRef: match[1],
    });
  }

  return matches;
}

function collectLines(markdown: string): LineMatch[] {
  const lines: LineMatch[] = [];
  LINE_RE.lastIndex = 0;

  let match: RegExpExecArray | null;
  while ((match = LINE_RE.exec(markdown)) !== null) {
    const rawLine = match[0];
    if (!rawLine) break;
    lines.push({
      text: rawLine.replace(/\n$/, ''),
      start: match.index,
      end: match.index + rawLine.length,
    });
  }

  return lines;
}

function lineLooksLikeChartSection(line: string): boolean {
  const trimmed = line.trim();
  if (!trimmed) return false;
  if (!CHART_SECTION_RE.test(trimmed)) return false;
  if (CHART_COMPLETION_RE.test(trimmed)) return false;

  return (
    CHART_HEADING_PREFIX_RE.test(trimmed) ||
    /^(图表|图|可视化|chart|graph|plot)\b/i.test(trimmed)
  );
}

function lineLooksLikeChartIntro(line: string): boolean {
  const trimmed = line.trim();
  if (!trimmed) return false;
  if (!CHART_SECTION_RE.test(trimmed)) return false;
  if (CHART_COMPLETION_RE.test(trimmed)) return false;
  if (lineLooksLikeChartSection(trimmed)) return false;
  return CHART_INTRO_RE.test(trimmed);
}

function isMarkdownTableDelimiter(line: string): boolean {
  return /^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?$/.test(line.trim());
}

function buildSectionAnchorText(heading: LineMatch, sectionLines: LineMatch[], introLine?: LineMatch): string {
  const selected: string[] = [heading.text];
  if (introLine) selected.push(introLine.text);

  for (const line of sectionLines) {
    const trimmed = line.text.trim();
    if (!trimmed || line === introLine) continue;
    if (isMarkdownTableDelimiter(trimmed)) continue;

    if (trimmed.startsWith('|')) {
      selected.push(trimmed);
      break;
    }

    if (/^[-*]\s+/.test(trimmed)) continue;
    selected.push(trimmed);
    if (selected.length >= 4) break;
  }

  return selected.join('\n').slice(0, 700);
}

function findSectionIntroLine(sectionLines: LineMatch[]): LineMatch | undefined {
  let scannedNonEmptyLines = 0;

  for (const line of sectionLines) {
    const trimmed = line.text.trim();
    if (!trimmed) continue;
    if (trimmed.startsWith('|')) return undefined;

    scannedNonEmptyLines += 1;
    if (scannedNonEmptyLines > 6) return undefined;
    if (lineLooksLikeChartIntro(line.text)) return line;
  }

  return undefined;
}

function collectChartAnchors(markdown: string, desiredCount: number): ChartAnchor[] {
  if (desiredCount <= 0) return [];

  const lines = collectLines(markdown);
  const headingLines = lines.filter(line => lineLooksLikeChartSection(line.text));
  const sectionRanges: Array<{ start: number; end: number }> = [];
  const sectionAnchors = headingLines.map((heading, index): ChartAnchor => {
    const nextHeading = headingLines[index + 1];
    const sectionEnd = nextHeading?.start ?? markdown.length;
    const sectionLines = lines.filter(line => line.start > heading.start && line.start < sectionEnd);
    const introLine = findSectionIntroLine(sectionLines);
    const anchorLine = introLine || heading;

    sectionRanges.push({ start: heading.start, end: sectionEnd });
    return {
      insertAt: anchorLine.end,
      insertBefore: false,
      text: buildSectionAnchorText(heading, sectionLines, introLine),
      kind: 'section',
    };
  });

  const introAnchors = lines
    .filter(line => lineLooksLikeChartIntro(line.text))
    .filter(line => !sectionRanges.some(range => line.start >= range.start && line.start < range.end))
    .map((line): ChartAnchor => ({
      insertAt: line.end,
      insertBefore: false,
      text: line.text,
      kind: 'section',
    }));

  const completionAnchors = lines
    .filter(line => CHART_COMPLETION_RE.test(line.text.trim()))
    .map((line): ChartAnchor => ({
      insertAt: line.start,
      insertBefore: true,
      text: line.text,
      kind: 'completion',
    }));

  const anchors = [...sectionAnchors, ...introAnchors].sort((a, b) => a.insertAt - b.insertAt);
  if (anchors.length >= desiredCount) {
    return anchors.slice(0, desiredCount);
  }
  if (anchors.length > 0) {
    return anchors;
  }

  return completionAnchors.length > 0 ? [completionAnchors[0]] : [];
}

function normalizeText(text: unknown): string {
  return String(text ?? '').toLowerCase().replace(/[\s_\-`*（）()，,。:：/\\|]+/g, '');
}

function inferChartType(text: string): string {
  if (/(饼图|环形图|pie|donut)/i.test(text)) return 'pie';
  if (/(堆叠|stacked)/i.test(text)) return 'stacked_bar';
  if (/(柱状图|条形图|bar)/i.test(text)) return 'bar';
  if (/(折线图|趋势|line)/i.test(text)) return 'line';
  if (/(散点图|scatter)/i.test(text)) return 'scatter';
  if (/(面积图|area)/i.test(text)) return 'area';
  if (/(雷达图|radar)/i.test(text)) return 'radar';
  if (/(树图|treemap|tree map)/i.test(text)) return 'treemap';
  if (/(箱线图|boxplot|box plot)/i.test(text)) return 'boxplot';
  if (/(热力图|heatmap|heat map)/i.test(text)) return 'heatmap';
  if (/(漏斗图|funnel)/i.test(text)) return 'funnel';
  if (/(双轴|组合图|dual axis|combo)/i.test(text)) return 'dual_axis_combo';
  return '';
}

function chartInfoText(info: StructuredChartInfo | undefined): string {
  if (!info) return '';
  return [
    info.id,
    info.chartType,
    info.x,
    ...(info.y || []),
    ...(info.columns || []),
    info.query,
  ].join(' ');
}

function mentionsAny(normalizedAnchor: string, words: string[]): boolean {
  return words.some(word => normalizedAnchor.includes(normalizeText(word)));
}

function chartHasAnyMetric(normalizedInfo: string, fields: string[]): boolean {
  return fields.some(field => normalizedInfo.includes(normalizeText(field)));
}

function scoreTopic(
  normalizedAnchor: string,
  normalizedInfo: string,
  anchorWords: string[],
  infoFields: string[],
  points: number,
  missPenalty = 2,
): number {
  if (!mentionsAny(normalizedAnchor, anchorWords)) return 0;
  return chartHasAnyMetric(normalizedInfo, infoFields) ? points : -missPenalty;
}

function scoreChartForAnchor(anchor: ChartAnchor, info: StructuredChartInfo | undefined): number {
  if (!info) return 0;

  const anchorText = anchor.text;
  const normalizedAnchor = normalizeText(anchorText);
  const normalizedInfo = normalizeText(chartInfoText(info));
  const anchorType = inferChartType(anchorText);
  const infoType = normalizeText(info.chartType);
  let score = 0;

  if (anchorType && infoType) {
    score += anchorType === infoType ? 14 : -10;
  }
  if (mentionsAny(normalizedAnchor, ['销售额', '销售金额', '实付', '总销售额', 'pay_amt', 'paid amount', 'revenue'])) {
    score += chartHasAnyMetric(normalizedInfo, ['pay_amt', 'total_pay_amt', 'daily_pay_amt', 'revenue']) ? 8 : -3;
  }
  if (mentionsAny(normalizedAnchor, ['订单量', '订单数', '订单数量', 'order_cnt', 'orders', 'count'])) {
    score += chartHasAnyMetric(normalizedInfo, ['order_cnt', 'daily_orders', 'orders', 'count']) ? 8 : -3;
  }
  if (mentionsAny(normalizedAnchor, ['客单价', '平均', 'avg'])) {
    score += chartHasAnyMetric(normalizedInfo, ['avg_pay_amt', 'avg_gross_amt', 'average', 'avg']) ? 6 : -2;
  }
  if (mentionsAny(normalizedAnchor, ['趋势', '每日', '逐日', '日期', '时间', 'date', 'daily', 'trend'])) {
    score += chartHasAnyMetric(normalizedInfo, ['date', 'day', 'daily', 'order_date']) ? 8 : -2;
  }
  if (mentionsAny(normalizedAnchor, ['占比', '比例', '分布', 'share', 'ratio', 'percent'])) {
    score += infoType === 'pie' ? 6 : 0;
  }
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['top客户', 'top customer', '客户排名', '客户排行', '排名', '排行', 'rank'],
    ['customer_name', 'customername', 'cust_name', 'custname', 'nm', 'total_spent', 'totalspent'],
    12,
    3,
  );
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['渠道', 'channel'],
    ['channel', 'ch'],
    8,
  );
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['来源', 'source', 'src'],
    ['source_code', 'sourcecode', 'src_cd', 'srccd'],
    10,
  );
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['区域', 'region', '地区'],
    ['region_code', 'regioncode', 'region', 'reg_cd', 'regcd'],
    10,
  );
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['状态', '订单状态', 'status'],
    ['order_status', 'orderstatus', 'stat_cd', 'statcd', 'status'],
    10,
  );
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['分层', '等级', '客群', 'segment'],
    ['customer_segment', 'customersegment', 'segment', 'seg'],
    10,
  );
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['vip', '非vip'],
    ['vip_status', 'vipstatus', 'vip_flg', 'vipflg'],
    12,
  );
  score += scoreTopic(
    normalizedAnchor,
    normalizedInfo,
    ['折扣', '让利', 'discount'],
    ['discount', 'disc_amt', 'discamt', 'total_discount', 'totaldiscount', 'avg_discount', 'avgdiscount'],
    10,
  );

  for (const field of [...(info.y || []), info.x || '']) {
    if (field && normalizedAnchor.includes(normalizeText(field))) {
      score += 5;
    }
  }

  return score;
}

function nextUnusedResultIndex(used: Set<number>, availableResultCount: number): number | null {
  for (let index = 0; index < availableResultCount; index += 1) {
    if (!used.has(index)) return index;
  }
  return null;
}

function resolveExplicitChartResultIndex(
  chartRef: string | undefined,
  used: Set<number>,
  availableResultCount: number,
  chartInfos: StructuredChartInfo[],
): number | null {
  const ref = String(chartRef || '').trim();
  if (!ref) return nextUnusedResultIndex(used, availableResultCount);

  for (let index = Math.min(chartInfos.length, availableResultCount) - 1; index >= 0; index -= 1) {
    if (chartInfos[index]?.id === ref && !used.has(index)) return index;
  }

  if (/^\d+$/.test(ref)) {
    const oneBased = Number(ref) - 1;
    if (oneBased >= 0 && oneBased < availableResultCount && !used.has(oneBased)) {
      return oneBased;
    }
    const zeroBased = Number(ref);
    if (zeroBased >= 0 && zeroBased < availableResultCount && !used.has(zeroBased)) {
      return zeroBased;
    }
  }

  return nextUnusedResultIndex(used, availableResultCount);
}

function assignChartResultsToAnchors(
  anchors: ChartAnchor[],
  availableResultCount: number,
  chartInfos: StructuredChartInfo[],
): PlannedChartAnchor[] {
  const planned: PlannedChartAnchor[] = [];
  const used = new Set<number>();

  for (const anchor of anchors) {
    if (used.size >= availableResultCount) break;

    if (anchor.kind === 'completion') {
      const remaining: number[] = [];
      let next = nextUnusedResultIndex(used, availableResultCount);
      while (next !== null) {
        remaining.push(next);
        used.add(next);
        next = nextUnusedResultIndex(used, availableResultCount);
      }
      if (remaining.length > 0) {
        planned.push({ ...anchor, resultIndexes: remaining });
      }
      continue;
    }

    let bestIndex: number | null = null;
    let bestScore = Number.NEGATIVE_INFINITY;
    for (let index = 0; index < availableResultCount; index += 1) {
      if (used.has(index)) continue;
      const score = scoreChartForAnchor(anchor, chartInfos[index]);
      if (score > bestScore) {
        bestScore = score;
        bestIndex = index;
      }
    }

    const fallbackIndex = nextUnusedResultIndex(used, availableResultCount);
    const resultIndex = bestIndex !== null && bestScore > 0 ? bestIndex : fallbackIndex;
    if (resultIndex === null) continue;
    used.add(resultIndex);
    planned.push({ ...anchor, resultIndexes: [resultIndex] });
  }

  return planned;
}

function splitMarkdownByAnchors(
  markdown: string,
  anchors: PlannedChartAnchor[],
  availableResultCount: number,
): SplitStructuredChartMarkdownResult {
  const segments: StructuredChartMarkdownSegment[] = [];
  const usedResultIndexes: number[] = [];
  let cursor = 0;

  for (const anchor of anchors) {
    if (usedResultIndexes.length >= availableResultCount) break;
    const before = markdown.slice(cursor, anchor.insertAt);
    if (before) segments.push({ kind: 'markdown', content: before });

    for (const resultIndex of anchor.resultIndexes) {
      if (resultIndex >= availableResultCount) continue;
      segments.push({ kind: 'chart', resultIndex });
      usedResultIndexes.push(resultIndex);
    }

    cursor = anchor.insertAt;
  }

  if (usedResultIndexes.length === 0) {
    return { segments: [{ kind: 'markdown', content: markdown }], usedResultCount: 0, usedResultIndexes: [] };
  }

  const tail = markdown.slice(cursor);
  if (tail) segments.push({ kind: 'markdown', content: tail });

  return { segments, usedResultCount: usedResultIndexes.length, usedResultIndexes };
}

export function splitStructuredChartMarkdown(
  markdown: string,
  availableResultCount: number,
  chartInfos: StructuredChartInfo[] = [],
): SplitStructuredChartMarkdownResult {
  if (!markdown) return { segments: [], usedResultCount: 0, usedResultIndexes: [] };

  const explicitPlaceholders = collectExplicitChartPlaceholders(markdown);
  if (explicitPlaceholders.length > 0) {
    const segments: StructuredChartMarkdownSegment[] = [];
    const usedResultIndexes: number[] = [];
    const used = new Set<number>();
    let cursor = 0;

    for (const placeholder of explicitPlaceholders) {
      const before = markdown.slice(cursor, placeholder.start);
      if (before) segments.push({ kind: 'markdown', content: before });

      const resultIndex = resolveExplicitChartResultIndex(
        placeholder.chartRef,
        used,
        availableResultCount,
        chartInfos,
      );
      if (resultIndex !== null) {
        segments.push({ kind: 'chart', resultIndex });
        usedResultIndexes.push(resultIndex);
        used.add(resultIndex);
      }

      cursor = placeholder.end;
    }

    const tail = markdown.slice(cursor);
    if (tail) segments.push({ kind: 'markdown', content: tail });

    return { segments, usedResultCount: usedResultIndexes.length, usedResultIndexes };
  }

  const placeholders = collectChartPlaceholders(markdown);
  if (!placeholders.length) {
    const anchors = collectChartAnchors(markdown, availableResultCount);
    if (anchors.length > 0) {
      return splitMarkdownByAnchors(
        markdown,
        assignChartResultsToAnchors(anchors, availableResultCount, chartInfos),
        availableResultCount,
      );
    }
    return { segments: [{ kind: 'markdown', content: markdown }], usedResultCount: 0, usedResultIndexes: [] };
  }

  const segments: StructuredChartMarkdownSegment[] = [];
  const usedResultIndexes: number[] = [];
  let cursor = 0;
  let usedResultCount = 0;

  for (const placeholder of placeholders) {
    const before = markdown.slice(cursor, placeholder.start);
    if (before) segments.push({ kind: 'markdown', content: before });

    if (usedResultCount < availableResultCount) {
      segments.push({ kind: 'chart', resultIndex: usedResultCount });
      usedResultIndexes.push(usedResultCount);
      usedResultCount += 1;
    }

    cursor = placeholder.end;
  }

  const tail = markdown.slice(cursor);
  if (tail) segments.push({ kind: 'markdown', content: tail });

  return { segments, usedResultCount, usedResultIndexes };
}

export function countStructuredChartPlaceholders(
  markdown: string,
  availableResultCount: number,
  chartInfos: StructuredChartInfo[] = [],
): number {
  return splitStructuredChartMarkdown(markdown, availableResultCount, chartInfos).usedResultCount;
}
