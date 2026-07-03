package agent

// WikiTaxonomyPlanPrompt assigns a directory path (category) to every entity /
// concept page produced by ONE ingest batch in a single call, so the whole set
// lands on one coherent tree that reuses existing folders — instead of each page
// inventing its own folders in parallel (which diverges worst on the founding
// batch, when the KB still has no folders to anchor on). The result is applied
// in reduce only to pages that don't already have a category, so user edits and
// previously-filed pages are never churned.
const WikiTaxonomyPlanPrompt = `你正在把 wiki 知识库组织成导航目录。请为下方每个条目分配一个目录路径（category），使整个集合落在同一棵连贯的目录树上。

<existing_folders>
{{.ExistingTaxonomy}}
</existing_folders>

<items>
{{.Items}}
</items>

<instructions>
对每个条目，输出一个 category 路径：从宽到窄的文件夹标签数组（最多 2 层）。category 分类的是该条目本质上是什么（它稳定所在的资料架），而不是它在某篇文档中扮演的角色。

每个条目的路径选择方式：
1. 如果 <existing_folders> 中有合适的既有文件夹，复用其精确标签（逐字符一致）。不要发明同义文件夹（例如已有“春节 / 传统习俗”适配时，不要创建“春节习俗”）。
2. 如果没有合适的既有文件夹，为它创建新的、宽泛的、稳定的文件夹（例如组织 → “组织”，法律理念 → “法律概念”，地点 → “地点”）。目录不必保持很小；大多数条目都有自然归属，因此应创建合理的一级文件夹，而不是不归档。将同类条目归入同一个新文件夹，保持目录树连贯。
3. 只有当条目确实不属于任何稳定主题时，才给出空路径 []。这种情况必须很少见。没有匹配既有文件夹不是使用 [] 的理由；请创建文件夹。

其他规则：
- 将同类条目放在同一文件夹、同一深度。不要把一个等价条目放得比同级更深（例如避免“地点 / 地址 / Address1”与“地点 / Address2”并列；等价条目请选择一致深度）。
- 优先使用一个宽泛的一级文件夹；只有当多个条目共享真正稳定的子领域时，才增加第二层。
- 不要把条目类型（"entity"/"concept"）作为文件夹。不要在单个标签中放斜杠。
- <items> 中的每个 slug 都必须在输出中精确出现一次。
- 所有文件夹标签都用 {{.Language}} 书写。

### JSON 格式规则
- 只输出有效 JSON，不要前言。
- 不要在 JSON 字符串值中使用字面换行。
</instructions>

输出格式：
{
  "assignments": [
    {"slug": "entity/zhang-san", "path": ["人物"]},
    {"slug": "concept/spring-festival", "path": ["节日", "传统节日"]}
  ]
}`

// Wiki ingest prompt templates for LLM-powered wiki page generation.
// These prompts are used by the wiki ingest pipeline to extract structured
// knowledge from raw documents and build/update wiki pages.

// WikiSummaryPrompt generates a summary page for a newly ingested document.
//
// Filename and title are intentionally NOT passed to the LLM: documents
// uploaded to WeKnora often carry filenames that say nothing about the
// content (e.g. scanned PDFs named after the scanner model "MX5280.pdf"),
// and feeding such filenames to the model invites hallucinated summaries
// when the actual extracted content is thin. The model must rely solely on
// the document content provided below.
const WikiSummaryPrompt = `你是一名 wiki 编辑。请根据以下文档内容，创建一个 Markdown 格式的结构化 wiki 摘要页。

<document>
<content>
{{.Content}}
</content>
</document>

<available_wiki_pages>
{{.ExtractedSlugs}}
</available_wiki_pages>

<instructions>
1. 输出第一行必须是：SUMMARY: {一句 15-40 个词的摘要，说明本文档主题，用于 wiki 索引列表}
2. SUMMARY 行之后，用 Markdown 格式写出文档的全面摘要。
3. 包含关键事实、论点和结论。
4. 使用正确标题层级（## 表示章节，### 表示小节）。
5. **Wiki 链接规则**：上方 available_wiki_pages 列表将 slugs 映射到显示名称及其别名（格式："[[slug]] = display name (Aliases: a, b)"）。每当你提到与列表条目匹配的名称或别名时，必须写成 [[slug|display name]]（例如 [[entity/zhong-guo|中国]]），不要写成加粗（**name**）或裸 [[slug]]。使用提供的精确 slug；不要发明新 slug。
6. **图片规则**：如果文档包含带 <image> 元素的 <images> 标签，你应使用 Markdown 语法 ![caption](url) 在摘要中包含相关图片。请把图片放在与文本上下文相关的位置。![caption](url) 中的 URL 是不透明 token；必须逐字原样复现，不要修改、缩短或规范化。
7. 末尾包含一个带项目符号的 "## 关键要点" 区段。
8. 使用 {{.Language}} 书写。
9. 摘要保持简洁但充分（根据文档长度约 500-1500 字）。
10. **空内容规则**：如果上方 <content> 块为空，只包含没有提取文本的图片引用，或没有任何实质信息，请精确输出："SUMMARY: 无法从该文档中提取文本内容。"，随后用简短说明解释该文档无法总结。不要编造主题，不要从任何其他线索猜测。
</instructions>

先输出 SUMMARY 行，然后输出 Markdown 内容。不要包含任何其他前言。`

// WikiKnowledgeExtractPrompt extracts both entities and concepts in a single LLM call.
// Returns a JSON object with "entities" and "concepts" arrays.
// This replaces the former separate WikiEntityExtractPrompt and WikiConceptExtractPrompt.
const WikiKnowledgeExtractPrompt = `你是知识抽取系统。请分析以下文档，抽取所有重要实体和关键概念。

<document>
<content>
{{.Content}}
</content>
</document>

<previous_slugs>
{{.PreviousSlugs}}
</previous_slugs>

<instructions>
返回一个包含两个数组的 JSON 对象："entities" 和 "concepts"。
**重要：所有名称、描述和详情都使用 {{.Language}} 书写**。

如果上方 <content> 块为空，只包含没有提取文本的图片引用，或没有任何实质信息，请返回 {"entities": [], "concepts": []}。不要从任何其他来源编造实体或概念。

### Slug 连续性规则
如果上方提供了 previous slugs，必须遵循这些规则：
- 如果上一次抽取中的实体或概念仍存在于当前文档中，**复用上一列表中的精确 slug**。不要为同一事物生成新 slug。
- 如果某个实体或概念不再出现在文档中，**不要在输出中包含它**。
- 只为真正新增的实体/概念（不在上一列表中）生成新 slug。
- 这可确保文档更新时 slug 保持稳定。

### Entities（人物、组织、产品、地点、技术、事件等）
每个实体应包含：
- "name"：使用 {{.Language}} 书写的人类可读实体名称。
- "slug"：URL 友好的 slug，格式为 "entity/<lowercase-hyphenated-name>"（非拉丁名称使用罗马化/拼音形式）。**如果该实体以前抽取过，请复用之前的 slug。**
- "aliases"：字符串数组，表示指向完全同一实体的名称。仅包含：官方缩写（例如 "IBM" 对应 "International Business Machines"）、全称/简称变体（例如 "腾讯" 对应 "腾讯控股有限公司"）、翻译名（例如 "Apple" 对应 "苹果公司"）和知名别名（例如 "Alphabet" 对应 "Google母公司"）。不要包含父类别、相关产品、通用术语或更宽泛概念。没有则提供 []。
- "description"：**索引列表摘要**，用 {{.Language}} 写一句 15-40 个词的句子。说明该实体是什么，以及它在文档中的角色。必须自包含（无需阅读完整页面也能理解）。它会展示在 wiki 索引中。
- "details"：用 {{.Language}} 写 2-5 句摘要，总结文档中的关键事实。**图片规则**：如果文档在 <images> 标签中包含相关 <image> 元素，请用 Markdown 语法 ![caption](url) 将其加入 details。![caption](url) 中的 URL 是不透明 token；必须逐字原样复现，不要修改、缩短或规范化。

只包含有实质讨论的实体（至少提到两次或有详细描述）。不要包含通用术语。

### Concepts（主题、议题、方法论、理论等）
每个概念应包含：
- "name"：使用 {{.Language}} 书写的人类可读概念名称。
- "slug"：URL 友好的 slug，格式为 "concept/<lowercase-hyphenated-name>"（非拉丁名称使用罗马化/拼音形式）。**如果该概念以前抽取过，请复用之前的 slug。**
- "aliases"：字符串数组，表示指向完全同一概念的名称。仅包含：官方缩写（例如 "RAG" 对应 "Retrieval-Augmented Generation"）、全称/简称变体，以及该领域中可互换使用的知名同义词。不要包含子主题、相关技术、更宽泛类别或实现细节。没有则提供 []。
- "description"：**索引列表摘要**，用 {{.Language}} 写一句 15-40 个词的句子。定义该概念是什么。必须自包含（无需阅读完整页面也能理解）。它会展示在 wiki 索引中。
- "details"：用 {{.Language}} 写 2-5 句说明，描述文档中讨论的内容。**图片规则**：如果文档在 <images> 标签中包含相关 <image> 元素，请用 Markdown 语法 ![caption](url) 将其加入 details。![caption](url) 中的 URL 是不透明 token；必须逐字原样复现，不要修改、缩短或规范化。

只包含有实质讨论的概念。跳过琐碎或过于通用的概念。

### 去重规则
- 如果某物是具体命名事物（人物、公司、产品、地点），只放入 "entities"。
- 如果某物是抽象想法、方法论或理论，只放入 "concepts"。
- 不要在两个数组中重复条目。

### JSON 格式规则
- **关键**：不要在 JSON 字符串值中使用字面换行字符。如果字符串中需要换行，必须使用转义序列 \n。
</instructions>

只输出有效 JSON。示例：
{
  "entities": [
    {
      "name": "示例科技公司",
      "slug": "entity/shili-keji-gongsi",
      "aliases": ["示例科技", "示例科技有限公司"],
      "description": "一家专注于人工智能解决方案的技术公司。",
      "details": "示例科技公司成立于 2020 年，已发展到 500 名员工。该公司聚焦企业级 AI 产品，并近期推出了旗舰 RAG 平台。"
    }
  ],
  "concepts": [
    {
      "name": "检索增强生成",
      "slug": "concept/retrieval-augmented-generation",
      "aliases": ["RAG"],
      "description": "一种将信息检索与语言模型生成结合的技术。",
      "details": "RAG 先通过向量相似度搜索从知识库中检索相关文档，再将这些文档作为上下文提供给 LLM 生成答案。"
    }
  ]
}`

// WikiCandidateSlugPrompt (Pass 0 of the chunk-cited pipeline) asks the LLM to
// scan a document and output the SKELETON of all entities/concepts it contains:
// name, slug, aliases, a short description, and a short details tiebreaker.
// The heavy lifting — linking each slug to concrete supporting chunks — is
// done in a second pass (see WikiChunkCitationPrompt). Because this prompt no
// longer has to carry full facts per item, it stays cheap even for long docs.
const WikiCandidateSlugPrompt = `你是知识抽取系统。请分析以下文档，并以轻量候选集形式列出所有重要实体和关键概念。稍后的另一轮会为每个条目附加具体支持 chunk，因此这里不需要为每个条目写详尽事实。

<document>
<content>
{{.Content}}
</content>
</document>

<previous_slugs>
{{.PreviousSlugs}}
</previous_slugs>

<instructions>
返回一个包含两个数组的 JSON 对象："entities" 和 "concepts"。
**重要：所有名称、描述和详情都使用 {{.Language}} 书写**。

如果上方 <content> 块为空，只包含没有提取文本的图片引用，或没有任何实质信息，请返回 {"entities": [], "concepts": []}。不要从任何其他来源编造实体或概念。

### 抽取范围（粒度：{{.Granularity}}）
{{.GranularityGuidance}}

### Slug 连续性规则
如果上方提供了 previous slugs，必须遵循这些规则：
- 如果上一次抽取中的实体或概念仍存在于当前文档中，**复用上一列表中的精确 slug**。不要为同一事物生成新 slug。
- 如果某个实体或概念不再出现在文档中，**不要在输出中包含它**。
- 只为真正新增的实体/概念（不在上一列表中）生成新 slug。
- 这可确保文档更新时 slug 保持稳定。

### Entities（人物、组织、产品、地点、技术、事件等）
每个实体应包含：
- "name"：使用 {{.Language}} 书写的人类可读实体名称。
- "slug"：URL 友好的 slug，格式为 "entity/<lowercase-hyphenated-name>"（非拉丁名称使用罗马化/拼音形式）。**如果该实体以前抽取过，请复用之前的 slug。**
- "aliases"：字符串数组，表示指向完全同一实体的名称。仅包含：官方缩写（例如 "IBM" 对应 "International Business Machines"）、全称/简称变体（例如 "腾讯" 对应 "腾讯控股有限公司"）、翻译名和知名别名。不要包含父类别、相关产品、通用术语或更宽泛概念。没有则提供 []。
- "description"：**索引列表摘要**，用 {{.Language}} 写一句 15-40 个词的句子。说明该实体是什么，以及它在文档中的角色。必须自包含。它会展示在 wiki 索引中。
- "details"：用 {{.Language}} 写 1-3 句简短兜底摘要。它只在下游 chunk 级引用失败时使用，因此不需要详尽。保持在 300 字符以内。

应用上方抽取范围规则。绝不要把仅被轻微提及的名称提升为实体。

### Concepts（主题、议题、方法论、理论等）
每个概念应包含：
- "name"：使用 {{.Language}} 书写的人类可读概念名称。
- "slug"：URL 友好的 slug，格式为 "concept/<lowercase-hyphenated-name>"（非拉丁名称使用罗马化/拼音形式）。**如果该概念以前抽取过，请复用之前的 slug。**
- "aliases"：字符串数组，表示指向完全同一概念的名称。仅包含：官方缩写（例如 "RAG" 对应 "Retrieval-Augmented Generation"）、全称/简称变体，以及该领域中可互换使用的知名同义词。不要包含子主题、相关技术、更宽泛类别或实现细节。没有则提供 []。
- "description"：**索引列表摘要**，用 {{.Language}} 写一句 15-40 个词的句子。定义该概念是什么。必须自包含。
- "details"：用 {{.Language}} 写 1-3 句简短兜底摘要。保持在 300 字符以内。

应用上方抽取范围规则。跳过只被点名而没有讨论的概念。

### 去重规则
- 如果某物是具体命名事物（人物、公司、产品、地点），只放入 "entities"。
- 如果某物是抽象想法、方法论或理论，只放入 "concepts"。
- 不要在两个数组中重复条目。

### JSON 格式规则
- **关键**：不要在 JSON 字符串值中使用字面换行字符。如果字符串中需要换行，必须使用转义序列 \n。
</instructions>

只输出有效 JSON。示例：
{
  "entities": [
    {
      "name": "示例科技公司",
      "slug": "entity/shili-keji-gongsi",
      "aliases": ["示例科技", "示例科技有限公司"],
      "description": "一家专注于人工智能解决方案的技术公司。",
      "details": "成立于 2020 年，聚焦企业级 AI 产品。"
    }
  ],
  "concepts": [
    {
      "name": "检索增强生成",
      "slug": "concept/retrieval-augmented-generation",
      "aliases": ["RAG"],
      "description": "一种将信息检索与语言模型生成结合的技术。",
      "details": "先检索文档，再将其作为上下文提供给 LLM。"
    }
  ]
}`

// WikiChunkCitationPrompt (Pass 1..N of the chunk-cited pipeline) asks the LLM
// to read a batch of chunks and, for each candidate entity/concept, list the
// chunk IDs that substantively discuss it. This keeps per-slug "facts" in
// their verbatim form (the chunk text) instead of asking the LLM to paraphrase.
// Block order matters for provider prefix caching: the static rules,
// output schema and the per-document-stable <candidate_slugs> are placed
// BEFORE the per-batch <chunks> block. Within one document only ChunksXML
// changes between batches, so every batch after the first shares the long
// [rules | candidate_slugs] prefix and avoids re-billing the static rules.
const WikiChunkCitationPrompt = `你是精确引用系统。你的任务是扫描一批文档 chunk，并判断下方每个候选实体/概念由哪些 chunk 进行了实质性讨论。

<instructions>
**重要：所有名称、描述和详情都使用 {{.Language}} 书写**。

### 主要任务
对下方 <candidate_slugs> 中列出的每个候选 slug，从下方 <chunks> 块中选择**实质性讨论**该实体/概念的 chunk ID。“实质性”指 chunk 至少陈述了关于该候选项的一个具体事实、属性、步骤、日期、数字、关系或其他有用信息，而不是顺带提及。

- 只引用下方 <chunks> 块中出现的 chunk。
- 逐字使用每个 <c> 元素的 "id" 属性（例如 "c003"）。
- 如果某个候选项在本批次任何 chunk 中都没有被有意义地讨论，请在输出中省略它（不要包含空数组）。
- 如果一个 chunk 确实讨论了多个候选项，它可以被多个候选项引用。
- 如果一个 chunk 过长或混合了无关主题，仍要为它讨论到的每个候选项引用它。

### 次要任务：new slugs
如果本批次揭示了 <candidate_slugs> 中**不存在**的重要实体/概念，你可以把它加入 "new_slugs"，以便纳入。只添加真正新的、被实质性讨论的条目。不要重新发现已列在 <candidate_slugs> 中的条目；如果它已经是候选项，请复用其 slug。

每个 new slug 必须包含：
- "type"："entity" 或 "concept"
- "name"、"slug"、"aliases"、"description"、"details"（语义与候选列表相同）
- "source_chunks"：当前批次中讨论它的 chunk ID 列表

### JSON 格式规则
- **关键**：不要在 JSON 字符串值中使用字面换行字符。如有需要，使用 \n。
- 只输出有效 JSON，不要前言。
</instructions>

输出格式：
{
  "citations": {
    "entity/xxx": ["c001", "c003"],
    "concept/yyy": ["c002"]
  },
  "new_slugs": [
    {
      "type": "entity",
      "name": "示例实体",
      "slug": "entity/shili-shiti",
      "aliases": [],
      "description": "...",
      "details": "...",
      "source_chunks": ["c005"]
    }
  ]
}

如果本批次没有值得引用的内容，请返回：{"citations": {}, "new_slugs": []}

<candidate_slugs>
{{.CandidateSlugs}}
</candidate_slugs>

<chunks>
{{.ChunksXML}}
</chunks>

现在将上述指令应用到 chunks，并只输出 JSON。`

// WikiPageModifyPrompt updates an existing wiki page with new additions and removes stale/deleted information in a single pass.
const WikiPageModifyPrompt = `你是一名 wiki 编辑，任务是更新既有 wiki 页面。你必须处理一组需要添加的新信息，和/或一组已删除文档中必须移除的独有贡献。

### 严格引用与合并规则（关键）：
1. **保留引用：** 合并新信息与既有内容时，必须严格保留所有既有行内 chunk 引用（例如 [c003]）。
2. **强制追踪：** 任何新添加的事实性声明、实体或数字数据后面，都必须跟随指向相应新源 chunk 的行内引用（例如 [c003]）。
3. **不幻觉：** 不要编造、综合或推断任何未在提供的源 chunk 中明确出现的信息。如果新 chunk 清楚且直接地取代或矛盾于既有内容，请更新正文以反映带引用的新信息，并添加一个简短的“矛盾 / 更新”区段总结变化。如果冲突含糊、未解决或未被提供的 chunk 直接支持，不要覆盖既有内容；只添加一个“矛盾 / 更新”区段，用引用描述该冲突。

<page_metadata>
  <slug>{{.PageSlug}}</slug>
  <title>{{.PageTitle}}</title>
  <type>{{.PageType}}</type>{{if .PageAliases}}
  <aliases>{{.PageAliases}}</aliases>{{end}}
</page_metadata>

此 wiki 页面专门关于 **{{.PageTitle}}**（{{.PageType}}）。页面上的每条陈述都必须直接关于这个精确的 {{.PageType}}，而不是关于相关、相邻或名称相似的事物。

<existing_page_content>
{{.ExistingContent}}
</existing_page_content>

{{if .HasAdditions}}
<new_information>
{{.NewContent}}
</new_information>

上方 <new_information> 块由逐字源 chunk 组成，这些 chunk 已被引用为直接支持此页面。每个文档内可选的 <source_context> 块是文档级摘要，会同时告诉你该文档讲什么以及它是什么类型的文档（例如简历、公告、产品页、日程）。请用它校准语气、保持主题相关并避免过度宣传。不要把 source_context 文本引用进页面；它只提供框架。
{{end}}

{{if .HasRetractions}}
<deleted_documents>
{{.DeletedContent}}
</deleted_documents>

<remaining_source_documents>
{{.RemainingSourcesContent}}
</remaining_source_documents>
{{end}}

<valid_wiki_links>
{{.AvailableSlugs}}
</valid_wiki_links>

<instructions>
1. 输出第一行必须是：SUMMARY: {一句 15-40 个词的摘要，说明更新后此页面的主题，用于 wiki 索引列表}
{{if .HasRetractions}}
2. 移除那些只来源于 <deleted_documents>，且不存在于任何 <remaining_source_documents> 或 <new_information> 中的事实/声明。
{{end}}
{{if .HasAdditions}}
3. 将 <new_information> 中的事实添加并合并到页面。你是编译器，不是创作者：
   - **关键冲突检查**：先确认 <new_information> 确实关于 **{{.PageTitle}}**（如 <page_metadata> 所声明）。如果某条新信息明显属于另一个不同但相关的事物（例如此页面关于“混元模型”，而新信息关于“Qwen3”；或此页面关于“居民身份证”，而新信息关于“工作居住证”），你必须拒绝这部分新信息，不要添加。
   - 如果它确实关于 {{.PageTitle}} 且与旧内容矛盾，优先使用较新的信息。
   - **贴近源文措辞。** chunk 是逐字内容。复用源文自己的句子；你可以轻微重排、去重并连接相关句子，但不要为了风格改写，不要把短陈述扩展成长陈述，不要编造过渡句。
   - **不要过度结构化。** 只有当源文本本身使用该标题，或页面既有内容已有该标题时，才引入章节标题（##、###）。对于源文本扁平的新页面，优先使用单个 "# {{.PageTitle}}" 标题，加 1-2 个短段落和扁平事实项目列表，而不是发明小节层级。
   - **不要添加修辞填充。** 除非源 chunk 中逐字出现，否则不要出现“旨在帮助…”“该平台致力于…”“具有重要意义”“设计用于…”“旨在提供…”等宣传性短语。
   - **范围纪律。** source_context 会告诉你文档是自述材料（例如简历）还是第三方权威材料。如果来源是自述材料，不要把声明提升为行业级陈述；保持描述性，必要时归因（来源为第一人称时，“根据简历所述…” / “据其描述…”可接受）。
{{end}}
4. 保留仍然有效且仍然关于 {{.PageTitle}} 的既有信息。
5. 只有当 slug 出现在上方 <valid_wiki_links> 列表中时，才保留 [[slug|name]] wiki-link 引用。移除任何 slug 不在列表中的 [[slug|name]]。不要发明新的 wiki-link slug。页面自身 slug（{{.PageSlug}}）不得在自身内容中以 [[...]] 链接形式出现。
6. 保持既有页面结构和格式风格。如果页面尚无一级标题，使用 "# {{.PageTitle}}" 作为顶级标题。不要引入超出源文本或既有页面支撑的新标题层级。
7. **图片规则**：适用时，从新信息中使用 Markdown 语法 ![caption](url) 包含相关图片。![caption](url) 中的 URL 是不透明 token；必须逐字原样复现，不要修改、缩短或规范化。
{{if .HasRetractions}}
8. 如果移除已删除内容后页面几乎为空，且没有新信息可添加，只输出："SUMMARY: （空页面）\n# {{.PageTitle}}\n\n*该页面的主要源文档已删除。*"
{{end}}
9. 使用 {{.Language}} 书写。
</instructions>

先输出 SUMMARY 行，然后输出更新后的 Markdown 内容。不要包含任何其他前言。`

// WikiIndexIntroPrompt generates the introduction for a NEW index page (first time only).
const WikiIndexIntroPrompt = `你是一名 wiki 编辑。请为 wiki 知识库索引页写一段简短介绍。

<document_summaries>
{{.DocumentSummaries}}
</document_summaries>

<instructions>
1. 写一行以 "# " 开头、能反映知识领域的标题。
2. 然后根据上方文档摘要，用 2-3 句话描述此 wiki 覆盖什么内容。
3. 保持简洁；这只是页首区段，目录列表会在下方单独添加。
4. 使用 {{.Language}} 书写。
</instructions>

只输出标题和介绍段落。不要生成任何目录列表或页面链接。`

// WikiIndexIntroUpdatePrompt incrementally updates an existing index introduction.
const WikiIndexIntroUpdatePrompt = `你是一名 wiki 编辑。请更新 wiki 索引页的介绍区段，以反映近期变化。

<current_introduction>
{{.ExistingIntro}}
</current_introduction>

<changes>
{{.ChangeDescription}}
</changes>

<document_summaries>
{{.DocumentSummaries}}
</document_summaries>

<instructions>
1. 更新介绍，使其准确反映 wiki 当前状态。
2. 如果新增了文档，且新主题显著改变 wiki 范围，请提及这些新主题。
3. 如果删除了文档，且相关主题不再适用，请移除对这些主题的引用。
4. 保持与既有介绍相同的语气、风格和标题格式。
5. 保持简洁：1 行标题 + 2-3 句话。
6. 使用 {{.Language}} 书写。
</instructions>

只输出更新后的标题和介绍段落。不要生成任何目录列表或页面链接。`

// WikiLogEntryTemplate is a simple template for log entries (not LLM-generated).
const WikiLogEntryTemplate = `## [{{.Date}}] {{.Operation}} | {{.Title}}
- **来源**：{{.SourceInfo}}
- **影响页面**：{{.PagesAffected}}
- **摘要**：{{.Summary}}
`

// WikiDeduplicationPrompt asks the LLM to identify duplicate entities/concepts
// between newly extracted items and existing wiki pages.
const WikiDeduplicationPrompt = `你是严格去重系统。给定一组新抽取条目和一组既有 wiki 页面，请判断哪些新条目与既有页面指向**完全相同**的现实实体或概念。

<new_items>
{{.NewItems}}
</new_items>

<existing_pages>
{{.ExistingPages}}
</existing_pages>

<instructions>
### 合并标准：必须全部为真
1. 新条目和既有页面指向**同一个现实事物**（同一人物、同一组织、同一具体概念）。
2. 匹配属于**名称变体**：缩写 ↔ 全称、翻译名，或轻微拼写差异。
3. 类型兼容：entities 与 entities 合并，concepts 与 concepts 合并。**绝不要把实体合并进概念，反之亦然。**

### 正确合并示例：
- "示例科技" → "示例科技有限公司"（同一公司，简称）
- "RAG" → "检索增强生成"（同一概念，缩写）
- "苹果公司" → "Apple Inc."（同一实体，翻译名）

### 错误合并示例：不要合并这些
- "混元模型" → "Qwen 模型"（同一类别中的竞品是不同实体，不要合并）
- "iPhone 15" → "Huawei Mate 60"（同一类别中的不同具体实例）
- "GPT-4" → "GPT-3.5"（产品的不同版本是不同实体）
- "AI 安全" → "内容审核机制"（相关主题，但不同概念）
- "运动员注册" → "学历验证"（都涉及验证，但领域完全不同）
- "比赛项目类别" → "年龄组"（年龄组是类别的一个方面，不是同一概念）
- "成绩标准" → "比赛轮次"（都与比赛有关，但属于不同概念）
- "机器学习" → "神经网络"（神经网络是机器学习的子集，不是同一概念）
- "居民身份证 / Resident ID Card" → "工作居住证 / Work Residence Permit"（都是政府签发证件，但凭证完全不同）
- "驾驶证 / Driver's License" → "行驶证 / Vehicle Registration"（都与车辆相关，但证件不同）
- "学位证 / Degree Certificate" → "毕业证 / Graduation Certificate"（都是教育文档，但不同）

### 关键原则：**相关 ≠ 相同**。两个条目名称共享少数字符，或属于同一领域 / 文档家族 / 行业，不是合并理由。**绝对不要**仅因为同属一个类别，就合并不同产品、不同公司、不同版本或不同证书/文档。拿不准时，不要合并。把同一事物拆成两个页面，远比错误合并两个不同事物更可接受。

返回一个包含 "merges" map 的 JSON 对象。key 是新条目的 slug，value 是它应合并到的既有页面 slug。只包含你高度确信是同一事物的条目。

如果没有条目匹配任何既有页面，请返回：{"merges": {}}

### JSON 格式规则
- **关键**：不要在 JSON 字符串值中使用字面换行字符。如果字符串中需要换行，必须使用转义序列 \n。
</instructions>

只输出有效 JSON。示例：
{"merges": {"entity/shili-keji-youxian-gongsi": "entity/shili-keji", "concept/rag": "concept/retrieval-augmented-generation"}}`

// Granularity guidance blocks injected into WikiCandidateSlugPrompt. The
// pipeline resolves a KnowledgeBase's configured granularity to one of these
// strings via WikiGranularityGuidance().
//
// The three levels form a spectrum from "only the document's main subjects"
// to "every named thing you see". Moving down the list monotonically
// increases the candidate slug count, the downstream chunk-citation cost,
// and the noise-to-signal ratio of the wiki index.
const (
	WikiGranularityGuidanceFocused = `**FOCUSED 模式：强力裁剪。**
只抽取文档的主要主题：这篇文档本质上讨论的少量实体/概念。

包含：
- 文档的主要主题。例如简历中的人物及其具名项目；公告中的发布组织以及被公告的事件/产品；产品页中的产品本身及其制造方。
- entities 和 concepts 合计最多 3-7 个条目。

排除（即使明确命名）：
- 顺带提到的技术栈 / 库 / 框架（例如简历中列出 "Spring Boot, MySQL, Redis"，不要抽取这些）。
- 仅被引用的通用概念和方法论（例如作为实现细节提到的“微服务”“异步处理”“无状态认证”“流式响应”）。
- 仅作为背景提到的地点、学校或组织（例如简历所有者的母校，除非文档本身就是关于该学校）。
- 任何因为内容不足而通常只能写一句描述的条目。

如果不确定某个条目是否应包含，请排除它。干净、聚焦的索引比全面但嘈杂的索引更有价值。`

	WikiGranularityGuidanceStandard = `**STANDARD 模式：平衡（默认）。**
抽取文档主要主题，以及被实质性讨论的实体/概念；实质性讨论指它们有专门段落、多个项目符号，或至少 2-3 句上下文。

包含：
- 文档的主要主题。
- 拥有具体内容块的次要实体/概念（段落、多点列表或专门小节）。
- 当文档解释主题如何使用某个具名方法论、架构或技术时，抽取它们；不要只因点名就抽取。

排除：
- 仅出现在逗号分隔技术列表中且没有进一步解释的条目（例如 "Tech stack: A, B, C, D"；除非 A/B/C/D 各自在其他位置也有自己的段落，否则都不抽取）。
- 一次性提及、括号引用和通用基础设施名词。
- 对文档的全部贡献只够写成一句短句的条目。

目标是紧凑、精选的索引。对边缘条目拿不准时，优先排除。`

	WikiGranularityGuidanceExhaustive = `**EXHAUSTIVE 模式：最大召回。**
抽取每个具名实体和每个可识别概念，包括只被点名一次的技术、工具、标准和方法论，前提是它们具体且知名（不是“数据库”或“函数”等通用术语）。

包含：
- 所有主要和次要主题。
- 所有具名技术、库、框架、数据库、服务、协议或标准。
- 所有拥有广泛使用名称的可识别概念和方法论（例如 RAG、微服务、异步处理、SSE、JWT）。

只排除：
- 真正通用的术语（例如“服务器”“函数”“数据”）。
- 只出现在 URL 路径或参考引用中的条目。

当知识库更像技术术语表，而不是精选叙事 wiki 时，使用此模式。`
)

// WikiGranularityGuidance returns the guidance text to inject into the
// WikiCandidateSlugPrompt template for the given granularity. Accepts the
// raw string value stored in WikiConfig.ExtractionGranularity; callers do
// NOT need to Normalize() first — unknown values fall through to standard.
func WikiGranularityGuidance(granularity string) string {
	switch granularity {
	case "focused":
		return WikiGranularityGuidanceFocused
	case "exhaustive":
		return WikiGranularityGuidanceExhaustive
	default:
		return WikiGranularityGuidanceStandard
	}
}
