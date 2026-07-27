import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./Input-field.vue', import.meta.url), 'utf8');

test('professional skill selector opens on a stopped click event', () => {
  const marker = source.indexOf('data-guide="chat-skill-selector"');
  const button = marker >= 0 ? source.slice(marker - 160, marker + 320) : '';

  assert.ok(button, 'skill selector button was not found');
  assert.match(button, /@click\.stop="toggleSkillSelector"/);
  assert.doesNotMatch(button, /@mousedown[^=]*="toggleSkillSelector"/);
});
