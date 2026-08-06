import assert from 'node:assert/strict'
import test from 'node:test'

import { resolveDocumentPreviewType } from '../../../utils/documentPreview.ts'

test('mobile-safe preview formats reject legacy binary office and browser-incompatible TIFF', () => {
  assert.equal(resolveDocumentPreviewType('docx'), 'docx')
  assert.equal(resolveDocumentPreviewType('pptx'), 'pptx')
  assert.equal(resolveDocumentPreviewType('ppt'), 'unsupported')
  assert.equal(resolveDocumentPreviewType('tiff'), 'unsupported')
  assert.equal(resolveDocumentPreviewType('webp'), 'image')
})
