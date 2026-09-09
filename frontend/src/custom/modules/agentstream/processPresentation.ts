// Shared by desktop, embed and mobile. Presentation never changes tool execution.
export type ProcessEvent = Record<string, any>;
export type ProcessState = "running" | "success" | "error" | "stopped";
export interface ProcessItem {
  key: string;
  text: string;
  state: ProcessState;
  category: string;
  event: ProcessEvent;
  sourceCount: number | null;
  sources: ProcessSource[];
}
export interface ProcessSource {
  id: string;
  kind: string;
  title: string;
  url?: string;
  knowledge_id?: string;
  knowledge_base?: string;
  snippets: { id: string; content: string }[];
}
export const THOUGHT_LIMIT = 3;
export const ACTION_LIMIT = 2;

export function previewText(value: unknown, tail = false): string {
  const text = String(value || "")
    .replace(/<\/?think>/g, "")
    .replace(/\s+/g, " ")
    .trim();
  const chars = Array.from(text);
  if (chars.length <= 180) return text;
  if (!tail) return chars.slice(0, 179).join("") + "…";
  const last = chars.slice(-179).join("");
  const boundary = last.search(/[。！？；]\s*/);
  return (
    "…" +
    (boundary >= 0 && boundary < 90 ? last.slice(boundary + 1).trim() : last)
  );
}
const families: [string, string[], string, string][] = [
  [
    "search",
    ["knowledge_search", "search_knowledge", "grep_chunks"],
    "正在查找相关资料",
    "资料检索完成",
  ],
  [
    "read",
    ["list_knowledge_chunks", "wiki_read_source_doc"],
    "正在阅读文档相关内容",
    "已获取文档相关内容",
  ],
  [
    "wiki",
    ["wiki_search", "wiki_read_page"],
    "正在查找 Wiki 资料",
    "已获取 Wiki 资料",
  ],
  ["web", ["web_search"], "正在搜索网页", "网页搜索完成"],
  ["webread", ["web_fetch"], "正在读取网页", "已获取网页内容"],
  [
    "graph",
    ["query_knowledge_graph"],
    "正在查找相关知识关系",
    "已获取相关关系",
  ],
  [
    "analysis",
    ["data_analysis", "table_analysis", "db_query"],
    "正在分析数据",
    "已获取分析结果",
  ],
  [
    "prepare",
    ["data_schema", "table_schema", "db_schema", "db_catalog"],
    "正在查看数据结构",
    "数据结构已获取",
  ],
  [
    "prepare",
    ["get_document_info", "database_query", "kb_list_documents"],
    "正在查找文档记录",
    "文档记录已获取",
  ],
  [
    "image",
    ["inspect_input_image", "image_analysis"],
    "正在识别图片",
    "已获取图片信息",
  ],
  ["audio", ["transcribe_input_file"], "正在转写音频", "音频转写完成"],
  ["mutation", ["kb_add_document"], "正在添加文档", "文档添加操作已完成"],
  ["mutation", ["kb_delete_document"], "正在删除文档", "文档删除操作已完成"],
  ["mutation", ["kb_replace_document"], "正在替换文档", "文档替换操作已完成"],
  [
    "mutation",
    [
      "wiki_write_page",
      "wiki_replace_text",
      "wiki_rename_page",
      "wiki_update_issue",
      "wiki_flag_issue",
    ],
    "正在更新 Wiki 内容",
    "Wiki 更新操作已完成",
  ],
  [
    "mutation",
    ["wiki_delete_page"],
    "正在删除 Wiki 页面",
    "Wiki 页面删除操作已完成",
  ],
  ["file", ["Read"], "正在读取文件", "已获取文件内容"],
  [
    "file",
    ["Bash", "execute_skill_script"],
    "正在执行处理步骤",
    "处理步骤已完成",
  ],
  ["file", ["Write", "Edit"], "正在处理文件", "文件处理步骤已完成"],
  [
    "prepare",
    ["Glob", "Grep", "read_skill", "kb_mutation_status"],
    "正在准备相关资料",
    "资料准备完成",
  ],
];
function family(name: string) {
  return families.find(([, names]) =>
    names.some(
      (n) => name === n || name.endsWith("__" + n) || name.endsWith("_" + n),
    ),
  );
}
export function safeSourceURL(value?: string): string {
  try {
    const u = new URL(value || "");
    return ["http:", "https:"].includes(u.protocol) ? u.href : "";
  } catch {
    return "";
  }
}
function parseArgs(value: unknown): ProcessEvent {
  if (typeof value === "object" && value) return value;
  try {
    return JSON.parse(String(value)) || {};
  } catch {
    return {};
  }
}
function sourcePreview(data: ProcessEvent): ProcessSource[] {
  if (Array.isArray(data.process_sources) && data.process_sources.length)
    return data.process_sources;
  const sources = new Map<string, ProcessSource>();
  for (const row of [data.results, data.knowledge_results, data.chunks].flatMap(
    (rows) => (Array.isArray(rows) ? rows : []),
  )) {
    if (!row || typeof row !== "object") continue;
    const doc = row.knowledge_id || data.knowledge_id;
    const url = safeSourceURL(row.url);
    const id = doc ? `document:${doc}` : url ? `web:${url}` : "";
    if (!id || sources.has(id)) continue;
    sources.set(id, {
      id,
      kind: doc ? "document" : "web",
      title:
        row.knowledge_title ||
        row.title ||
        data.knowledge_title ||
        url ||
        "未命名文档",
      knowledge_id: doc,
      url,
      snippets: [],
    });
  }
  return [...sources.values()].slice(0, 2);
}
export function processItem(
  event: ProcessEvent,
  index: number,
  complete = false,
): ProcessItem | null {
  const key = String(
    event.event_id || event.tool_call_id || `process:${index}`,
  );
  const data = event.tool_data || {};
  const args = parseArgs(event.arguments);
  const sources = sourcePreview(data);
  const reportedCount = data.process_source_count;
  const count =
    typeof reportedCount === "number" &&
    Number.isInteger(reportedCount) &&
    reportedCount >= sources.length
      ? reportedCount
      : null;
  const pending =
    event.type === "thinking"
      ? event.thinking === true
      : event.pending === true;
  let state: ProcessState = pending
    ? complete
      ? "stopped"
      : "running"
    : event.success === false
      ? "error"
      : "success";
  let category = "thought";
  let text = "";
  if (event.type === "thinking") {
    category = event.process_kind === "process_status" ? "status" : "thought";
    text = previewText(event.content, category === "thought");
  } else if (event.type === "tool_call") {
    const name = String(event.tool_name || "");
    if (name === "thinking" || name === "todo_write") {
      text = previewText(
        data.thought || data.task || args.thought || args.task,
        true,
      );
    } else {
      const match = family(name);
      category = match?.[0] || "tool";
      const progress = event.agent_progress?.message;
      text = progress
        ? previewText(progress)
        : match
          ? pending
            ? match[2]
            : match[3]
          : pending
            ? "正在调用外部工具"
            : "本次工具操作完成";
      if (pending && ["search", "web"].includes(category)) {
        const query =
          args.query ||
          data.query ||
          (Array.isArray(args.queries) ? args.queries[0] : "");
        if (query)
          text = `${category === "web" ? "正在搜索网页" : "正在查找资料"}：「${previewText(query)}」`;
      }
      if (
        !pending &&
        state !== "error" &&
        ["search", "web"].includes(category)
      ) {
        text =
          count === 0
            ? "本次未找到相关资料"
            : count !== null
              ? `找到 ${count} ${category === "web" ? "条网页结果" : "份相关文档"}`
              : sources.length
                ? "已找到相关资料"
                : match![3];
      }
      const title =
        data.knowledge_title ||
        args.knowledge_title ||
        args.file_path?.split(/[\\/]/).pop();
      if (title && ["read", "file", "analysis"].includes(category))
        text += `：《${title}》`;
      if (state === "error")
        text = `${match ? match[2].replace(/^正在/, "") : "本次操作"}未完成`;
      if (state === "stopped")
        text = `${match ? match[2].replace(/^正在/, "") : "本次操作"}已停止`;
    }
  } else return null;
  text = previewText(text);
  return text
    ? { key, text, state, category, event, sourceCount: count, sources }
    : null;
}

export function reasoningRoundCount(stream: ProcessEvent[]): number {
  return stream.filter(event => event.type === 'thinking' &&
    (!event.process_kind || event.process_kind === 'thought_delta') &&
    String(event.content || '').trim()).length
}

export function processItems(
  stream: ProcessEvent[],
  complete = false,
): ProcessItem[] {
  return stream
    .map((e, i) => processItem(e, i, complete))
    .filter((e): e is ProcessItem => !!e);
}
export function visibleProcess(items: ProcessItem[], pinnedKey = "") {
  const thoughts = items
    .filter((e) => e.category === "thought")
    .slice(-THOUGHT_LIMIT);
  const actions = items.filter(
    (e) => e.category !== "thought" && e.category !== "status",
  );
  const important = actions.filter(
    (e) => e.category !== "prepare" || e.state === "error",
  );
  const pool = important.length ? important : actions;
  const running = pool.filter((e) => e.state === "running");
  const recent = pool.filter((e) => e.state !== "running").at(-1);
  const pinned = actions.find((e) => e.key === pinnedKey);
  let visible: ProcessItem[] = [];
  if (pinned) visible.push(pinned);
  const active = running.filter((e) => e.key !== pinnedKey);
  if (active.length) {
    if (!pinned) visible.push(active[0]);
    const remaining = pinned ? active : active.slice(1);
    if (remaining.length === 1) visible.push(remaining[0]);
    else if (remaining.length)
      visible.push({
        key: "active-group",
        text: `另有 ${remaining.length} 项正在执行`,
        state: "running",
        category: "group",
        event: {},
        sources: [],
        sourceCount: 0,
      });
    else if (!pinned && recent) visible.push(recent);
  } else if (!pinned) visible = pool.slice(-ACTION_LIMIT);
  else {
    const latest = pool.filter((e) => e.key !== pinnedKey).at(-1);
    if (latest) visible.push(latest);
  }
  return {
    thoughts,
    actions: visible.slice(0, ACTION_LIMIT),
    status:
      items.filter((e) => e.category === "status").at(-1)?.text || "正在思考",
    hidden: Math.max(0, actions.length - visible.length),
  };
}
