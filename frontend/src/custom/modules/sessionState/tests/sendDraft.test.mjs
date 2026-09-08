import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';

// Execute the production handlers with controlled upload/stream promises. No
// network requests, model calls, or changes to a user's conversation are made.
function script(relativePath) {
  const source = readFileSync(new URL(relativePath, import.meta.url), 'utf8');
  return ts.createSourceFile(relativePath, source.match(/<script[^>]*>([\s\S]*?)<\/script>/)[1], ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
}
function initializer(source, name) {
  for (const statement of source.statements) {
    if (!ts.isVariableStatement(statement)) continue;
    const declaration = statement.declarationList.declarations.find(item => item.name.getText(source) === name);
    if (declaration) return declaration.initializer.getText(source);
  }
  throw new Error(`Missing production handler: ${name}`);
}
function compile(code, bindings) {
  const js = ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
  return new Function(...Object.keys(bindings), js)(...Object.values(bindings));
}
const inputSource = script('../../../../components/Input-field.vue');
const chatSource = script('../../../../views/chat/index.vue');
const ref = value => ({ value });
const noop = () => {};
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function input(embeddedMode, mounted = true) {
  const query = ref('待发送文字');
  const sent = [];
  const props = { embeddedMode, isReplying: false };
  const bindings = {
    query, props, selectedModelId: ref('model'),
    getTextareaEl: () => mounted ? { value: query.value, blur: noop } : null,
    emit: (...args) => sent.push({ args, draftAtSend: query.value }),
    t: value => value, MessagePlugin: { info: noop, error: noop, warning: noop },
    chatResources: { isFresh: () => true }, isSelectedAgentResolved: ref(true),
    ensureModelSelection: noop, normalizeModelId: value => value, isUsableChatModelId: () => true,
    selectedAgent: ref({ config: { agent_mode: 'agent' } }),
    collectAgentNotReadyReasons: () => ({ keys: [], labels: [] }),
    hasUnresolvedSelectedFiles: () => false,
    settingsStore: { settings: {} }, selectedProfessionalSkillNames: ref([]),
    rawSelectedSkillNames: ref([]), selectedSkillNames: ref([]),
    allSelectedItems: ref([{ id: 'kb', name: '资料', type: 'kb' }]), skillMentionItems: ref([]),
    uploadedImages: ref([]), uploadedAttachments: ref([]),
  };
  const handlers = compile(`
    const clearvalue = ${initializer(inputSource, 'clearvalue')};
    const createSession = ${initializer(inputSource, 'createSession')};
    return { clearvalue, createSession };
  `, bindings);
  return { ...handlers, query, sent, props };
}

for (const embeddedMode of [false, true]) {
  test(`sending consumes input before the parent sees it (embedded=${embeddedMode})`, async () => {
    const state = input(embeddedMode);
    await state.createSession(state.query.value);
    assert.equal(state.query.value, '');
    assert.equal(state.sent.length, 1);
    assert.equal(state.sent[0].args[1], '待发送文字');
    assert.equal(state.sent[0].draftAtSend, '');
  });
}
test('clearing state does not depend on a mounted textarea', () => {
  const state = input(false, false);
  state.clearvalue();
  assert.equal(state.query.value, '');
});
test('blocked sends retain the input and never emit a request', async () => {
  const state = input(false);
  state.props.isReplying = true;
  await state.createSession(state.query.value);
  assert.equal(state.query.value, '待发送文字');
  assert.equal(state.sent.length, 0);
});

function conversation() {
  const uploads = deferred();
  const stream = deferred();
  let draft;
  let streamRequest;
  const settings = { selectedKnowledgeBases: ['kb'] };
  const field = { query: '', attachments: [], images: [] };
  const bindings = {
    props: { embeddedMode: false }, uploadsPreparing: ref(false),
    stopStream: noop, prepareForNewOutgoingMessage: noop, markCurrentSessionRead: noop,
    isReplying: ref(false), loading: ref(false), queueRejectionNotice: ref(null),
    session_id: ref('session'),
    useSettingsStoreInstance: {
      captureConversationScopedState: () => settings, selectedAgentId: 'agent',
      isAgentStreamMode: true, isWebSearchEnabled: false, settings,
    },
    chatImagePlaceholder: () => 'image-placeholder',
    saveSessionDraftState: (session, resources, attachments, images, query) => {
      draft = { session, resources, attachments, images, query };
    },
    prepareUploads: () => uploads.promise,
    inputFieldRef: ref({
      restoreQuery: value => { field.query = value; },
      setUploadedImages: value => { field.images = value; },
      setUploadedAttachments: value => { field.attachments = value; },
    }),
    MessagePlugin: { error: noop }, messagesList: [], userHasScrolledUp: ref(false),
    scrollToBottom: noop,
    startStream: request => { streamRequest = request; return stream.promise; },
    isAttachingImStream: ref(false), queueRejection: ref(null), currentAssistantMessageId: ref(''),
    saveCurrentConversationDraft: () => { draft = { ...draft, query: field.query, attachments: field.attachments, images: field.images }; },
  };
  const errorWatch = chatSource.statements.find(statement => ts.isExpressionStatement(statement)
    && ts.isCallExpression(statement.expression)
    && statement.expression.expression.getText(chatSource) === 'watch'
    && statement.expression.arguments[0]?.getText(chatSource) === 'error');
  assert.ok(errorWatch);
  const handlers = compile(`
    let pendingQueueDraft = null;
    const sendMsg = ${initializer(chatSource, 'sendMsg')};
    const onError = ${errorWatch.expression.arguments[1].getText(chatSource)};
    return { sendMsg, onError };
  `, bindings);
  return { ...handlers, uploads, stream, field, bindings,
    get draft() { return draft; },
    get streamRequest() { return streamRequest; },
  };
}

test('submitted draft clears while the answer is still running; resources stay selected', async () => {
  const state = conversation();
  const attachments = [{ name: '资料.txt', size: 12, file: {} }];
  const run = state.sendMsg('请生成文档', 'model', [], [], attachments);
  assert.equal(state.draft.query, '请生成文档', 'pending uploads retain recoverable text');
  state.uploads.resolve(['upload']);
  await Promise.resolve();
  assert.equal(state.streamRequest.query, '请生成文档');
  assert.equal(state.draft.query, '');
  assert.equal(state.draft.attachments, attachments);
  assert.deepEqual(state.draft.resources.selectedKnowledgeBases, ['kb']);
  // The user can compose the next turn before this stream finishes.
  state.field.query = '下一条草稿';
  state.bindings.saveCurrentConversationDraft();
  state.stream.resolve();
  await run;
  assert.equal(state.draft.query, '下一条草稿');
});

test('upload failure restores the unsent text and attachments', async () => {
  const state = conversation();
  const attachments = [{ name: '资料.txt', size: 12, file: {} }];
  const run = state.sendMsg('请生成文档', 'model', [], [], attachments);
  state.uploads.reject(new Error('upload failed'));
  await run;
  assert.equal(state.streamRequest, undefined);
  assert.equal(state.field.query, '请生成文档');
  assert.equal(state.draft.query, '请生成文档');
  assert.equal(state.field.attachments, attachments);
  assert.equal(state.bindings.isReplying.value, false);
});

test('queue rejection restores the draft and completion cannot erase it', async () => {
  const state = conversation();
  const run = state.sendMsg('请生成文档');
  state.uploads.resolve([]);
  await Promise.resolve();
  state.bindings.queueRejection.value = { message: 'queue full' };
  state.onError('queue full');
  state.stream.resolve();
  await run;
  assert.equal(state.field.query, '请生成文档');
  assert.equal(state.draft.query, '请生成文档');
  assert.equal(state.bindings.messagesList.length, 0);
});
