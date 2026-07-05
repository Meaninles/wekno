import { computed, reactive } from 'vue'

export type UploadInfoItemStatus = 'queued' | 'uploading' | 'processing' | 'success' | 'failed'
export type UploadInfoItemSource = 'file' | 'url'

export interface UploadInfoItem {
  id: string
  source: UploadInfoItemSource
  name: string
  size?: number
  loaded: number
  total?: number
  percent: number
  status: UploadInfoItemStatus
  order: number
  startedAt?: number
  finishedAt?: number
  message?: string
  error?: string
}

export interface UploadInfoBatch {
  id: string
  kbId: string
  createdAt: number
  startedAt?: number
  finishedAt?: number
  items: UploadInfoItem[]
}

export interface UploadInfoBatchHandle {
  id: string
  fileItemIds: string[]
  urlItemIds: string[]
}

interface UploadInfoState {
  visible: boolean
  activeBatchId: string
  batches: UploadInfoBatch[]
}

const MAX_BATCHES = 8
let nextBatchSeq = 0

const state = reactive<UploadInfoState>({
  visible: false,
  activeBatchId: '',
  batches: [],
})

function getUploadDisplayName(file: File) {
  const relativePath = (file as File & { webkitRelativePath?: string }).webkitRelativePath
  return relativePath || file.name
}

function findBatch(batchId: string) {
  return state.batches.find((batch) => batch.id === batchId)
}

function findItem(itemId: string) {
  for (const batch of state.batches) {
    const item = batch.items.find((entry) => entry.id === itemId)
    if (item) return { batch, item }
  }
  return null
}

function isItemTerminal(status: UploadInfoItemStatus) {
  return status === 'success' || status === 'failed'
}

function syncBatchFinishedAt(batch: UploadInfoBatch) {
  if (batch.items.length === 0) return
  if (batch.items.every((item) => isItemTerminal(item.status))) {
    batch.finishedAt = Math.max(...batch.items.map((item) => item.finishedAt || Date.now()))
  } else {
    batch.finishedAt = undefined
  }
}

function trimBatches() {
  if (state.batches.length <= MAX_BATCHES) return
  const keep = state.batches.slice(0, MAX_BATCHES)
  const active = state.batches.find((batch) => batch.id === state.activeBatchId)
  state.batches = active && !keep.some((batch) => batch.id === active.id)
    ? [...keep.slice(0, MAX_BATCHES - 1), active]
    : keep
}

const activeBatch = computed(() =>
  state.batches.find((batch) => batch.id === state.activeBatchId) || state.batches[0] || null,
)

const activeUploadCount = computed(() =>
  state.batches.reduce((count, batch) => count + batch.items.filter((item) => !isItemTerminal(item.status)).length, 0),
)

const completedItemCount = computed(() =>
  state.batches.reduce((count, batch) => count + batch.items.filter((item) => item.status === 'success').length, 0),
)

const totalItemCount = computed(() =>
  state.batches.reduce((count, batch) => count + batch.items.length, 0),
)

const latestSummary = computed(() => {
  const batch = activeBatch.value
  const items = batch?.items || []
  const total = items.length
  const success = items.filter((item) => item.status === 'success').length
  const failed = items.filter((item) => item.status === 'failed').length
  const running = items.filter((item) => item.status === 'uploading' || item.status === 'processing').length
  const queued = items.filter((item) => item.status === 'queued').length
  const percent = total > 0
    ? Math.round(items.reduce((sum, item) => sum + item.percent, 0) / total)
    : 0
  return { total, success, failed, running, queued, percent }
})

function createBatch(params: { kbId: string; files?: File[]; urls?: string[] }): UploadInfoBatchHandle {
  const now = Date.now()
  const id = `upload-${now}-${++nextBatchSeq}`
  const files = params.files || []
  const urls = params.urls || []
  const fileItemIds: string[] = []
  const urlItemIds: string[] = []
  const items: UploadInfoItem[] = []

  files.forEach((file, index) => {
    const itemId = `${id}-file-${index}`
    fileItemIds.push(itemId)
    items.push({
      id: itemId,
      source: 'file',
      name: getUploadDisplayName(file),
      size: file.size,
      loaded: 0,
      total: file.size || undefined,
      percent: 0,
      status: 'queued',
      order: index,
    })
  })

  urls.forEach((url, index) => {
    const itemId = `${id}-url-${index}`
    urlItemIds.push(itemId)
    items.push({
      id: itemId,
      source: 'url',
      name: url,
      loaded: 0,
      percent: 0,
      status: 'queued',
      order: files.length + index,
    })
  })

  state.batches.unshift({
    id,
    kbId: params.kbId,
    createdAt: now,
    items,
  })
  state.activeBatchId = id
  trimBatches()

  return { id, fileItemIds, urlItemIds }
}

function openPanel(batchId?: string) {
  if (batchId && findBatch(batchId)) {
    state.activeBatchId = batchId
  } else if (!state.activeBatchId && state.batches[0]) {
    state.activeBatchId = state.batches[0].id
  }
  state.visible = true
}

function closePanel() {
  state.visible = false
}

function selectBatch(batchId: string) {
  if (findBatch(batchId)) state.activeBatchId = batchId
}

function startItem(itemId: string) {
  const target = findItem(itemId)
  if (!target) return
  const now = Date.now()
  target.batch.startedAt ||= now
  target.item.startedAt ||= now
  target.item.status = target.item.source === 'url' ? 'processing' : 'uploading'
  target.item.error = ''
  target.item.message = ''
  syncBatchFinishedAt(target.batch)
}

function updateItemProgress(itemId: string, loaded?: number, total?: number) {
  const target = findItem(itemId)
  if (!target) return
  const item = target.item
  const nextLoaded = Math.max(0, Number(loaded || 0))
  const nextTotal = Number(total || item.total || item.size || 0)
  item.loaded = nextLoaded
  if (nextTotal > 0) item.total = nextTotal
  if (item.status === 'queued') item.status = 'uploading'
  if (item.total && item.total > 0) {
    const rawPercent = Math.round((item.loaded / item.total) * 100)
    item.percent = Math.max(item.percent, Math.min(rawPercent, 100))
    if (item.percent >= 100 && item.status === 'uploading') {
      item.status = 'processing'
    }
  } else if (item.status === 'uploading') {
    item.percent = Math.max(item.percent, 5)
  }
}

function markItemProcessing(itemId: string, message?: string) {
  const target = findItem(itemId)
  if (!target) return
  target.item.status = 'processing'
  target.item.percent = Math.max(target.item.percent, target.item.source === 'file' ? 100 : 50)
  target.item.message = message || ''
}

function markItemSuccess(itemId: string, message?: string) {
  const target = findItem(itemId)
  if (!target) return
  const item = target.item
  item.status = 'success'
  item.percent = 100
  item.loaded = item.total || item.size || item.loaded
  item.message = message || ''
  item.error = ''
  item.finishedAt = Date.now()
  syncBatchFinishedAt(target.batch)
}

function markItemFailed(itemId: string, error: string) {
  const target = findItem(itemId)
  if (!target) return
  const item = target.item
  item.status = 'failed'
  item.error = error
  item.finishedAt = Date.now()
  if (item.percent === 0) item.percent = item.source === 'url' ? 100 : Math.min(100, item.percent)
  syncBatchFinishedAt(target.batch)
}

function clearCompletedItems() {
  for (let index = state.batches.length - 1; index >= 0; index -= 1) {
    const batch = state.batches[index]
    batch.items = batch.items.filter((item) => item.status !== 'success')
    batch.finishedAt = undefined
    if (batch.items.length === 0) {
      state.batches.splice(index, 1)
    } else {
      syncBatchFinishedAt(batch)
    }
  }
  if (!state.batches.some((batch) => batch.id === state.activeBatchId)) {
    state.activeBatchId = state.batches[0]?.id || ''
  }
}

function clearAllBatches() {
  state.batches = []
  state.activeBatchId = ''
}

export function useUploadInfoStore() {
  return {
    state,
    activeBatch,
    activeUploadCount,
    completedItemCount,
    totalItemCount,
    latestSummary,
    createBatch,
    openPanel,
    closePanel,
    selectBatch,
    startItem,
    updateItemProgress,
    markItemProcessing,
    markItemSuccess,
    markItemFailed,
    clearCompletedItems,
    clearAllBatches,
  }
}
