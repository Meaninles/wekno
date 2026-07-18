import assert from 'node:assert/strict';
import test from 'node:test';

import {
  ARTIFACT_PAGE_SIZE,
  artifactMetaText,
  artifactTotalCount,
  nextArtifactVisibleCount,
  remainingArtifactCount,
  visibleArtifacts,
} from './artifactPagination.ts';

test('shows ten artifacts initially and adds ten per request', () => {
  const files = Array.from({ length: 38 }, (_, index) => index + 1);

  assert.equal(ARTIFACT_PAGE_SIZE, 10);
  assert.deepEqual(visibleArtifacts(files, ARTIFACT_PAGE_SIZE), files.slice(0, 10));
  assert.equal(nextArtifactVisibleCount(10, files.length), 20);
  assert.equal(nextArtifactVisibleCount(20, files.length), 30);
  assert.equal(nextArtifactVisibleCount(30, files.length), 38);
  assert.equal(remainingArtifactCount(files.length, 30), 8);
});

test('uses the true artifact total and reports unavailable entries separately', () => {
  assert.equal(artifactTotalCount({ artifact_original_count: 38 }, 38), 38);
  assert.equal(artifactMetaText({ artifact_original_count: 38 }, 38), '共 38 个文件');
  assert.equal(
    artifactMetaText({ artifact_original_count: 40, artifact_returned_count: 38 }, 38),
    '共 40 个文件（可展示 38 个）',
  );
});
