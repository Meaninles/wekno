// Draft metadata is small and frequently updated. Original File blobs are
// stored once, separately, without reading/encoding their bytes in JavaScript.
export type UploadBinding = { uploadId: string; sourceId?: string }
type StoredFile = { key: string; draft: string; id: string; file: File; bindings: Record<string, UploadBinding> }
type StoredDraft<T> = { key: string; value: T }
const fileIds = new WeakMap<File, string>()
const writes = new Map<string, Promise<void>>()
let database: Promise<IDBDatabase> | undefined

export function localDraftScope(): string {
  const user = JSON.parse(localStorage.getItem('weknora_user') || 'null')
  const tenant = localStorage.getItem('weknora_selected_tenant_id') || JSON.parse(localStorage.getItem('weknora_tenant') || 'null')?.id
  return user?.id && tenant ? JSON.stringify(['web', String(user.id), String(tenant)]) : ''
}

export function embedDraftScope(channel: string, visitor: string): string {
  return visitor ? JSON.stringify(['embed', channel, visitor]) : ''
}

export function draftKey(session: unknown, scope = localDraftScope()): string {
  return scope && session ? JSON.stringify([scope, String(session)]) : ''
}

export function fileId(file: File): string {
  let id = fileIds.get(file)
  if (!id) { id = crypto.randomUUID(); fileIds.set(file, id) }
  return id
}

function open(): Promise<IDBDatabase> {
  if (!database) database = new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open('weknora-conversation-drafts', 1)
    request.onupgradeneeded = () => {
      request.result.createObjectStore('drafts', { keyPath: 'key' })
      request.result.createObjectStore('files', { keyPath: 'key' }).createIndex('draft', 'draft')
    }
    request.onsuccess = () => {
      request.result.onversionchange = () => { request.result.close(); database = undefined }
      resolve(request.result)
    }
    request.onerror = () => reject(request.error)
    request.onblocked = () => reject(new Error('请关闭旧的对话页面后重试'))
  }).catch(error => { database = undefined; throw error })
  return database
}

function read<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => { request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error) })
}

function finished(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve()
    tx.onabort = () => reject(tx.error || new Error('无法保存附件草稿'))
    tx.onerror = () => reject(tx.error)
  })
}

// Serialize writes for a draft so a slow 128 MiB insert cannot resurrect an
// attachment removed by a later composer update. Rejections remain observable.
function serialize<T>(key: string, work: () => Promise<T>): Promise<T> {
  const result = (writes.get(key) || Promise.resolve()).catch(() => {}).then(work)
  const tail = result.then(() => {})
  writes.set(key, tail)
  void tail.catch(() => {})
  return result
}

export async function flushDraft(key: string): Promise<void> { await writes.get(key) }

export function writeDraft<T>(key: string, value: T | null, files: File[]): Promise<void> {
  return serialize(key, async () => {
    const db = await open()
    const tx = db.transaction(['drafts', 'files'], 'readwrite')
    const done = finished(tx)
    const originals = tx.objectStore('files')
    const wanted = new Set(files.map(fileId))
    const cursor = originals.index('draft').openCursor(IDBKeyRange.only(key))
    cursor.onsuccess = () => {
      const row = cursor.result
      if (!row) return
      if (!wanted.has((row.value as StoredFile).id)) row.delete()
      row.continue()
    }
    for (const file of files) {
      const id = fileId(file), fileKey = JSON.stringify([key, id])
      const existing = originals.getKey(fileKey)
      existing.onsuccess = () => {
        if (existing.result === undefined) originals.put({ key: fileKey, draft: key, id, file, bindings: {} } satisfies StoredFile)
      }
    }
    if (value === null) tx.objectStore('drafts').delete(key)
    else tx.objectStore('drafts').put({ key, value } satisfies StoredDraft<T>)
    await done
  })
}

export async function readDraft<T>(key: string): Promise<{ value: T; files: Map<string, File> } | null> {
  await flushDraft(key)
  const db = await open(), tx = db.transaction(['drafts', 'files'], 'readonly')
  const draftRequest = read<StoredDraft<T> | undefined>(tx.objectStore('drafts').get(key))
  const fileRequest = read<StoredFile[]>(tx.objectStore('files').index('draft').getAll(key))
  const [draft, originals] = await Promise.all([draftRequest, fileRequest])
  if (!draft) return null
  const files = new Map<string, File>()
  for (const original of originals) {
    fileIds.set(original.file, original.id)
    files.set(original.id, original.file)
  }
  return { value: draft.value, files }
}

// This updates just the file record's small handle metadata; IndexedDB keeps
// the File as an immutable blob reference, without a JS byte/base64 copy.
export function uploadBinding(key: string, file: File, endpoint: string, sourceId?: string): Promise<UploadBinding> {
  return serialize(key, async () => {
    const db = await open(), tx = db.transaction('files', 'readwrite'), done = finished(tx)
    const store = tx.objectStore('files'), id = fileId(file)
    const original = await read<StoredFile | undefined>(store.get(JSON.stringify([key, id])))
    if (!original) { tx.abort(); await done; throw new Error('附件草稿不存在，请重新选择文件') }
    let binding = original.bindings[endpoint]
    if (!binding || sourceId) {
      binding = { ...(binding || { uploadId: crypto.randomUUID() }), ...(sourceId ? { sourceId } : {}) }
      original.bindings[endpoint] = binding
      store.put(original)
    }
    await done
    return binding
  })
}
