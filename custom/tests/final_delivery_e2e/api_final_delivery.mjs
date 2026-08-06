const baseUrl = String(
  process.env.WEKNORA_FINAL_DELIVERY_E2E_BASE_URL || 'http://localhost:8080',
).replace(/\/+$/, '')
const token = String(process.env.WEKNORA_FINAL_DELIVERY_E2E_TOKEN || '')
const concurrency = Math.max(
  2,
  Math.min(5, Number(process.env.WEKNORA_FINAL_DELIVERY_E2E_CONCURRENCY || 3)),
)
const runId = new Date().toISOString().replace(/\D/g, '').slice(0, 14)
const createdSessions = new Set()
let createdAgentId = ''
let poolBackup = null
let poolCurrent = null

if (!token) throw new Error('WEKNORA_FINAL_DELIVERY_E2E_TOKEN is required')

function assert(condition, message) {
  if (!condition) throw new Error(`Assertion failed: ${message}`)
}

async function parseJSON(response) {
  const text = await response.text()
  if (!text.trim()) return null
  try {
    return JSON.parse(text)
  } catch {
    return { raw: text }
  }
}

function dataOf(payload) {
  return payload && Object.prototype.hasOwnProperty.call(payload, 'data')
    ? payload.data
    : payload
}

async function request(
  path,
  { method = 'GET', body, expected = [200] } = {},
) {
  const response = await fetch(`${baseUrl}${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const payload = await parseJSON(response)
  if (!expected.includes(response.status)) {
    throw new Error(
      `${method} ${path} returned ${response.status}: ${JSON.stringify(payload).slice(0, 1200)}`,
    )
  }
  return dataOf(payload)
}

async function createSession(index) {
  const session = await request('/api/v1/sessions', {
    method: 'POST',
    body: {
      title: `Final delivery E2E ${runId}-${index}`,
      description: 'Local explicit-final-v1 acceptance; safe to delete.',
    },
    expected: [200, 201],
  })
  assert(session?.id, `session ${index} was created`)
  createdSessions.add(String(session.id))
  return String(session.id)
}

async function createIsolatedAgent(modelId) {
  const agent = await request('/api/v1/agents', {
    method: 'POST',
    body: {
      name: `Final delivery E2E ${runId}`,
      description: 'Local isolated explicit-final-v1 acceptance; safe to delete.',
      config: {
        agent_mode: 'smart-reasoning',
        agent_type: 'general-agent',
        model_id: modelId,
        temperature: 0.1,
        thinking: false,
        enable_artifacts: false,
        max_iterations: 30,
        llm_call_timeout: 300,
        allowed_tools: [],
        mcp_selection_mode: 'none',
        mcp_services: [],
        skills_selection_mode: 'none',
        selected_skills: [],
        lightweight_skills_selection_mode: 'none',
        selected_lightweight_skills: [],
        professional_skills_selection_mode: 'none',
        selected_professional_skills: [],
        kb_selection_mode: 'none',
        knowledge_bases: [],
        retrieve_kb_only_when_mentioned: true,
        web_search_enabled: false,
        claude_sdk_web_search_enabled: false,
        web_fetch_enabled: false,
        multi_turn_enabled: false,
        history_turns: 0,
      },
    },
    expected: [200, 201],
  })
  assert(agent?.id, 'isolated general-agent was created')
  createdAgentId = String(agent.id)
  return agent
}

function resourcePoolBody(pool, maxInflight) {
  const total = Number(maxInflight)
  const reserve = Math.min(Number(pool.interactive_reserve), Math.max(0, total - 1))
  const background = Math.max(1, total - reserve)
  const tenant = Math.min(Math.max(1, Number(pool.tenant_burst)), total)
  return {
    id: String(pool.id),
    name: String(pool.name),
    resource_kind: String(pool.resource_kind),
    chat_max_concurrent: null,
    chat_max_waiting: pool.chat_max_waiting ?? null,
    max_inflight: total,
    max_background_inflight: background,
    interactive_reserve: reserve,
    tenant_guaranteed: 1,
    tenant_burst: tenant,
    document_guaranteed: 1,
    document_burst: Math.min(Math.max(1, Number(pool.document_burst)), tenant, background),
    rpm: Number(pool.rpm),
    tpm: Number(pool.tpm),
    token_burst: 0,
    request_timeout_seconds: Number(pool.request_timeout_seconds),
    circuit_threshold: Number(pool.circuit_threshold),
    circuit_window_seconds: Number(pool.circuit_window_seconds),
    circuit_open_seconds: Number(pool.circuit_open_seconds),
    state: String(pool.state),
  }
}

async function setModelConcurrency(modelId, minimumConcurrency) {
  const [bindings, pools] = await Promise.all([
    request('/api/v1/custom/capacity-control/bindings'),
    request('/api/v1/custom/capacity-control/resource-pools'),
  ])
  assert(Array.isArray(bindings) && Array.isArray(pools), 'model resource policy is listable')
  const binding = bindings.find((candidate) => candidate.model_id === modelId)
  assert(binding?.resource_pool_id, `model ${modelId} has a resource-pool binding`)
  const pool = pools.find((candidate) => candidate.id === binding.resource_pool_id)
  assert(pool?.id, `resource pool ${binding.resource_pool_id} exists`)
  poolBackup = structuredClone(pool)
  poolCurrent = structuredClone(pool)
  const desired = Math.max(
    minimumConcurrency,
    Number(pool.max_inflight || 0),
  )
  const response = await fetch(
    `${baseUrl}/api/v1/custom/capacity-control/resource-pools/${encodeURIComponent(pool.id)}`,
    {
      method: 'PUT',
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
        'If-Match': `"${pool.policy_version}"`,
      },
      body: JSON.stringify(resourcePoolBody(pool, desired)),
    },
  )
  const payload = await parseJSON(response)
  assert(response.status === 200, `resource pool concurrency update returned ${response.status}`)
  poolCurrent = dataOf(payload)
  assert(
    Number(poolCurrent.max_inflight) >= minimumConcurrency && poolCurrent.chat_max_concurrent == null,
    `model concurrency is at least ${minimumConcurrency}`,
  )
}

function parseSSEChunk(state, chunk) {
  state.buffer += chunk
  while (true) {
    const newline = state.buffer.indexOf('\n')
    if (newline < 0) return
    const line = state.buffer.slice(0, newline).replace(/\r$/, '')
    state.buffer = state.buffer.slice(newline + 1)
    if (!line.startsWith('data:')) continue
    const raw = line.slice(5).trim()
    if (!raw || raw === '[DONE]') continue
    state.events.push(JSON.parse(raw))
  }
}

async function runConversation(
  sessionId,
  agent,
  modelId,
  index,
  scenario = 'complex',
) {
  const smoke = scenario === 'smoke'
  const label = smoke
    ? `E2E-FINAL-SMOKE-${runId}-${index}`
    : `E2E-FINAL-DELIVERY-${runId}-${index}`
  const query = smoke
    ? [
        '除 final_answer 外不要调用任何工具。请用三段中文回答“事件驱动架构的定义、一个核心优势、一个主要风险”，每段必须是完整句子。',
        '最后一行原样写出校验串：',
        label,
      ].join('\n')
    : [
        '除 final_answer 外不要调用任何工具。请写一份完整、可直接提交管理层的决策备忘录，主题是“零售公司从单体系统迁移到事件驱动架构”。',
        '必须依次包含：执行摘要；当前约束；三种方案的对比表（成本、风险、交付周期）；八步实施计划；至少五项风险登记表；六项量化验收指标；最终建议。',
        '正文保持信息密度，避免只给提纲，并在最后一行原样写出校验串：',
        label,
      ].join('\n')
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), 12 * 60 * 1000)
  const startedAt = Date.now()
  try {
    const response = await fetch(
      `${baseUrl}/api/v1/agent-chat/${encodeURIComponent(sessionId)}`,
      {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${token}`,
          Accept: 'text/event-stream',
          'Content-Type': 'application/json',
          'X-Final-Delivery-E2E': label,
        },
        body: JSON.stringify({
          query,
          knowledge_base_ids: [],
          knowledge_ids: [],
          agent_enabled: true,
          agent_id: agent.id,
          web_search_enabled: false,
          summary_model_id: modelId,
          disable_title: true,
          enable_memory: false,
          channel: 'api',
          mcp_service_ids: [],
          skill_names: [],
          professional_skill_names: [],
        }),
        signal: controller.signal,
      },
    )
    assert(response.status === 200, `conversation ${index} returned HTTP ${response.status}`)

    const state = { buffer: '', events: [] }
    const decoder = new TextDecoder()
    const reader = response.body.getReader()
    while (true) {
      const { value, done } = await reader.read()
      if (done) break
      parseSSEChunk(state, decoder.decode(value, { stream: true }))
    }
    parseSSEChunk(state, `${decoder.decode()}\n`)

    const terminalError = state.events.find(
      (event) => event.response_type === 'error' && event.done !== false,
    )
    assert(!terminalError, `conversation ${index} had no terminal error: ${terminalError?.content || ''}`)

    const markerIndex = state.events.findIndex(
      (event) =>
        event.response_type === 'agent_progress' &&
        event.data?.answer_contract === 'explicit-final-v1',
    )
    assert(markerIndex >= 0, `conversation ${index} emitted explicit-final-v1 marker`)

    const finalAnswerToolCallIds = new Set(
      state.events
        .filter(
          (event) =>
            String(event.data?.tool_name || '') ===
            'mcp__weknora__final_answer',
        )
        .map((event) =>
          String(event.data?.tool_call_id || event.id || ''),
        )
        .filter(Boolean),
    )
    assert(
      finalAnswerToolCallIds.size === 1,
      `conversation ${index} called final_answer exactly once`,
    )

    const answerEvents = state.events.filter(
      (event) => event.response_type === 'answer',
    )
    assert(answerEvents.length > 1, `conversation ${index} streamed answer chunks plus done`)
    const answerIds = new Set(
      answerEvents
        .map((event) => String(event.data?.event_id || event.id || ''))
        .filter(Boolean),
    )
    assert(answerIds.size === 1, `conversation ${index} used one stable answer id`)
    const answer = answerEvents.map((event) => String(event.content || '')).join('')
    assert(
      answer.length >= (smoke ? 60 : 500),
      `conversation ${index} returned a substantial final answer`,
    )
    const requiredSections = smoke
      ? ['事件驱动', '优势', '风险', label]
      : [
          '执行摘要',
          '当前约束',
          '实施计划',
          '风险',
          '验收指标',
          '最终建议',
          label,
        ]
    for (const section of requiredSections) {
      assert(answer.includes(section), `conversation ${index} contains ${section}`)
    }

    const firstAnswerIndex = state.events.findIndex(
      (event) => event.response_type === 'answer',
    )
    const lastOperationalIndex = state.events.reduce((latest, event, eventIndex) => {
      if (
        event.response_type === 'tool_call' ||
        event.response_type === 'tool_result' ||
        event.response_type === 'thinking' ||
        event.response_type === 'agent_progress'
      ) {
        return eventIndex
      }
      return latest
    }, -1)
    assert(
      firstAnswerIndex > lastOperationalIndex,
      `conversation ${index} forwarded the final answer only after process/tool events`,
    )

    const complete = state.events.find(
      (event) => event.response_type === 'complete',
    )
    assert(complete, `conversation ${index} emitted complete`)
    assert(
      String(complete.data?.final_answer || '') === answer,
      `conversation ${index} complete.final_answer exactly matches streamed answer`,
    )

    const messages = await request(
      `/api/v1/messages/${encodeURIComponent(sessionId)}/load?limit=20`,
    )
    const assistant = Array.isArray(messages)
      ? messages.find(
          (message) =>
            message.role === 'assistant' &&
            (
              message.id === complete.data?.message_id ||
              String(message.content || '').includes(label)
            ),
        )
      : null
    assert(assistant, `conversation ${index} persisted an assistant message`)
    assert(
      String(assistant.content || '') === answer,
      `conversation ${index} persisted answer exactly matches verified stream`,
    )

    return {
      index,
      sessionId,
      answerLength: answer.length,
      eventCount: state.events.length,
      elapsedMs: Date.now() - startedAt,
      marker: label,
      scenario,
    }
  } finally {
    clearTimeout(timeout)
  }
}

async function cleanup() {
  for (const sessionId of createdSessions) {
    try {
      await request(`/api/v1/sessions/${encodeURIComponent(sessionId)}`, {
        method: 'DELETE',
        expected: [200, 202, 404],
      })
    } catch (error) {
      console.warn(`[WARN] failed to delete ${sessionId}: ${error.message}`)
    }
  }
  if (createdAgentId) {
    try {
      await request(`/api/v1/agents/${encodeURIComponent(createdAgentId)}`, {
        method: 'DELETE',
        expected: [200, 202, 204, 404],
      })
    } catch (error) {
      console.warn(`[WARN] failed to delete agent ${createdAgentId}: ${error.message}`)
    }
  }
  if (poolBackup?.id && poolCurrent?.policy_version) {
    try {
      const response = await fetch(
        `${baseUrl}/api/v1/custom/capacity-control/resource-pools/${encodeURIComponent(poolBackup.id)}`,
        {
          method: 'PUT',
          headers: {
            Authorization: `Bearer ${token}`,
            'Content-Type': 'application/json',
            'If-Match': `"${poolCurrent.policy_version}"`,
          },
          body: JSON.stringify(
            resourcePoolBody(
              poolBackup,
              poolBackup.max_inflight,
            ),
          ),
        },
      )
      const payload = await parseJSON(response)
      if (response.status !== 200) {
        throw new Error(
          `resource pool restore returned ${response.status}: ${JSON.stringify(payload).slice(0, 800)}`,
        )
      }
    } catch (error) {
      console.warn(`[WARN] failed to restore resource pool: ${error.message}`)
    }
  }
}

async function main() {
  const models = await request('/api/v1/models')
  assert(Array.isArray(models), 'models endpoint returns an array')
  const preferredModelIds = [
    'prod-deepseek-v4-flash-int8-chat',
    'prod-qwen36-35b-chat',
    'prod-qwen36-27b-tool-chat',
  ]
  const model = preferredModelIds
    .map((modelId) =>
      models.find(
        (candidate) =>
          candidate.id === modelId &&
          candidate.type === 'KnowledgeQA' &&
          candidate.status === 'active' &&
          candidate.workload_scope !== 'derivative_only',
      ),
    )
    .find(Boolean)
  assert(model?.id, 'an active llmgateway interactive chat model is available')

  await setModelConcurrency(String(model.id), Math.max(3, concurrency))
  const agent = await createIsolatedAgent(String(model.id))

  const smokeSession = await createSession('smoke')
  const smokeResult = await runConversation(
    smokeSession,
    agent,
    String(model.id),
    1,
    'smoke',
  )
  console.log(
    `[PASS] ordinary explicit-final-v1 smoke: answerLength=${smokeResult.answerLength}, elapsedMs=${smokeResult.elapsedMs}`,
  )

  const sessions = await Promise.all(
    Array.from({ length: concurrency }, (_, index) => createSession(index + 1)),
  )
  const results = await Promise.all(
    sessions.map((sessionId, index) =>
      runConversation(sessionId, agent, String(model.id), index + 1),
    ),
  )
  console.log(
    JSON.stringify(
      {
        passed: true,
        contract: 'explicit-final-v1',
        concurrency,
        agentId: agent.id,
        modelId: model.id,
        smoke: smokeResult,
        results,
      },
      null,
      2,
    ),
  )
}

try {
  await main()
} finally {
  await cleanup()
}
