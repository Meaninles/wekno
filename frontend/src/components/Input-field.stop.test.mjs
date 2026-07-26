import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./Input-field.vue', import.meta.url), 'utf8');

test('stop request snapshots reactive identifiers before clearing parent state', () => {
  const handlerStart = source.indexOf('const handleStop = async () => {');
  const handlerEnd = source.indexOf('\nonBeforeRouteUpdate', handlerStart);
  assert.ok(handlerStart >= 0 && handlerEnd > handlerStart);

  const handler = source.slice(handlerStart, handlerEnd);
  const sessionSnapshot = handler.indexOf('const sessionId = props.sessionId;');
  const messageSnapshot = handler.indexOf('const assistantMessageId = props.assistantMessageId;');
  const clearParentState = handler.indexOf("emit('stop-generation');");
  const stopRequest = handler.indexOf('stopSession(sessionId, assistantMessageId)');

  assert.ok(sessionSnapshot >= 0);
  assert.ok(messageSnapshot >= 0);
  assert.ok(clearParentState > sessionSnapshot);
  assert.ok(clearParentState > messageSnapshot);
  assert.ok(stopRequest > clearParentState);
  assert.doesNotMatch(handler, /stopSession\(props\.sessionId,\s*props\.assistantMessageId\)/);
});
