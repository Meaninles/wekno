export const RUN_WAITING_UPDATE_INTERVAL_MS = 5_000

type RunMessage = Record<string, unknown>

// Only true ReAct turns and Claude Agent SDK turns receive the timed waiting
// projection. `isAgentMode` is intentionally not used because quick QA reuses
// the Agent renderer for its RAG pipeline.
export function usesTimedRunWaiting(message?: RunMessage | null): boolean {
  return Boolean(
    message?.agent_mode === true ||
      message?._usesClaudeSDKTerminalDelivery === true,
  )
}

export function runWaitingElapsedSeconds(elapsedMs: number): number {
  const normalized = Number.isFinite(elapsedMs) ? Math.max(0, elapsedMs) : 0
  return Math.floor(normalized / RUN_WAITING_UPDATE_INTERVAL_MS) *
    (RUN_WAITING_UPDATE_INTERVAL_MS / 1_000)
}

export function formatRunWaiting(elapsedMs: number): string {
  const seconds = runWaitingElapsedSeconds(elapsedMs)
  return seconds > 0
    ? `正在处理，请稍候 · 已用 ${seconds} 秒`
    : '正在处理，请稍候'
}

