import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const messageSource = readFileSync(join(here, 'components', 'MobileChatMessage.vue'), 'utf8')
const previewSource = readFileSync(join(here, 'components', 'MobileDocumentPreviewSheet.vue'), 'utf8')
const managerSource = readFileSync(join(here, 'views', 'MobileKnowledgeManager.vue'), 'utf8')
const downloadSource = readFileSync(join(here, 'documentDownload.ts'), 'utf8')
const webArtifactSource = readFileSync(
  join(here, '..', '..', '..', 'views', 'chat', 'components', 'tool-results', 'GeneralAgentArtifactsResult.vue'),
  'utf8',
)

test('mobile artifacts reuse the knowledge document preview sheet and mobile-fit renderer', () => {
  assert.match(messageSource, /import MobileDocumentPreviewSheet from "\.\/MobileDocumentPreviewSheet\.vue"/)
  assert.match(messageSource, /:blob-loader="loadArtifactPreviewBlob"/)
  assert.match(messageSource, /:download-handler="downloadPreviewArtifact"/)
  assert.match(previewSource, /blobLoader\?: \(signal\?: AbortSignal\) => Promise<Blob>/)
  assert.match(previewSource, /:mobile-fit="true"/)
  assert.match(managerSource, /<MobileDocumentPreviewSheet/)
})

test('mobile artifact actions place preview before native download', () => {
  const actionsStart = messageSource.indexOf('<div class="mobile-artifact-file__actions">')
  const actionsEnd = messageSource.indexOf('</div>', actionsStart)
  const actions = messageSource.slice(actionsStart, actionsEnd)
  const previewPosition = actions.indexOf('name="eye"')
  const downloadPosition = actions.indexOf('name="download"')

  assert.ok(actionsStart >= 0)
  assert.ok(previewPosition >= 0)
  assert.ok(downloadPosition > previewPosition)
})

test('artifact and knowledge downloads both use signed top-level navigation', () => {
  assert.match(managerSource, /downloadKnowledgeNatively\(item\.id\)/)
  assert.match(messageSource, /downloadArtifactNatively\(file\.artifact_id\)/)
  assert.doesNotMatch(messageSource, /downloadBlob\(/)
  assert.match(downloadSource, /window\.location\.assign\(url\)/)
  assert.match(downloadSource, /mobile-documents\/artifacts\/\$\{encodeURIComponent\(id\)\}\/download-link/)
})

test('web artifact dialog stays below the teleported fullscreen preview', () => {
  assert.match(webArtifactSource, /:z-index="1900"/)
})
