import { readFile } from 'node:fs/promises'
import { basename, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const baseUrl = String(process.env.WEKNORA_BASE_URL || 'http://localhost:8080').replace(/\/+$/, '')
const token = String(process.env.WEKNORA_TOKEN || '').trim()
const modelFilter = new Set(
  String(process.env.WEKNORA_MODEL_FILTER || '')
    .split(',')
    .map((value) => value.trim())
    .filter(Boolean),
)
const taskFilter = new Set(
  String(process.env.WEKNORA_TASK_FILTER || '')
    .split(',')
    .map((value) => value.trim())
    .filter(Boolean),
)
const runId = new Date().toISOString().replace(/\D/g, '').slice(0, 14)
const fixturePath = resolve(
  fileURLToPath(new URL('.', import.meta.url)),
  'fixtures',
  'weknora_model_acceptance.md',
)

if (!token) throw new Error('WEKNORA_TOKEN is required')

const created = {
  sessions: new Set(),
  agents: new Set(),
  knowledgeBaseId: '',
}

function dataOf(payload) {
  return payload && Object.prototype.hasOwnProperty.call(payload, 'data')
    ? payload.data
    : payload
}

async function parseJson(response) {
  const text = await response.text()
  if (!text.trim()) return null
  try {
    return JSON.parse(text)
  } catch {
    return { raw: text }
  }
}

async function api(path, { method = 'GET', body, expected = [200] } = {}) {
  const response = await fetch(`${baseUrl}${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const payload = await parseJson(response)
  if (!expected.includes(response.status)) {
    throw new Error(
      `${method} ${path} returned ${response.status}: ${JSON.stringify(payload).slice(0, 1200)}`,
    )
  }
  return dataOf(payload)
}

async function uploadFixture(knowledgeBaseId) {
  const bytes = await readFile(fixturePath)
  const form = new FormData()
  form.append('file', new Blob([bytes], { type: 'text/markdown' }), basename(fixturePath))
  form.append('fileName', basename(fixturePath))
  const response = await fetch(
    `${baseUrl}/api/v1/knowledge-bases/${encodeURIComponent(knowledgeBaseId)}/knowledge/file`,
    {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}` },
      body: form,
    },
  )
  const payload = await parseJson(response)
  if (![200, 201].includes(response.status)) {
    throw new Error(`fixture upload returned ${response.status}: ${JSON.stringify(payload).slice(0, 1200)}`)
  }
  const knowledge = dataOf(payload)
  if (!knowledge?.id) throw new Error('fixture upload returned no knowledge id')
  return String(knowledge.id)
}

async function waitForKnowledge(knowledgeId, timeoutMs = 10 * 60 * 1000) {
  const started = Date.now()
  while (Date.now() - started < timeoutMs) {
    const knowledge = await api(`/api/v1/knowledge/${encodeURIComponent(knowledgeId)}`)
    if (knowledge?.core_status === 'ready') {
      return { knowledge, elapsedMs: Date.now() - started }
    }
    if (['failed', 'cancelled'].includes(String(knowledge?.parse_status || ''))) {
      throw new Error(
        `knowledge processing ended in ${knowledge.parse_status}: ${knowledge.error_message || ''}`,
      )
    }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 2000))
  }
  throw new Error(`knowledge ${knowledgeId} did not become core-ready within ${timeoutMs}ms`)
}

async function createSession(label) {
  const session = await api('/api/v1/sessions', {
    method: 'POST',
    body: {
      title: `Local model workflow ${runId} ${label}`,
      description: 'Temporary local business workflow model acceptance; safe to delete.',
    },
    expected: [200, 201],
  })
  if (!session?.id) throw new Error(`session ${label} returned no id`)
  created.sessions.add(String(session.id))
  return String(session.id)
}

function configureCommon(config, modelId, rerankModelId) {
  const value = structuredClone(config)
  value.model_id = modelId
  value.query_understand_model_id = modelId
  value.rerank_model_id = rerankModelId
  value.temperature = 0.1
  value.thinking = false
  value.enable_artifacts = false
  value.max_iterations = 40
  value.llm_call_timeout = 300
  value.mcp_selection_mode = 'none'
  value.mcp_services = []
  value.skills_selection_mode = 'none'
  value.selected_skills = []
  value.lightweight_skills_selection_mode = 'none'
  value.selected_lightweight_skills = []
  value.professional_skills_selection_mode = 'none'
  value.selected_professional_skills = []
  value.web_search_enabled = false
  value.claude_sdk_web_search_enabled = false
  value.web_fetch_enabled = false
  value.multi_turn_enabled = false
  value.history_turns = 0
  value.image_upload_enabled = false
  value.audio_upload_enabled = false
  return value
}

async function createAgent({ builtin, model, rerankModelId, kind, dbSourceId = '' }) {
  const config = configureCommon(builtin.config, model.id, rerankModelId)
  if (kind === 'general-agent') {
    config.agent_type = 'general-agent'
    config.allowed_tools = []
    config.kb_selection_mode = 'none'
    config.knowledge_bases = []
    config.db_data_sources = []
  } else {
    config.agent_type = 'data-analysis'
    config.kb_selection_mode = 'none'
    config.knowledge_bases = []
    config.db_data_sources = [dbSourceId]
  }
  const agent = await api('/api/v1/agents', {
    method: 'POST',
    body: {
      name: `Local ${kind} ${model.name} ${runId}`,
      description: 'Temporary local business workflow model acceptance; safe to delete.',
      config,
    },
    expected: [200, 201],
  })
  if (!agent?.id) throw new Error(`${kind}/${model.name} returned no agent id`)
  created.agents.add(String(agent.id))
  return agent
}

function collectSseLine(state, line) {
  if (!line.startsWith('data:')) return
  const raw = line.slice(5).trim()
  if (!raw || raw === '[DONE]') return
  state.events.push(JSON.parse(raw))
}

async function stream(path, body, timeoutMs) {
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), timeoutMs)
  const started = Date.now()
  try {
    const response = await fetch(`${baseUrl}${path}`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: 'text/event-stream',
        'Content-Type': 'application/json',
      },
      body: JSON.stringify(body),
      signal: controller.signal,
    })
    if (response.status !== 200) {
      const payload = await parseJson(response)
      return {
        httpStatus: response.status,
        elapsedMs: Date.now() - started,
        events: [],
        transportError: JSON.stringify(payload).slice(0, 1000),
      }
    }
    const state = { events: [], buffer: '' }
    const decoder = new TextDecoder()
    const reader = response.body.getReader()
    while (true) {
      const { value, done } = await reader.read()
      if (done) break
      state.buffer += decoder.decode(value, { stream: true })
      while (true) {
        const newline = state.buffer.indexOf('\n')
        if (newline < 0) break
        const line = state.buffer.slice(0, newline).replace(/\r$/, '')
        state.buffer = state.buffer.slice(newline + 1)
        collectSseLine(state, line)
      }
    }
    state.buffer += decoder.decode()
    if (state.buffer.trim()) collectSseLine(state, state.buffer.replace(/\r$/, ''))
    return {
      httpStatus: response.status,
      elapsedMs: Date.now() - started,
      events: state.events,
      transportError: '',
    }
  } catch (error) {
    return {
      httpStatus: 0,
      elapsedMs: Date.now() - started,
      events: [],
      transportError: `${error?.name || 'Error'}: ${error?.message || error}`,
    }
  } finally {
    clearTimeout(timeout)
  }
}

function normalizedAnswer(events) {
  const answerEvents = events.filter((event) => event.response_type === 'answer')
  let answer = answerEvents.map((event) => String(event.content || '')).join('')
  if (!answer) {
    const complete = events.find((event) => event.response_type === 'complete')
    answer = String(complete?.data?.final_answer || complete?.content || '')
  }
  return answer
}

function eventToolName(event) {
  return String(event?.data?.tool_name || event?.tool_name || '')
}

function toolMatches(toolName, expected) {
  return toolName === expected || toolName.endsWith(`__${expected}`) || toolName.endsWith(`_${expected}`)
}

function compactWhitespace(value) {
  return String(value || '').replace(/[\s*_`~]+/g, '')
}

function evaluateStream({
  model,
  task,
  run,
  requiredText,
  forbiddenText = [],
  requiredTools = [],
  requireReferences = false,
}) {
  const events = run.events || []
  const answer = normalizedAnswer(events)
  const compactAnswer = compactWhitespace(answer)
  const toolNames = events.map(eventToolName).filter(Boolean)
  const errors = events
    .filter((event) => event.response_type === 'error' && event.done !== false)
    .map((event) => String(event.content || event?.data?.error || 'unknown stream error'))
  const missingText = requiredText.filter(
    (text) => !answer.includes(text) && !compactAnswer.includes(compactWhitespace(text)),
  )
  const presentForbiddenText = forbiddenText.filter(
    (text) => answer.includes(text) || compactAnswer.includes(compactWhitespace(text)),
  )
  const missingTools = requiredTools.filter(
    (expected) => !toolNames.some((name) => toolMatches(name, expected)),
  )
  const referenceCount = events.reduce((total, event) => {
    const refs = event.knowledge_references || event?.data?.knowledge_references || []
    return total + (Array.isArray(refs) ? refs.length : 0)
  }, 0)
  const thinkingEvents = events.filter((event) => event.response_type === 'thinking')
  const thinkingCharacters = thinkingEvents.reduce(
    (total, event) => total + [...String(event?.content || '')].length,
    0,
  )
  const complete = events.some((event) => event.response_type === 'complete')
  const passed =
    run.httpStatus === 200 &&
    !run.transportError &&
    errors.length === 0 &&
    complete &&
    answer.length > 0 &&
    missingText.length === 0 &&
    presentForbiddenText.length === 0 &&
    missingTools.length === 0 &&
    thinkingEvents.length === 0 &&
    (!requireReferences || referenceCount > 0)
  const result = {
    model: model.name,
    model_id: model.id,
    task,
    passed,
    http_status: run.httpStatus,
    elapsed_ms: run.elapsedMs,
    event_count: events.length,
    answer_characters: [...answer].length,
    tool_names: [...new Set(toolNames)],
    reference_count: referenceCount,
    thinking_event_count: thinkingEvents.length,
    thinking_characters: thinkingCharacters,
    complete,
    missing_text: missingText,
    present_forbidden_text: presentForbiddenText,
    missing_tools: missingTools,
    errors,
    transport_error: run.transportError,
    answer_excerpt: answer.replace(/\s+/g, ' ').slice(0, 360),
  }
  if (!passed) {
    const summaries = new Map()
    for (const event of events) {
      const responseType = String(event?.response_type || '')
      const toolName = eventToolName(event)
      const key = `${responseType}\u0000${toolName}`
      if (!summaries.has(key)) {
        summaries.set(key, {
          response_type: responseType,
          tool_name: toolName,
          event_count: 0,
          content_characters: 0,
          first_content_excerpt: '',
          done_event_count: 0,
          data_keys: new Set(),
          complete_final_answer_characters: 0,
        })
      }
      const summary = summaries.get(key)
      const content = String(event?.content || '')
      summary.event_count += 1
      summary.content_characters += [...content].length
      if (!summary.first_content_excerpt && content.trim()) {
        summary.first_content_excerpt = content.replace(/\s+/g, ' ').slice(0, 160)
      }
      if (event?.done === true) summary.done_event_count += 1
      if (event?.data && typeof event.data === 'object' && !Array.isArray(event.data)) {
        for (const dataKey of Object.keys(event.data)) summary.data_keys.add(dataKey)
      }
      if (event?.response_type === 'complete') {
        summary.complete_final_answer_characters += [
          ...String(event?.data?.final_answer || ''),
        ].length
      }
    }
    result.event_trace = [...summaries.values()].map((summary) => ({
      ...summary,
      data_keys: [...summary.data_keys].sort(),
    }))
  }
  return result
}

async function runGeneralAgent(model, builtin, rerankModelId) {
  const marker = `GA-${runId}-${model.name}`
  const agent = await createAgent({ builtin, model, rerankModelId, kind: 'general-agent' })
  const sessionId = await createSession(`general-${model.name}`)
  const run = await stream(
    `/api/v1/agent-chat/${encodeURIComponent(sessionId)}`,
    {
      query: [
        '请作为项目经理，根据以下事实给出恰好三步上线行动清单：上线窗口为周六 22:00-23:00；错误率超过 2% 必须回滚；负责人是李明。',
        '不要调用外部搜索或知识库。最后一行原样输出校验串：',
        marker,
      ].join('\n'),
      knowledge_base_ids: [],
      knowledge_ids: [],
      agent_enabled: true,
      agent_id: agent.id,
      web_search_enabled: false,
      summary_model_id: model.id,
      disable_title: true,
      enable_memory: false,
      channel: 'api',
      mcp_service_ids: [],
      skill_names: [],
      professional_skill_names: [],
    },
    8 * 60 * 1000,
  )
  return evaluateStream({
    model,
    task: 'general-agent',
    run,
    requiredText: ['22:00', '23:00', '2%', '李明', marker],
  })
}

async function runDataAnalysis(model, builtin, rerankModelId, dbSourceId) {
  const marker = `DB-${runId}-${model.name}`
  const agent = await createAgent({
    builtin,
    model,
    rerankModelId,
    kind: 'data-analysis',
    dbSourceId,
  })
  const sessionId = await createSession(`data-${model.name}`)
  const run = await stream(
    `/api/v1/agent-chat/${encodeURIComponent(sessionId)}`,
    {
      query: [
        '请实际查询绑定的数据库，分析 e2e_sales 表：按 region 汇总 amount 总额和订单数，并指出总额最高的地区。',
        '必须先查目录和表结构再执行 SQL，不要猜测，不需要图表。答案中保留 region 原值 east/west 和精确数值。最后一行原样输出：',
        marker,
      ].join('\n'),
      knowledge_base_ids: [],
      knowledge_ids: [],
      agent_enabled: true,
      agent_id: agent.id,
      web_search_enabled: false,
      summary_model_id: model.id,
      disable_title: true,
      enable_memory: false,
      channel: 'api',
      mcp_service_ids: [],
      skill_names: [],
      professional_skill_names: [],
    },
    10 * 60 * 1000,
  )
  return evaluateStream({
    model,
    task: 'data-analysis',
    run,
    requiredText: ['east', '185.75', '2', 'west', '98', '1', marker],
    requiredTools: ['db_catalog', 'db_schema', 'db_query', 'final_answer'],
  })
}

async function runKnowledgeChat(model, knowledgeBaseId, knowledgeId) {
  const marker = `KB-${runId}-${model.name}`
  const sessionId = await createSession(`knowledge-${model.name}`)
  const run = await stream(
    `/api/v1/knowledge-chat/${encodeURIComponent(sessionId)}`,
    {
      query: [
        '仅根据选定知识库回答：北辰计划的文档识别码、上线日期、批准预算、负责人和首要风险分别是什么？应对措施要求提前多少天？',
        '答案最后一行原样输出：',
        marker,
      ].join('\n'),
      knowledge_base_ids: [knowledgeBaseId],
      knowledge_ids: [knowledgeId],
      agent_enabled: false,
      web_search_enabled: false,
      summary_model_id: model.id,
      disable_title: true,
      enable_memory: false,
      channel: 'api',
      mcp_service_ids: [],
      skill_names: [],
      professional_skill_names: [],
    },
    6 * 60 * 1000,
  )
  return evaluateStream({
    model,
    task: 'knowledge-base',
    run,
    requiredText: [
      'KBFLOW-20260801',
      '2026年9月18日',
      '360万元',
      '林梅',
      '主供应商交付延迟',
      '至少14天',
      marker,
    ],
    forbiddenText: ['360,000', '36万元'],
    requireReferences: true,
  })
}

async function cleanup() {
  for (const sessionId of created.sessions) {
    try {
      await api(`/api/v1/sessions/${encodeURIComponent(sessionId)}`, {
        method: 'DELETE',
        expected: [200, 202, 204, 404],
      })
    } catch (error) {
      console.warn(`[WARN] failed to delete session ${sessionId}: ${error.message}`)
    }
  }
  for (const agentId of created.agents) {
    try {
      await api(`/api/v1/agents/${encodeURIComponent(agentId)}`, {
        method: 'DELETE',
        expected: [200, 202, 204, 404],
      })
    } catch (error) {
      console.warn(`[WARN] failed to delete agent ${agentId}: ${error.message}`)
    }
  }
  if (created.knowledgeBaseId) {
    try {
      await api(`/api/v1/knowledge-bases/${encodeURIComponent(created.knowledgeBaseId)}`, {
        method: 'DELETE',
        expected: [200, 202, 204, 404],
      })
    } catch (error) {
      console.warn(`[WARN] failed to delete knowledge base ${created.knowledgeBaseId}: ${error.message}`)
    }
  }
}

async function main() {
  const started = Date.now()
  const [models, builtinGeneral, builtinData, dbSources] = await Promise.all([
    api('/api/v1/models'),
    api('/api/v1/agents/builtin-general-agent'),
    api('/api/v1/agents/builtin-data-analyst'),
    api('/api/v1/custom/db-analytics/sources'),
  ])
  const modelNames = ['DeepSeek-V4-Flash-INT8-local', 'Qwen3.6-27B-tool-local']
  const selectedModelNames = modelNames.filter((name) => modelFilter.size === 0 || modelFilter.has(name))
  if (selectedModelNames.length === 0) throw new Error('WEKNORA_MODEL_FILTER did not select a known chat model')
  const chatModels = selectedModelNames.map((name) => {
    const model = models.find(
      (candidate) =>
        candidate.name === name &&
        candidate.type === 'KnowledgeQA' &&
        candidate.workload_scope !== 'derivative_only',
    )
    if (!model) throw new Error(`required chat model ${name} was not found`)
    return model
  })
  const embedding = models.find(
    (candidate) => candidate.name === 'Qwen3-Embedding-8B-local' && candidate.type === 'Embedding',
  )
  const rerank = models.find(
    (candidate) => candidate.name === 'bge-reranker-v2-m3-local' && candidate.type === 'Rerank',
  )
  const derivative = models.find(
    (candidate) =>
      candidate.name === 'Qwen3.6-27B-tool-local' &&
      candidate.type === 'KnowledgeQA' &&
      candidate.workload_scope === 'derivative_only',
  )
  if (!embedding?.id || !rerank?.id || !derivative?.id) {
    throw new Error('local embedding/rerank/derivative models were not found')
  }

  const dbSourceList = Array.isArray(dbSources) ? dbSources : dbSources?.data || []
  const dbSource = dbSourceList.find(
    (candidate) =>
      candidate.id === '7a0f1cbf-8fb9-4d61-bf3b-19140d3f8e21' && candidate.status === 'active',
  )
  if (!dbSource?.id) throw new Error('active local read-only PostgreSQL analytics source was not found')
  await api(`/api/v1/custom/db-analytics/sources/${encodeURIComponent(dbSource.id)}/test`, {
    method: 'POST',
    expected: [200],
  })

  const knowledgeBase = await api('/api/v1/knowledge-bases', {
    method: 'POST',
    body: {
      name: `Local model business acceptance ${runId}`,
      type: 'document',
      embedding_model_id: embedding.id,
      summary_model_id: chatModels[0].id,
      derivative_model_id: derivative.id,
      storage_provider_config: { provider: 'local' },
      question_generation_config: { enabled: false, question_count: 0 },
      indexing_strategy: {
        vector_enabled: true,
        keyword_enabled: true,
        wiki_enabled: false,
        graph_enabled: false,
      },
    },
    expected: [201],
  })
  if (!knowledgeBase?.id) throw new Error('knowledge base creation returned no id')
  created.knowledgeBaseId = String(knowledgeBase.id)
  const knowledgeId = await uploadFixture(created.knowledgeBaseId)
  const ingestion = await waitForKnowledge(knowledgeId)

  const search = await api(
    `/api/v1/knowledge-bases/${encodeURIComponent(created.knowledgeBaseId)}/hybrid-search`,
    {
      method: 'POST',
      body: {
        query_text: '北辰计划 KBFLOW-20260801 上线日期 预算 林梅',
        match_count: 10,
        knowledge_ids: [knowledgeId],
        vector_threshold: 0,
        keyword_threshold: 0,
        disable_vector_match: false,
      },
    },
  )
  if (!Array.isArray(search) || search.length === 0) {
    throw new Error('hybrid search did not retrieve the acceptance fixture')
  }

  const results = []
  for (const model of chatModels) {
    if (taskFilter.size === 0 || taskFilter.has('general-agent')) {
      console.log(JSON.stringify({ phase: 'general-agent', model: model.name }))
      results.push(await runGeneralAgent(model, builtinGeneral, rerank.id))
    }
    if (taskFilter.size === 0 || taskFilter.has('knowledge-base')) {
      console.log(JSON.stringify({ phase: 'knowledge-base', model: model.name }))
      results.push(await runKnowledgeChat(model, created.knowledgeBaseId, knowledgeId))
    }
    if (taskFilter.size === 0 || taskFilter.has('data-analysis')) {
      console.log(JSON.stringify({ phase: 'data-analysis', model: model.name }))
      results.push(await runDataAnalysis(model, builtinData, rerank.id, dbSource.id))
    }
    console.log(
      JSON.stringify(
        {
          progress: model.name,
          results: results.filter((result) => result.model === model.name),
        },
        null,
        2,
      ),
    )
  }

  const report = {
    passed: results.every((result) => result.passed),
    run_id: runId,
    elapsed_ms: Date.now() - started,
    setup: {
      knowledge_ingestion_ms: ingestion.elapsedMs,
      hybrid_search_hits: search.length,
      database_source_tested: true,
    },
    summary: {
      passed: results.filter((result) => result.passed).length,
      failed: results.filter((result) => !result.passed).length,
      total: results.length,
    },
    results,
  }
  console.log(JSON.stringify({ final_report: report }, null, 2))
  if (!report.passed) process.exitCode = 1
}

try {
  await main()
} finally {
  await cleanup()
}
