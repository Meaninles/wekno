const baseUrl = String(process.env.WEKNORA_LIGHTWEIGHT_E2E_BASE_URL || 'http://localhost:8080').replace(/\/+$/, '')
const token = String(process.env.WEKNORA_LIGHTWEIGHT_E2E_TOKEN || '')
const agentId = String(process.env.WEKNORA_LIGHTWEIGHT_E2E_AGENT_ID || 'builtin-general-agent')

if (!token) throw new Error('WEKNORA_LIGHTWEIGHT_E2E_TOKEN is required')

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

async function request(path, { method = 'GET', body, expected = [200] } = {}) {
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
    throw new Error(`${method} ${path} returned ${response.status}: ${JSON.stringify(payload).slice(0, 1200)}`)
  }
  return dataOf(payload)
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

const agents = await request('/api/v1/agents')
const agent = (Array.isArray(agents) ? agents : []).find((item) => item.id === agentId)
assert(agent, `agent ${agentId} is accessible`)
assert(agent.config?.lightweight_skills_selection_mode === 'selected', 'agent lightweight mode is selected')
assert(
  Array.isArray(agent.config?.selected_lightweight_skills) &&
    agent.config.selected_lightweight_skills.includes('制度助手'),
  'agent binds 制度助手 as a configured lightweight skill',
)

const timestamp = new Date().toISOString().replace(/\D/g, '').slice(0, 14)
const session = await request('/api/v1/sessions', {
  method: 'POST',
  body: {
    title: `Configured lightweight Skill API ${timestamp}`,
    description: 'Local configured-lightweight-skill acceptance; intentionally kept for inspection.',
  },
  expected: [200, 201],
})
assert(session?.id, 'new session was created')

const response = await fetch(`${baseUrl}/api/v1/agent-chat/${encodeURIComponent(session.id)}`, {
  method: 'POST',
  headers: {
    Authorization: `Bearer ${token}`,
    Accept: 'text/event-stream',
    'Content-Type': 'application/json',
    'X-Lightweight-Skill-E2E': 'configured-only',
  },
  body: JSON.stringify({
    query: '你是谁？请用一句话说明你的名称和服务范围，不要检索资料。',
    knowledge_base_ids: [],
    knowledge_ids: [],
    agent_enabled: true,
    agent_id: agentId,
    web_search_enabled: false,
    disable_title: true,
    enable_memory: false,
    channel: 'api',
    mcp_service_ids: [],
    skill_names: [],
    professional_skill_names: [],
  }),
})
assert(response.status === 200, `agent chat returned HTTP ${response.status}`)

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
assert(!terminalError, `conversation has no terminal error: ${terminalError?.content || ''}`)

const answer = state.events
  .filter((event) => event.response_type === 'answer')
  .map((event) => String(event.content || ''))
  .join('')
if (!answer.includes('茅小规') || !answer.includes('制度')) {
  process.stderr.write(
    `${JSON.stringify({ session_id: session.id, answer, events: state.events }, null, 2)}\n`,
  )
}
assert(answer.includes('茅小规'), 'answer follows the configured 制度助手 identity')
assert(answer.includes('制度'), 'answer follows the configured 制度助手 service scope')

const complete = state.events.find((event) => event.response_type === 'complete')
assert(complete, 'conversation emitted complete')
assert(String(complete.data?.final_answer || '') === answer, 'complete.final_answer matches streamed answer')

const messages = await request(`/api/v1/messages/${encodeURIComponent(session.id)}/load?limit=20`)
const assistant = (Array.isArray(messages) ? messages : []).find(
  (message) => message.role === 'assistant' && String(message.content || '') === answer,
)
assert(assistant, 'verified answer was persisted')

process.stdout.write(
  JSON.stringify(
    {
      session_id: session.id,
      agent_id: agentId,
      request_skill_names: [],
      configured_lightweight_skills: agent.config.selected_lightweight_skills,
      answer,
      event_types: [...new Set(state.events.map((event) => event.response_type))],
    },
    null,
    2,
  ),
)
