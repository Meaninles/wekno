import { useRouter } from 'vue-router'
import { useMenuStore } from '@/stores/menu'

/**
 * Shared "start a new chat with query + KB/file preselected" helper.
 *
 * Used from:
 *   - Global command palette (⌘K) when user presses ⌘↵ on a result
 *   - KB-scoped search bar on the KB detail page
 *   - Empty-state "Ask AI directly" button in the palette
 *
 * Mirrors the legacy behavior of KnowledgeSearch.vue#startChat.
 */
export function useStartChat() {
  const router = useRouter()
  const menuStore = useMenuStore()

  /**
   * @param query The user's query; it becomes the pre-filled first message
   * @param kbIds Knowledge bases to scope the new chat to; empty means no constraint
   * @param fileIds Specific knowledge/file ids to attach as context
   */
  const startChat = (query: string, kbIds: string[] = [], fileIds: string[] = []) => {
    const q = (query || '').trim()
    if (!q) return

    menuStore.setPrefillQuery(q)
    router.push({
      path: '/platform/creatChat',
      query: {
        ...(kbIds.length > 0 ? { kb_ids: kbIds.join(',') } : {}),
        ...(fileIds.length > 0 ? { file_ids: fileIds.join(',') } : {}),
      },
    })
  }

  return { startChat }
}
