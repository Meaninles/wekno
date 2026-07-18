export const ARTIFACT_PAGE_SIZE = 10;

export interface ArtifactCountData {
  artifact_original_count?: number | string;
  artifact_returned_count?: number | string;
}

function nonNegativeInteger(value: unknown): number {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed <= 0) return 0;
  return Math.floor(parsed);
}

export function artifactTotalCount(data: ArtifactCountData | null | undefined, availableCount: number): number {
  return Math.max(
    nonNegativeInteger(data?.artifact_original_count),
    nonNegativeInteger(data?.artifact_returned_count),
    nonNegativeInteger(availableCount),
  );
}

export function artifactMetaText(data: ArtifactCountData | null | undefined, availableCount: number): string {
  const available = nonNegativeInteger(availableCount);
  const total = artifactTotalCount(data, available);
  if (total > available) {
    return `共 ${total} 个文件（可展示 ${available} 个）`;
  }
  return `共 ${total} 个文件`;
}

export function visibleArtifacts<T>(files: readonly T[], visibleCount: number): readonly T[] {
  return files.slice(0, Math.max(ARTIFACT_PAGE_SIZE, nonNegativeInteger(visibleCount)));
}

export function remainingArtifactCount(availableCount: number, visibleCount: number): number {
  return Math.max(0, nonNegativeInteger(availableCount) - nonNegativeInteger(visibleCount));
}

export function nextArtifactVisibleCount(currentCount: number, availableCount: number): number {
  return Math.min(
    nonNegativeInteger(availableCount),
    Math.max(ARTIFACT_PAGE_SIZE, nonNegativeInteger(currentCount)) + ARTIFACT_PAGE_SIZE,
  );
}
