export type AgentConversationMode = 'agent'
export const resolveAgentEnabledFromMode = (mode?: string | null): boolean | null => mode === 'agent' ? true : null
