export interface GraphCenterState {
  center: string
  history: string[]
}

export function navigateGraphCenterState(
  state: GraphCenterState,
  nextCenter: string,
  recordHistory = true,
  historyLimit = 100,
): GraphCenterState {
  const next = nextCenter.trim()
  if (!next || next === state.center) {
    return { center: state.center, history: [...state.history] }
  }
  const history = [...state.history]
  if (recordHistory && state.center) {
    history.push(state.center)
  }
  const safeLimit = Math.max(1, historyLimit)
  return {
    center: next,
    history: history.slice(-safeLimit),
  }
}

export function popGraphCenterState(state: GraphCenterState): GraphCenterState {
  if (state.history.length === 0) {
    return { center: state.center, history: [] }
  }
  const history = [...state.history]
  const center = history.pop() || state.center
  return { center, history }
}

export function clampGraphPage(page: number, totalPages: number): number {
  const total = Math.max(1, Math.trunc(totalPages) || 1)
  const requested = Math.trunc(page) || 1
  return Math.max(1, Math.min(requested, total))
}
