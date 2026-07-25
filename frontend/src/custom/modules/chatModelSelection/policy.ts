export type ChatModelSelectionSource =
  | 'explicit-user'
  | 'agent-default'
  | 'current'
  | 'last-user'
  | 'catalog-default'
  | 'none'

export interface ChatModelSelectionInput {
  currentModelId?: string | null
  agentModelId?: string | null
  lastUserModelId?: string | null
  availableModelIds?: Array<string | null | undefined>
  catalogReady: boolean
}

export interface ChatModelSelection {
  modelId: string
  source: ChatModelSelectionSource
}

const normalizeModelId = (value?: string | null): string => String(value || '').trim()

/**
 * Resolve the effective chat model without losing an explicit UI choice.
 *
 * `lastUserModelId` is only written by the model selector. Matching it against
 * the current store value lets the choice survive the create-chat -> chat
 * component remount, while an agent switch can still replace it with that
 * agent's configured default.
 */
export const resolveChatModelSelection = (
  input: ChatModelSelectionInput,
): ChatModelSelection => {
  const currentModelId = normalizeModelId(input.currentModelId)
  const agentModelId = normalizeModelId(input.agentModelId)
  const lastUserModelId = normalizeModelId(input.lastUserModelId)
  const availableModelIds = (input.availableModelIds || [])
    .map(normalizeModelId)
    .filter(Boolean)
  const available = new Set(availableModelIds)
  const isSelectable = (modelId: string): boolean =>
    !!modelId && (modelId === agentModelId || available.has(modelId))

  // The catalog can be empty briefly during tenant/resource hydration. Keep a
  // known explicit choice until the loaded catalog can actually validate it.
  if (
    currentModelId
    && currentModelId === lastUserModelId
    && (!input.catalogReady || isSelectable(currentModelId))
  ) {
    return { modelId: currentModelId, source: 'explicit-user' }
  }

  if (agentModelId) {
    return { modelId: agentModelId, source: 'agent-default' }
  }

  if (currentModelId && (!input.catalogReady || isSelectable(currentModelId))) {
    return { modelId: currentModelId, source: 'current' }
  }

  if (lastUserModelId && (!input.catalogReady || isSelectable(lastUserModelId))) {
    return { modelId: lastUserModelId, source: 'last-user' }
  }

  if (availableModelIds.length > 0) {
    return { modelId: availableModelIds[0], source: 'catalog-default' }
  }

  return { modelId: '', source: 'none' }
}
