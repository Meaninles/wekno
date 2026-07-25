export type AgentConversationMode = 'quick-answer' | 'smart-reasoning'

/**
 * Return the conversation runtime flag represented by a resolved agent mode.
 * Unknown/empty values stay untouched so an unresolved async agent response
 * cannot briefly disable a valid persisted selection.
 */
export const resolveAgentEnabledFromMode = (
  mode?: string | null,
): boolean | null => {
  if (mode === 'smart-reasoning') return true
  if (mode === 'quick-answer') return false
  return null
}
