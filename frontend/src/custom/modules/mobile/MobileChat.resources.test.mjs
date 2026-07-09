import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'views', 'MobileChat.vue'), 'utf8')

test('mobile resource chips collapse duplicate lightweight/professional skill names', () => {
  assert.match(source, /const selectedProfessionalSkillNameSet = computed\(\(\) => new Set\(selectedProfessionalSkillNames\.value\)\)/)
  assert.match(source, /rawSelectedSkillNames\.value\.filter\(\(name\) => !selectedProfessionalSkillNameSet\.value\.has\(name\)\)/)
  assert.match(source, /`skill-name:\$\{name\}`/)
  assert.match(source, /type: "professional"/)
})

test('mobile resource restore and upload paths dedupe repeated files', () => {
  assert.match(source, /const uniqueFilesByIdentity = \(files: File\[\]\) =>/)
  assert.match(source, /const uniqueAttachmentsByIdentity = \(attachments: MobileUploadAttachment\[\]\) =>/)
  assert.match(source, /pendingAttachments\.value = uniqueAttachmentsByIdentity\(fromDraftAttachments/)
  assert.match(source, /pendingImages\.value = uniqueFilesByIdentity\(/)
  assert.match(source, /pendingImages\.value = uniqueFilesByIdentity\(\[/)
  assert.match(source, /pendingAttachments\.value = uniqueAttachmentsByIdentity\(\[/)
})

test('mobile agent sheet only marks the exact selected built-in agent', () => {
  assert.match(source, /\[BUILTIN_SIMPLE_CHAT_ID\]: "简单对话"/)
  assert.match(source, /const isSelectedAgent = \(agent: CustomAgent\) => selectedAgentId\.value === agent\.id/)
  assert.match(source, /:class="\{ selected: isSelectedAgent\(agent\) \}"/)
  assert.doesNotMatch(source, /\[BUILTIN_QUICK_ANSWER_ID,\s*BUILTIN_SIMPLE_CHAT_ID\][\s\S]*agent\.id === BUILTIN_QUICK_ANSWER_ID/)
})
