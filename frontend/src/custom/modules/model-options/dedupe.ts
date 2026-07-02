export interface ModelOptionLike {
  id?: string
  name: string
  display_name?: string
  type?: string
  source?: string
  description?: string
  status?: string
  parameters?: {
    parameter_size?: string
  }
}

function normalized(value: string | undefined): string {
  return (value || '').trim().toLowerCase()
}

function optionIdentity(model: ModelOptionLike): string {
  return [
    normalized(model.type),
    normalized(model.source),
    normalized(model.display_name),
    normalized(model.name),
    normalized(model.description),
    normalized(model.parameters?.parameter_size),
  ].join('\x1f')
}

function shouldPreferCandidate<T extends ModelOptionLike>(
  current: T,
  candidate: T,
  preferredModelId: string,
): boolean {
  const currentId = normalized(current.id)
  const candidateId = normalized(candidate.id)
  if (preferredModelId) {
    if (candidateId === preferredModelId) return true
    if (currentId === preferredModelId) return false
  }

  const currentActive = normalized(current.status) === 'active'
  const candidateActive = normalized(candidate.status) === 'active'
  if (currentActive !== candidateActive) return candidateActive

  return false
}

export function dedupeChatModelOptions<T extends ModelOptionLike>(
  models: T[],
  preferredModelId = '',
): T[] {
  const preferred = normalized(preferredModelId)
  const byIdentity = new Map<string, T>()

  for (const model of models) {
    const identity = optionIdentity(model)
    const current = byIdentity.get(identity)
    if (!current || shouldPreferCandidate(current, model, preferred)) {
      byIdentity.set(identity, model)
    }
  }

  return Array.from(byIdentity.values())
}
