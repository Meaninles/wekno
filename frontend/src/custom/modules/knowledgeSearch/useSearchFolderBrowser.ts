import { computed, ref } from 'vue'
import { listKnowledgeFolderNodes } from '@/custom/modules/knowledgeFolders/api'
import type {
  KnowledgeFolder,
  KnowledgeFolderBreadcrumb,
  KnowledgeFolderNode,
} from '@/custom/modules/knowledgeFolders/types'

export const SEARCH_FOLDER_BROWSER_PAGE_SIZE = 10

export function useSearchFolderBrowser() {
  const originKey = ref('')
  const knowledgeBaseId = ref('')
  const rootFolder = ref<KnowledgeFolder | null>(null)
  const currentFolder = ref<KnowledgeFolder | null>(null)
  const breadcrumbs = ref<KnowledgeFolderBreadcrumb[]>([])
  const nodes = ref<KnowledgeFolderNode[]>([])
  const page = ref(1)
  const total = ref(0)
  const loading = ref(false)
  const error = ref('')
  let requestGeneration = 0

  const totalPages = computed(() =>
    Math.max(1, Math.ceil(total.value / SEARCH_FOLDER_BROWSER_PAGE_SIZE)),
  )

  const collapse = () => {
    requestGeneration += 1
    originKey.value = ''
    knowledgeBaseId.value = ''
    rootFolder.value = null
    currentFolder.value = null
    breadcrumbs.value = []
    nodes.value = []
    page.value = 1
    total.value = 0
    loading.value = false
    error.value = ''
  }

  const load = async (folderId: string, targetPage = 1) => {
    if (!knowledgeBaseId.value || !folderId) return
    const generation = ++requestGeneration
    loading.value = true
    error.value = ''
    try {
      const response: any = await listKnowledgeFolderNodes(knowledgeBaseId.value, {
        folder_id: folderId,
        page: targetPage,
        page_size: SEARCH_FOLDER_BROWSER_PAGE_SIZE,
      })
      if (generation !== requestGeneration) return
      nodes.value = Array.isArray(response?.data) ? response.data : []
      currentFolder.value = response?.current || (
        rootFolder.value?.id === folderId ? rootFolder.value : currentFolder.value
      )
      breadcrumbs.value = Array.isArray(response?.breadcrumbs) ? response.breadcrumbs : []
      page.value = Number(response?.page || targetPage)
      total.value = Number(response?.total || 0)
    } catch (cause: any) {
      if (generation !== requestGeneration) return
      nodes.value = []
      total.value = 0
      error.value = cause?.message || '文件夹内容加载失败'
    } finally {
      if (generation === requestGeneration) loading.value = false
    }
  }

  const toggleRoot = async (
    key: string,
    targetKnowledgeBaseId: string,
    folder: KnowledgeFolder,
  ) => {
    if (originKey.value === key) {
      collapse()
      return
    }
    originKey.value = key
    knowledgeBaseId.value = targetKnowledgeBaseId
    rootFolder.value = folder
    currentFolder.value = folder
    breadcrumbs.value = [{ id: folder.id, name: folder.name }]
    nodes.value = []
    page.value = 1
    total.value = 0
    error.value = ''
    await load(folder.id, 1)
  }

  const enterFolder = async (folder: KnowledgeFolder) => {
    await load(folder.id, 1)
  }

  const openBreadcrumb = async (folderId: string) => {
    await load(folderId, 1)
  }

  const changePage = async (targetPage: number) => {
    const folderId = currentFolder.value?.id
    if (!folderId || targetPage < 1 || targetPage > totalPages.value) return
    await load(folderId, targetPage)
  }

  return {
    originKey,
    knowledgeBaseId,
    rootFolder,
    currentFolder,
    breadcrumbs,
    nodes,
    page,
    total,
    totalPages,
    loading,
    error,
    collapse,
    toggleRoot,
    enterFolder,
    openBreadcrumb,
    changePage,
  }
}
