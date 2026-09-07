import { BUILTIN_KNOWLEDGE_QA_ID, BUILTIN_GENERAL_AGENT_ID, BUILTIN_SIMPLE_CHAT_ID, BUILTIN_WIKI_RESEARCHER_ID, BUILTIN_WIKI_FIXER_ID, BUILTIN_DATA_ANALYST_ID, BUILTIN_TABLE_ANALYST_ID, BUILTIN_DOCUMENT_PROCESSING_ID } from '@/api/agent'
export function isAgentStreamBuiltinAgentId(id?: string | null): boolean {
  return [BUILTIN_KNOWLEDGE_QA_ID, BUILTIN_GENERAL_AGENT_ID, BUILTIN_SIMPLE_CHAT_ID, BUILTIN_WIKI_RESEARCHER_ID, BUILTIN_WIKI_FIXER_ID, BUILTIN_DATA_ANALYST_ID, BUILTIN_TABLE_ANALYST_ID, BUILTIN_DOCUMENT_PROCESSING_ID].includes(id || "")
}
export function isAgentStreamAgentId(_id?: string | null, _enabled?: boolean): boolean { return true }
export function reconcileBuiltinAgentMode(settings: { selectedAgentId?: string; isAgentEnabled: boolean }): boolean {
  const changed = !settings.isAgentEnabled
  settings.isAgentEnabled = true
  return changed
}
