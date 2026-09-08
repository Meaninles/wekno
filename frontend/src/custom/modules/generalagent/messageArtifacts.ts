import { normalizeMessageArtifacts } from '../chatshare/media.ts';

export function artifactResultForMessage(message: { artifacts?: unknown; artifact_notice?: string }) {
  const artifacts = normalizeMessageArtifacts(message.artifacts);
  const notice = message.artifact_notice || '';
  if (!artifacts.length && !notice) return null;
  return {
    display_type: 'general_agent_artifacts' as const,
    artifacts,
    notice,
    artifact_original_count: artifacts.length,
    artifact_returned_count: artifacts.length,
    artifact_returned_size: artifacts.reduce((size, file) => size + file.file_size, 0),
  };
}
