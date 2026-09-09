import assert from 'node:assert/strict'
import test from 'node:test'
import { build } from 'esbuild'
import { readFile } from 'node:fs/promises'
import { dirname } from 'node:path'
import { parse, compileScript } from '@vue/compiler-sfc'

// Exercise the real composable with deterministic transport/storage, not a
// second implementation of its state machine.
const compiled = await build({
  stdin: { contents: `export { useChatUploads } from './src/custom/modules/chatuploads/uploads.ts'; export { default as Card } from './src/custom/modules/chatuploads/ChatUploadCards.vue'; export { effectScope, createSSRApp } from 'vue'; export { renderToString } from 'vue/server-renderer'`, resolveDir: process.cwd() },
  bundle: true, write: false, platform: 'node', format: 'esm',
  plugins: [{ name: 'upload-fixtures', setup(builder) {
    builder.onLoad({filter: /\.vue$/}, async args => {
      const {descriptor} = parse(await readFile(args.path, 'utf8'))
      return {contents: compileScript(descriptor, {id: 'upload-test', inlineTemplate: true}).content, loader: 'ts', resolveDir: dirname(args.path)}
    })
    builder.onResolve({filter: /^(tdesign-vue-next|vue-i18n)$/}, args => ({path: args.path, namespace: 'ui-fixture'}))
    builder.onLoad({filter: /.*/, namespace: 'ui-fixture'}, () => ({contents: `export const MessagePlugin = {}; export const useI18n = () => ({t: key => key})`}))
    builder.onResolve({ filter: /^(.*sessionState\/storage|@\/utils\/request)$/ }, args => ({ path: args.path, namespace: 'fixture' }))
    builder.onLoad({ filter: /.*/, namespace: 'fixture' }, args => ({ contents: args.path.includes('storage') ? `
      const ids = new WeakMap(); let next = 0;
      export const fileId = file => { if (!ids.has(file)) ids.set(file, String(++next)); return ids.get(file) };
      export const draftKey = id => id, embedDraftScope = () => '', flushDraft = async () => {};
      export const uploadBinding = async (key, file) => ({uploadId: fileId(file)});
    ` : `
      export const postUpload = (...args) => globalThis.uploadFixture(...args);
      export const get = async () => ({data: []}), post = async () => {}, del = async () => {};
    ` }))
  } }],
})
const { useChatUploads, effectScope, Card, createSSRApp, renderToString } = await import('data:text/javascript;base64,' + Buffer.from(compiled.outputFiles[0].text).toString('base64'))
const tick = () => new Promise(resolve => setImmediate(resolve))
const ctx = { sessionId: 'visible-session', directInput: true }

test('cards render a real upload state, completion mark and always-mounted file picker', async () => {
  const files = [{id: 'file', name: 'test.txt', file: new File(['x'], 'test.txt'), size: 1}]
  const render = state => renderToString(createSSRApp(Card, {files, rows: [{key: 'file', name: 'test.txt', state}]}))
  assert.match(await render('uploading'), /is-uploading/)
  assert.match(await render('uploading'), /上传中/)
  assert.match(await render('ready'), /上传完成/)
  assert.match(await render('ready'), /state-mark is-ready/)
  assert.doesNotMatch(await render('ready'), /等待上传/)
  const empty = await renderToString(createSSRApp(Card, {files: [], rows: []}))
  assert.match(empty, /type="file"/)
  assert.doesNotMatch(empty, /class="chat-upload-card"/)
  const source = await readFile('src/custom/modules/chatuploads/ChatUploadCards.vue', 'utf8')
  assert.match(source, /is-ready \.chat-upload-card__progress-ring \{ display: none; \}/)
})

test('oversized attachments explain the 128 MiB limit and knowledge-base alternative', async () => {
  const scope = effectScope(), uploads = scope.run(useChatUploads)
  const file = new File(['fixture'], 'too-large.wav')
  Object.defineProperty(file, 'size', {value: 128 * 1024 * 1024 + 1})
  await assert.rejects(uploads.prepare([file], ctx), error => /128/.test(error.message) && /知识库.*解析成功/.test(error.message))
  scope.stop()
})

test('uploads track individual files across additions, retry, completion and departure', async () => {
  const requests = []
  globalThis.uploadFixture = (url, form, unused, config) => new Promise((resolve, reject) => {
    const request = { id: form.get('upload_id'), file: form.get('file'), resolve, reject }
    requests.push(request)
    config.signal.addEventListener('abort', () => reject(new DOMException('cancelled', 'AbortError')), { once: true })
  })
  const scope = effectScope()
  const uploads = scope.run(useChatUploads)
  const a = new File(['first'], 'same.txt'), b = new File(['other'], 'same.txt'), c = new File(['third'], 'new.txt')
  const first = uploads.prepare([a], ctx)
  await tick()
  assert.equal(uploads.rows.value[0].state, 'uploading')
  const all = uploads.prepare([a, b, c], ctx)
  await tick()
  assert.equal(requests.length, 2)
  assert.deepEqual(uploads.rows.value.map(r => r.state), ['uploading', 'uploading', 'queued'])
  const complete = r => r.resolve({data: {id: r.id, input_file_id: r.id, original_input: true, ready: true}})
  complete(requests[0])
  await first
  await tick()
  assert.equal(requests.length, 3)
  requests[1].reject({status: 400, message: 'retry me'})
  await tick()
  assert.equal(uploads.rows.value[1].state, 'failed')
  uploads.retry(uploads.rows.value[1].key)
  await tick()
  complete(requests[2]); complete(requests[3])
  assert.equal((await all).inputFileIds.length, 3)
  assert.ok(uploads.rows.value.every(r => r.state === 'ready'))
  assert.equal(uploads.preparing.value, false)
  await uploads.prepare([b, c], ctx)
  assert.equal(requests.length, 4)
  assert.equal(uploads.rows.value.length, 2)
  const waiting = uploads.prepare([new File(['x'], 'pending.txt')], ctx)
  const rejected = assert.rejects(waiting, { name: 'AbortError' })
  await tick()
  uploads.detach()
  await rejected
  assert.deepEqual(uploads.rows.value, [])
  scope.stop()
  delete globalThis.uploadFixture
})

test('hiding the page cancels the send waiter; returning resumes only uploads', async () => {
  const page = new EventTarget()
  page.hidden = false
  globalThis.document = page
  let finish, calls = 0
  globalThis.uploadFixture = (url, form, unused, config) => new Promise((resolve, reject) => {
    calls++
    finish = () => resolve({data: {id: form.get('upload_id'), original_input: true, ready: true}})
    config.signal.addEventListener('abort', () => reject(new DOMException('cancelled', 'AbortError')), { once: true })
  })
  const scope = effectScope(), uploads = scope.run(useChatUploads)
  const waiting = uploads.prepare([new File(['content'], 'source.txt')], ctx)
  const rejected = assert.rejects(waiting, {name: 'AbortError'})
  await tick()
  page.hidden = true
  page.dispatchEvent(new Event('visibilitychange'))
  await rejected
  page.hidden = false
  page.dispatchEvent(new Event('visibilitychange'))
  await tick()
  assert.equal(calls, 2)
  finish()
  await tick()
  assert.equal(uploads.rows.value[0].state, 'ready')
  scope.stop()
  delete globalThis.document
  delete globalThis.uploadFixture
})
