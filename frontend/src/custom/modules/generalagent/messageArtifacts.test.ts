import test from 'node:test';
import assert from 'node:assert/strict';
import { artifactResultForMessage } from './messageArtifacts.ts';

test('committed artifacts render without tool events and survive JSON history roundtrip', () => {
 const message = { artifacts: [
  { artifact_id:'a', filename:'same.txt', file_size:5, download_url:'/a' },
  { artifact_id:'b', filename:'same.txt', file_size:7, download_url:'/b' },
 ], artifact_notice:'部分文件未交付' };
 const live = artifactResultForMessage(message)!;
 assert.equal(live.artifacts.length, 2);
 assert.equal(live.artifact_returned_size, 12);
 assert.equal(live.notice, message.artifact_notice);
 assert.deepEqual(artifactResultForMessage(JSON.parse(JSON.stringify(message))), live);
});
test('ordinary answers have no artifact block; rejection-only delivery shows its notice', () => {
 assert.equal(artifactResultForMessage({}), null);
 assert.equal(artifactResultForMessage({artifacts:[]}), null);
 assert.equal(artifactResultForMessage({artifact_notice:'文件无法打开'})?.notice, '文件无法打开');
});
