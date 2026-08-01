import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'

const baseUrl = String(
  process.env.WEKNORA_QUEUE_E2E_BASE_URL || 'http://localhost:8080',
).replace(/\/+$/, '')
const token = String(process.env.WEKNORA_QUEUE_E2E_TOKEN || '')
const settingKeys = [
  'chat.queue.enabled',
  'chat.queue.default_max_waiting',
  'chat.queue.max_waiting_per_user',
]
const apiReplicas = [
  'weknora-runtime-api-1',
  'weknora-runtime-api-2',
  'weknora-runtime-api-3',
]

if (!token) throw new Error('WEKNORA_QUEUE_E2E_TOKEN is required')

const runId = new Date().toISOString().replace(/\D/g, '').slice(0, 14)
const createdSessions = new Set()
const stoppedContainers = new Set()
const poolBackups = new Map()
const poolCurrents = new Map()
const settingBackups = new Map()
const passed = []
let cachedRedisPassword = ''

function assert(condition, message) {
  if (!condition) throw new Error(`Assertion failed: ${message}`)
}

function pass(name) {
  passed.push(name)
  console.log(`[PASS] ${name}`)
}

function delay(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds))
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

async function request(
  path,
  {
    method = 'GET',
    body,
    headers = {},
    expected = [200],
    anonymous = false,
  } = {},
) {
  const response = await fetch(`${baseUrl}${path}`, {
    method,
    headers: {
      ...(anonymous ? {} : { Authorization: `Bearer ${token}` }),
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      ...headers,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const payload = await parseJSON(response)
  if (!expected.includes(response.status)) {
    throw new Error(
      `${method} ${path} returned ${response.status}, expected ${expected.join(',')}: ` +
        JSON.stringify(payload).slice(0, 1200),
    )
  }
  return { status: response.status, payload }
}

function dataOf(payload) {
  return payload && Object.prototype.hasOwnProperty.call(payload, 'data')
    ? payload.data
    : payload
}

function docker(args, options = {}) {
  return execFileSync('docker', args, {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    ...options,
  }).trim()
}

function redisPassword() {
  if (cachedRedisPassword) return cachedRedisPassword
  const envPath = new URL('../../../.env', import.meta.url)
  const content = readFileSync(envPath, 'utf8')
  const line = content
    .split(/\r?\n/)
    .find((candidate) => candidate.startsWith('REDIS_PASSWORD='))
  assert(line, 'local Redis password is configured')
  let value = line.slice('REDIS_PASSWORD='.length).trim()
  if (
    value.length >= 2 &&
    ((value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'")))
  ) {
    value = value.slice(1, -1)
  }
  assert(value, 'local Redis password is non-empty')
  cachedRedisPassword = value
  return cachedRedisPassword
}

function queueStats() {
  const script = `
active=0
waiting=0
users=0
meta=0
keys=0
for key in $(redis-cli --raw --scan --pattern 'weknora:{chat-queue}:*'); do
  keys=$((keys + 1))
  case "$key" in
    *:pool:*:active)
      value=$(redis-cli --raw ZCARD "$key")
      active=$((active + value))
      ;;
    *:pool:*:waiting)
      value=$(redis-cli --raw ZCARD "$key")
      waiting=$((waiting + value))
      ;;
    *:user:*:waiting)
      value=$(redis-cli --raw ZCARD "$key")
      users=$((users + value))
      ;;
    *:wait-meta)
      value=$(redis-cli --raw HLEN "$key")
      meta=$((meta + value))
      ;;
  esac
done
printf '%s|%s|%s|%s|%s' "$active" "$waiting" "$users" "$meta" "$keys"
`
  const output = docker([
    'exec',
    '-e',
    `REDISCLI_AUTH=${redisPassword()}`,
    'WeKnora-redis-dev',
    'sh',
    '-lc',
    script,
  ])
  const parts = output.split('|').map((value) => Number(value))
  assert(
    parts.length === 5 && parts.every(Number.isFinite),
    `Redis queue sampler returned ${output}`,
  )
  return {
    active: parts[0],
    waiting: parts[1],
    users: parts[2],
    meta: parts[3],
    keys: parts[4],
  }
}

async function waitQueueDrained(timeoutMilliseconds = 15000) {
  const deadline = Date.now() + timeoutMilliseconds
  let latest
  do {
    latest = queueStats()
    if (
      latest.active === 0 &&
      latest.waiting === 0 &&
      latest.users === 0 &&
      latest.meta === 0
    ) {
      return latest
    }
    await delay(300)
  } while (Date.now() < deadline)
  throw new Error(`chat queue did not drain: ${JSON.stringify(latest)}`)
}

async function waitForHTTP(path, expectedStatus, timeoutMilliseconds = 90000) {
  const deadline = Date.now() + timeoutMilliseconds
  let latest = 0
  do {
    try {
      const response = await fetch(`${baseUrl}${path}`)
      latest = response.status
      await response.body?.cancel()
      if (latest === expectedStatus) return
    } catch {
      latest = 0
    }
    await delay(500)
  } while (Date.now() < deadline)
  throw new Error(
    `${path} did not reach HTTP ${expectedStatus}; latest=${latest}`,
  )
}

async function waitContainerReady(name, timeoutMilliseconds = 90000) {
  const deadline = Date.now() + timeoutMilliseconds
  let latest = ''
  do {
    try {
      latest = docker([
        'inspect',
        name,
        '--format',
        '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}',
      ])
      if (latest === 'healthy' || latest === 'running') return
    } catch {
      latest = 'missing'
    }
    await delay(500)
  } while (Date.now() < deadline)
  throw new Error(`${name} did not become ready; latest=${latest}`)
}

async function stopContainer(name) {
  docker(['stop', '--time', '30', name])
  stoppedContainers.add(name)
}

async function startContainer(name) {
  docker(['start', name])
  await waitContainerReady(name)
  stoppedContainers.delete(name)
}

async function backupAndResetSettings() {
  const listed = dataOf(
    (await request('/api/v1/system/admin/settings')).payload,
  )
  assert(Array.isArray(listed), 'system settings API returns an array')
  for (const key of settingKeys) {
    const row = listed.find((candidate) => candidate.key === key)
    assert(row, `system setting ${key} is registered`)
    settingBackups.set(key, {
      persisted: Number(row.id || 0) > 0,
      value: row.value,
    })
    await request(
      `/api/v1/system/admin/settings/${encodeURIComponent(key)}`,
      { method: 'DELETE', expected: [200, 204] },
    )
  }
  await delay(1200)
}

async function setSetting(key, value) {
  const result = await request(
    `/api/v1/system/admin/settings/${encodeURIComponent(key)}`,
    { method: 'PUT', body: { value } },
  )
  assert(result.payload?.key === key, `updated system setting ${key}`)
}

async function restoreSettings() {
  for (const [key, backup] of settingBackups) {
    try {
      if (backup.persisted) {
        await request(
          `/api/v1/system/admin/settings/${encodeURIComponent(key)}`,
          { method: 'PUT', body: { value: backup.value } },
        )
      } else {
        await request(
          `/api/v1/system/admin/settings/${encodeURIComponent(key)}`,
          { method: 'DELETE', expected: [200, 204] },
        )
      }
    } catch (error) {
      console.warn(`[WARN] failed to restore ${key}: ${error.message}`)
    }
  }
}

function resourcePoolBody(pool, maxInflight, chatMaxWaiting) {
  const total = maxInflight == null ? Number(pool.max_inflight) : Number(maxInflight)
  const reserve = Math.min(Number(pool.interactive_reserve), Math.max(0, total - 1))
  const background = Math.max(1, total - reserve)
  const tenant = Math.min(Math.max(1, Number(pool.tenant_burst)), total)
  return {
    id: String(pool.id),
    name: String(pool.name),
    resource_kind: String(pool.resource_kind),
    chat_max_concurrent: null,
    chat_max_waiting: chatMaxWaiting,
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

async function setPoolLimits(poolId, concurrent, waiting) {
  const current = poolCurrents.get(poolId)
  assert(current, `resource pool ${poolId} is tracked`)
  const response = await request(
    `/api/v1/custom/capacity-control/resource-pools/${encodeURIComponent(poolId)}`,
    {
      method: 'PUT',
      body: resourcePoolBody(current, concurrent, waiting),
      headers: { 'If-Match': `"${current.policy_version}"` },
    },
  )
  const updated = dataOf(response.payload)
  poolCurrents.set(poolId, updated)
  await delay(1300)
  return updated
}

async function restorePools() {
  for (const [poolId, backup] of poolBackups) {
    try {
      const current = poolCurrents.get(poolId)
      if (!current) continue
      const response = await request(
        `/api/v1/custom/capacity-control/resource-pools/${encodeURIComponent(poolId)}`,
        {
          method: 'PUT',
          body: resourcePoolBody(
            backup,
            backup.max_inflight,
            backup.chat_max_waiting ?? null,
          ),
          headers: { 'If-Match': `"${current.policy_version}"` },
        },
      )
      poolCurrents.set(poolId, dataOf(response.payload))
    } catch (error) {
      console.warn(`[WARN] failed to restore pool ${poolId}: ${error.message}`)
    }
  }
}

async function createSession(label) {
  const response = await request('/api/v1/sessions', {
    method: 'POST',
    body: {
      title: `Queue API ${runId} ${label}`,
      description: 'Local chat queue acceptance; safe to delete.',
    },
    expected: [200, 201],
  })
  const session = dataOf(response.payload)
  assert(session?.id, `session ${label} was created`)
  createdSessions.add(String(session.id))
  return String(session.id)
}

async function deleteCreatedSessions() {
  for (const sessionId of [...createdSessions]) {
    try {
      await request(`/api/v1/sessions/${encodeURIComponent(sessionId)}`, {
        method: 'DELETE',
        expected: [200, 202, 404],
      })
      createdSessions.delete(sessionId)
    } catch (error) {
      console.warn(`[WARN] failed to delete session ${sessionId}: ${error.message}`)
    }
  }
}

function parseSSELine(handle, line) {
  if (!line.startsWith('data:')) return
  const raw = line.slice(5).trim()
  if (!raw || raw === '[DONE]') return
  try {
    const event = JSON.parse(raw)
    event.__observed_at = Date.now()
    handle.events.push(event)
    if (event.assistant_message_id) {
      handle.assistantMessageId = String(event.assistant_message_id)
    }
  } catch {
    handle.decodeErrors += 1
  }
}

async function pumpChat(handle) {
  const decoder = new TextDecoder()
  let buffer = ''
  try {
    while (true) {
      const { value, done } = await handle.reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      while (true) {
        const newline = buffer.indexOf('\n')
        if (newline < 0) break
        const line = buffer.slice(0, newline).replace(/\r$/, '')
        buffer = buffer.slice(newline + 1)
        parseSSELine(handle, line)
      }
    }
    if (buffer) parseSSELine(handle, buffer.replace(/\r$/, ''))
  } catch (error) {
    if (error?.name !== 'AbortError' && !handle.closed) {
      handle.pumpError = error
    }
  }
}

async function startChat(sessionId, modelId, label) {
  const controller = new AbortController()
  const response = await fetch(
    `${baseUrl}/api/v1/knowledge-chat/${encodeURIComponent(sessionId)}`,
    {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: 'text/event-stream',
        'Content-Type': 'application/json',
        'X-Queue-E2E-Case': label,
      },
      body: JSON.stringify({
        query:
          `本地并发验收 ${runId}/${label}。` +
          '请按从1到400的顺序逐项输出数字和一句极短说明，未完成前不要提前结束。',
        knowledge_base_ids: [],
        knowledge_ids: [],
        agent_enabled: false,
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

  if (response.status !== 200) {
    const payload = await parseJSON(response)
    return {
      sessionId,
      modelId,
      status: response.status,
      rejection: payload?.data || payload,
      events: [],
      closed: true,
      controller,
    }
  }

  const handle = {
    sessionId,
    modelId,
    status: response.status,
    rejection: null,
    events: [],
    assistantMessageId: '',
    decodeErrors: 0,
    pumpError: null,
    closed: false,
    controller,
    reader: response.body.getReader(),
    pump: null,
  }
  handle.pump = pumpChat(handle)
  return handle
}

async function probeRequests(specifications, label, observeMilliseconds = 1800) {
  const sessions = await Promise.all(
    specifications.map((_, index) => createSession(`${label}-${index + 1}`)),
  )
  const handles = await Promise.all(
    specifications.map((specification, index) =>
      startChat(
        sessions[index],
        specification.modelId,
        `${label}-${index + 1}`,
      ),
    ),
  )
  await delay(observeMilliseconds)
  return handles
}

function queueEvents(handle) {
  return handle.events.filter(
    (event) => event.response_type === 'queue_status',
  )
}

function summarize(handles) {
  const accepted = handles.filter((handle) => handle.status === 200)
  const rejected = handles.filter((handle) => handle.status !== 200)
  const waiting = accepted.filter((handle) =>
    queueEvents(handle).some((event) => event.data?.state === 'waiting'),
  )
  const admittedAfterWait = accepted.filter((handle) =>
    queueEvents(handle).some((event) => event.data?.state === 'admitted'),
  )
  const codes = new Map()
  for (const handle of rejected) {
    const code = String(handle.rejection?.code || '')
    codes.set(code, (codes.get(code) || 0) + 1)
  }
  return { accepted, rejected, waiting, admittedAfterWait, codes }
}

async function waitForAssistantIDs(handles, timeoutMilliseconds = 3000) {
  const accepted = handles.filter((handle) => handle.status === 200)
  const deadline = Date.now() + timeoutMilliseconds
  while (
    accepted.some((handle) => !handle.assistantMessageId) &&
    Date.now() < deadline
  ) {
    await delay(50)
  }
}

async function stopHandle(handle) {
  if (handle.status !== 200 || !handle.assistantMessageId) return
  await request(
    `/api/v1/sessions/${encodeURIComponent(handle.sessionId)}/stop`,
    {
      method: 'POST',
      body: { message_id: handle.assistantMessageId },
      expected: [200, 404],
    },
  )
}

async function closeHandle(handle) {
  if (handle.status !== 200 || handle.closed) return
  handle.closed = true
  handle.controller.abort()
  try {
    await handle.reader.cancel()
  } catch {
    // The abort may have already closed the reader.
  }
  await Promise.race([handle.pump, delay(1000)])
}

async function cleanupHandles(handles, waitForDrain = true) {
  await waitForAssistantIDs(handles)
  await Promise.all(handles.map((handle) => stopHandle(handle)))
  await delay(600)
  await Promise.all(handles.map((handle) => closeHandle(handle)))
  if (waitForDrain) await waitQueueDrained()
}

async function assertRejectedSessionsHaveNoMessages(handles, code) {
  const rejected = handles.filter(
    (handle) => handle.rejection?.code === code,
  )
  assert(rejected.length > 0, `${code} was observed`)
  for (const handle of rejected.slice(0, 4)) {
    const response = await request(
      `/api/v1/messages/${encodeURIComponent(handle.sessionId)}/load?limit=20`,
    )
    const messages = dataOf(response.payload)
    assert(
      Array.isArray(messages) && messages.length === 0,
      `${code} rejection created no ghost messages`,
    )
  }
}

function assertQueueBounds(handles, concurrent, waiting) {
  const observations = handles.flatMap((handle) => [
    ...queueEvents(handle).map((event) => event.data || {}),
    ...(handle.rejection ? [handle.rejection] : []),
  ])
  assert(observations.length > 0, 'queue limit observations were emitted')
  for (const observation of observations) {
    if (Number.isFinite(Number(observation.max_concurrent))) {
      assert(
        Number(observation.max_concurrent) === concurrent,
        `max_concurrent=${concurrent} is visible in queue state`,
      )
    }
    if (Number.isFinite(Number(observation.max_waiting))) {
      assert(
        Number(observation.max_waiting) === waiting,
        `max_waiting=${waiting} is visible in queue state`,
      )
    }
    if (Number.isFinite(Number(observation.active))) {
      assert(
        Number(observation.active) <= concurrent,
        `active conversations never exceed ${concurrent}`,
      )
    }
    if (Number.isFinite(Number(observation.waiting))) {
      assert(
        Number(observation.waiting) <= waiting,
        `waiting conversations never exceed ${waiting}`,
      )
    }
  }
}

function requestsFor(modelId, count) {
  return Array.from({ length: count }, () => ({ modelId }))
}

async function run() {
  await waitForHTTP('/health', 200)
  const initialStats = queueStats()
  assert(
    initialStats.active === 0 &&
      initialStats.waiting === 0 &&
      initialStats.users === 0 &&
      initialStats.meta === 0,
    `queue starts empty: ${JSON.stringify(initialStats)}`,
  )

  await backupAndResetSettings()
  const defaults = dataOf(
    (await request('/api/v1/system/admin/settings')).payload,
  )
  const settingValue = (key) =>
    defaults.find((candidate) => candidate.key === key)?.value
  assert(settingValue('chat.queue.enabled') === true, 'queue defaults enabled')
  assert(
    Number(settingValue('chat.queue.default_max_waiting')) === 500,
    'default model-pool waiting capacity is 500',
  )
  assert(
    Number(settingValue('chat.queue.max_waiting_per_user')) === 3,
    'default single-user waiting capacity is 3',
  )

  await request(
    '/api/v1/system/admin/settings/chat.queue.default_max_waiting',
    { method: 'PUT', body: { value: -1 }, expected: [400] },
  )
  await request(
    '/api/v1/system/admin/settings/chat.queue.max_waiting_per_user',
    { method: 'PUT', body: { value: 0 }, expected: [400] },
  )
  pass('queue defaults, administrator visibility, and invalid-bound validation')

  const models = dataOf((await request('/api/v1/models')).payload)
  const bindings = dataOf(
    (
      await request(
        '/api/v1/custom/capacity-control/bindings',
      )
    ).payload,
  )
  const pools = dataOf(
    (
      await request(
        '/api/v1/custom/capacity-control/resource-pools',
      )
    ).payload,
  )
  assert(
    Array.isArray(models) && Array.isArray(bindings) && Array.isArray(pools),
    'models, bindings, and pools are listable',
  )
  const interactive = models.filter(
    (model) =>
      model.type === 'KnowledgeQA' &&
      model.status === 'active' &&
      model.workload_scope !== 'derivative_only',
  )
  const preferredIDs = [
    'prod-deepseek-v4-flash-int8-chat',
    'prod-qwen36-27b-tool-chat',
    'prod-qwen36-27b-chat',
    'prod-qwen36-35b-chat',
  ]
  interactive.sort((left, right) => {
    const leftIndex = preferredIDs.indexOf(left.id)
    const rightIndex = preferredIDs.indexOf(right.id)
    return (
      (leftIndex < 0 ? 999 : leftIndex) -
      (rightIndex < 0 ? 999 : rightIndex)
    )
  })
  const selections = []
  for (const model of interactive) {
    const binding = bindings.find(
      (candidate) =>
        candidate.model_id === model.id &&
        (model.tenant_id === undefined ||
          Number(candidate.model_tenant_id) === Number(model.tenant_id)),
    )
    if (!binding) continue
    const pool = pools.find(
      (candidate) => candidate.id === binding.resource_pool_id,
    )
    if (!pool || pool.resource_kind !== 'chat' || pool.state !== 'enabled') {
      continue
    }
    if (
      selections.some(
        (selection) => selection.pool.id === pool.id,
      )
    ) {
      continue
    }
    selections.push({ model, binding, pool })
    if (selections.length === 2) break
  }
  assert(selections.length >= 2, 'two interactive models use distinct pools')
  for (const selection of selections) {
    poolBackups.set(selection.pool.id, structuredClone(selection.pool))
    poolCurrents.set(selection.pool.id, structuredClone(selection.pool))
  }
  const primary = selections[0]
  const secondary = selections[1]
  console.log(
    `[INFO] queue models: ${primary.model.display_name || primary.model.name} / ` +
      `${secondary.model.display_name || secondary.model.name}`,
  )

  await setSetting('chat.queue.enabled', true)
  await setSetting('chat.queue.default_max_waiting', 2)
  await setSetting('chat.queue.max_waiting_per_user', 20)
  await setPoolLimits(primary.pool.id, 1, null)

  const inherited = await probeRequests(
    requestsFor(primary.model.id, 6),
    'global-inherited',
  )
  const inheritedSummary = summarize(inherited)
  assert(
    inheritedSummary.accepted.length === 3,
    'global 1 active + 2 waiting admits exactly three simultaneous requests',
  )
  assert(
    inheritedSummary.codes.get('CHAT_QUEUE_FULL') === 3,
    'global waiting ceiling rejects excess requests as system-full',
  )
  assert(inheritedSummary.waiting.length >= 1, 'accepted waiters emit queue_status')
  assertQueueBounds(inherited, 1, 2)
  await assertRejectedSessionsHaveNoMessages(inherited, 'CHAT_QUEUE_FULL')
  await cleanupHandles(inherited)
  pass('global inherited ceiling, blue waiting state, red full rejection, and no ghost turns')

  await setPoolLimits(primary.pool.id, 2, 5)
  const overridden = await probeRequests(
    requestsFor(primary.model.id, 10),
    'pool-override',
  )
  const overriddenSummary = summarize(overridden)
  assert(
    overriddenSummary.accepted.length === 7,
    'per-model override admits two active plus five waiting',
  )
  assert(
    overriddenSummary.codes.get('CHAT_QUEUE_FULL') === 3,
    'per-model override rejects only requests beyond its own waiting ceiling',
  )
  assertQueueBounds(overridden, 2, 5)
  await cleanupHandles(overridden)
  pass('per-model concurrent/waiting override takes precedence across API replicas')

  await setSetting('chat.queue.max_waiting_per_user', 2)
  await setPoolLimits(primary.pool.id, 1, 10)
  const personal = await probeRequests(
    requestsFor(primary.model.id, 5),
    'personal-limit',
  )
  const personalSummary = summarize(personal)
  assert(
    personalSummary.accepted.length === 3,
    'one active conversation plus two personal waiters are admitted',
  )
  assert(
    personalSummary.codes.get('CHAT_QUEUE_USER_LIMIT') === 2,
    'excess sessions are rejected by the single-user waiting limit',
  )
  assert(
    !personalSummary.codes.has('CHAT_QUEUE_FULL'),
    'personal-cap rejection is distinct from model-pool full',
  )
  await assertRejectedSessionsHaveNoMessages(
    personal,
    'CHAT_QUEUE_USER_LIMIT',
  )
  const queuedToCancel = personalSummary.waiting[0]
  assert(queuedToCancel, 'a personal queued conversation is cancellable')
  await waitForAssistantIDs([queuedToCancel])
  await stopHandle(queuedToCancel)
  await delay(800)
  await closeHandle(queuedToCancel)
  const replacement = await probeRequests(
    requestsFor(primary.model.id, 1),
    'personal-replacement',
    900,
  )
  assert(
    replacement[0].status === 200,
    'cancelling one waiter immediately frees the personal queue slot',
  )
  await cleanupHandles([...personal, ...replacement])
  pass('orange single-user cap, queued cancellation, replacement admission, and no ghost turns')

  await setSetting('chat.queue.max_waiting_per_user', 20)
  await setPoolLimits(primary.pool.id, 1, 8)
  const hot = await probeRequests(
    requestsFor(primary.model.id, 5),
    'hot-expand',
    900,
  )
  const hotBefore = summarize(hot)
  assert(hotBefore.waiting.length >= 2, 'hot-update run has multiple waiters')
  const changedAt = Date.now()
  await setPoolLimits(primary.pool.id, 3, 8)
  const hotDeadline = Date.now() + 6000
  let hotAdmissions = []
  do {
    hotAdmissions = hot.flatMap((handle) =>
      queueEvents(handle).filter(
        (event) =>
          event.data?.state === 'admitted' &&
          event.__observed_at >= changedAt &&
          Number(event.data?.max_concurrent) === 3 &&
          Number(event.data?.active) >= 2,
      ),
    )
    if (hotAdmissions.length >= 1) break
    await delay(100)
  } while (Date.now() < hotDeadline)
  assert(
    hotAdmissions.length >= 1,
    'waiters consume newly added slots without restarting any API replica',
  )
  await cleanupHandles(hot)
  pass('live maximum-concurrency expansion promotes queued conversations automatically')

  await setPoolLimits(primary.pool.id, 1, 2)
  await setPoolLimits(secondary.pool.id, 1, 2)
  const mixed = await probeRequests(
    [
      ...requestsFor(primary.model.id, 3),
      ...requestsFor(secondary.model.id, 3),
    ],
    'multi-model',
  )
  const mixedSummary = summarize(mixed)
  assert(
    mixedSummary.accepted.length === 6 &&
      mixedSummary.rejected.length === 0,
    'two model pools each admit their independent active and waiting capacity',
  )
  const observedPoolIDs = new Set(
    mixed.flatMap((handle) =>
      queueEvents(handle)
        .map((event) => event.data?.resource_pool_id)
        .filter(Boolean),
    ),
  )
  assert(
    observedPoolIDs.has(primary.pool.id) &&
      observedPoolIDs.has(secondary.pool.id),
    'SSE queue states identify both independent model pools',
  )
  await cleanupHandles(mixed)
  pass('two actual chat-model routes queue independently')

  await stopContainer(apiReplicas[0])
  for (let index = 0; index < 12; index += 1) {
    const health = await request('/health', { anonymous: true })
    assert(health.payload?.status === 'ok', 'API remains healthy with one replica down')
  }
  await setPoolLimits(primary.pool.id, 2, 3)
  const reducedAPI = await probeRequests(
    requestsFor(primary.model.id, 7),
    'api-replica-loss',
  )
  const reducedSummary = summarize(reducedAPI)
  assert(
    reducedSummary.accepted.length === 5 &&
      reducedSummary.codes.get('CHAT_QUEUE_FULL') === 2,
    'remaining API replicas share the same distributed chat ceiling',
  )
  await cleanupHandles(reducedAPI)
  await startContainer(apiReplicas[0])
  await waitForHTTP('/health', 200)
  pass('single API replica loss, distributed queue continuity, and healthy rejoin')

  const redisProbeSession = await createSession('redis-outage')
  await stopContainer('WeKnora-redis-dev')
  const healthWithoutRedis = await request('/health', { anonymous: true })
  assert(
    healthWithoutRedis.payload?.status === 'ok',
    'basic API health is isolated from a Redis queue outage',
  )
  const unavailable = await startChat(
    redisProbeSession,
    primary.model.id,
    'redis-outage',
  )
  assert(
    unavailable.status === 503 &&
      unavailable.rejection?.code === 'CHAT_QUEUE_UNAVAILABLE',
    'chat admission fails closed with a structured retryable 503',
  )
  await startContainer('WeKnora-redis-dev')
  await waitForHTTP('/health', 200)
  await delay(1500)
  const recovered = await probeRequests(
    requestsFor(primary.model.id, 1),
    'redis-recovered',
    700,
  )
  assert(recovered[0].status === 200, 'chat admission recovers after Redis returns')
  await cleanupHandles(recovered)
  pass('Redis outage error isolation, distinct unavailable response, and recovery')

  const logSince = new Date(Date.now() - 10 * 60 * 1000).toISOString()
  const handledBy = []
  for (const replica of apiReplicas) {
    const logs = docker(['logs', '--since', logSince, replica])
    if ([...createdSessions].some((sessionId) => logs.includes(sessionId))) {
      handledBy.push(replica)
    }
  }
  assert(
    handledBy.length >= 2,
    `queue test requests crossed multiple API replicas: ${handledBy.join(',')}`,
  )
  pass('load-balanced requests exercised shared queue state on multiple API instances')

  console.log('')
  console.log(
    `Chat queue API/concurrency/fault acceptance passed: ${passed.length} groups`,
  )
}

try {
  await run()
} finally {
  for (const name of [...stoppedContainers]) {
    try {
      docker(['start', name])
      await waitContainerReady(name)
      stoppedContainers.delete(name)
    } catch (error) {
      console.warn(`[WARN] failed to restore ${name}: ${error.message}`)
    }
  }
  try {
    await waitForHTTP('/health', 200)
  } catch (error) {
    console.warn(`[WARN] API was not ready during cleanup: ${error.message}`)
  }
  try {
    await restorePools()
  } finally {
    await restoreSettings()
  }
  try {
    await deleteCreatedSessions()
  } finally {
    try {
      await waitQueueDrained(20000)
    } catch (error) {
      console.warn(`[WARN] ${error.message}`)
    }
  }
}
