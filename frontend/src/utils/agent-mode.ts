import {
  BUILTIN_DATA_ANALYST_ID,
  BUILTIN_DEEP_RESEARCHER_ID,
  BUILTIN_DOCUMENT_PROCESSING_ID,
  BUILTIN_GENERAL_AGENT_ID,
  BUILTIN_KNOWLEDGE_GRAPH_EXPERT_ID,
  BUILTIN_QUICK_ANSWER_ID,
  BUILTIN_SIMPLE_CHAT_ID,
  BUILTIN_SMART_REASONING_ID,
  BUILTIN_TABLE_ANALYST_ID,
  BUILTIN_WIKI_RESEARCHER_ID,
} from '@/api/agent'

const NORMAL_MODE_BUILTIN_AGENT_IDS = new Set([
  BUILTIN_QUICK_ANSWER_ID,
  BUILTIN_SIMPLE_CHAT_ID,
])

const AGENT_STREAM_BUILTIN_AGENT_IDS = new Set([
  BUILTIN_SMART_REASONING_ID,
  BUILTIN_WIKI_RESEARCHER_ID,
  BUILTIN_DEEP_RESEARCHER_ID,
  BUILTIN_DATA_ANALYST_ID,
  BUILTIN_TABLE_ANALYST_ID,
  BUILTIN_GENERAL_AGENT_ID,
  BUILTIN_DOCUMENT_PROCESSING_ID,
  BUILTIN_KNOWLEDGE_GRAPH_EXPERT_ID,
])

/** Whether the selected agent id uses the normal KnowledgeQA stream pipeline. */
export function isQuickAnswerAgentId(agentId: string | null | undefined): boolean {
  return NORMAL_MODE_BUILTIN_AGENT_IDS.has(agentId || BUILTIN_QUICK_ANSWER_ID)
}

/** Whether the selected built-in agent id always uses the Agent stream pipeline. */
export function isAgentStreamBuiltinAgentId(agentId: string | null | undefined): boolean {
  return AGENT_STREAM_BUILTIN_AGENT_IDS.has(agentId || '')
}

/** Whether requests should use the Agent stream pipeline (not quick-answer RAG). */
export function isAgentStreamAgentId(
  agentId: string | null | undefined,
  isAgentEnabled: boolean,
): boolean {
  const id = agentId || BUILTIN_QUICK_ANSWER_ID
  if (NORMAL_MODE_BUILTIN_AGENT_IDS.has(id)) return false
  if (AGENT_STREAM_BUILTIN_AGENT_IDS.has(id)) return true
  return isAgentEnabled
}

/** Reconcile builtin agent id with isAgentEnabled after localStorage reload. */
export function reconcileBuiltinAgentMode(settings: {
  selectedAgentId?: string
  isAgentEnabled: boolean
}): boolean {
  const agentId = settings.selectedAgentId || BUILTIN_QUICK_ANSWER_ID
  if (NORMAL_MODE_BUILTIN_AGENT_IDS.has(agentId) && settings.isAgentEnabled) {
    settings.isAgentEnabled = false
    return true
  }
  if (AGENT_STREAM_BUILTIN_AGENT_IDS.has(agentId) && !settings.isAgentEnabled) {
    settings.isAgentEnabled = true
    return true
  }
  return false
}
