import { del, get, post, postUpload, put } from '@/utils/request'
import type { KnowledgeProcessOverrides } from '@/types/knowledgeProcess'
import type {
  KnowledgeFolder,
	KnowledgeBaseTaskStats,
	KnowledgeFolderDeleteOperation,
  KnowledgeFolderListParams,
  KnowledgeFolderNodePage,
  KnowledgeFolderOption,
} from './types'

const base = (knowledgeBaseId: string) =>
  `/api/v1/custom/knowledge-folders/knowledge-bases/${encodeURIComponent(knowledgeBaseId)}`

export function listKnowledgeFolderNodes(
  knowledgeBaseId: string,
  params: KnowledgeFolderListParams = {},
) {
  const query = new URLSearchParams()
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== null && value !== '') {
      query.set(key, String(value))
    }
  })
  const qs = query.toString()
  return get<KnowledgeFolderNodePage>(`${base(knowledgeBaseId)}/nodes${qs ? `?${qs}` : ''}`)
}

export function listKnowledgeFolderOptions(knowledgeBaseId: string) {
  return get<{ success: boolean; data: KnowledgeFolderOption[] }>(
    `${base(knowledgeBaseId)}/folders/options`,
  )
}

export function searchKnowledgeFolderNodes(
  knowledgeBaseId: string,
  params: KnowledgeFolderListParams,
) {
  const query = new URLSearchParams()
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== null && value !== '') {
      query.set(key, String(value))
    }
  })
  return get<KnowledgeFolderNodePage>(`${base(knowledgeBaseId)}/search?${query.toString()}`)
}

export function searchAccessibleKnowledgeFolderNodes(params: {
  keyword: string
  page?: number
  page_size?: number
  knowledge_base_ids?: string
}) {
  const query = new URLSearchParams()
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== null && value !== '') {
      query.set(key, String(value))
    }
  })
  return get<KnowledgeFolderNodePage>(
    `/api/v1/custom/knowledge-folders/search?${query.toString()}`,
  )
}

export function createKnowledgeFolder(
  knowledgeBaseId: string,
  data: { parent_id?: string; name: string; description?: string; sort_order?: number },
) {
  return post<{ success: boolean; data: KnowledgeFolder }>(
    `${base(knowledgeBaseId)}/folders`,
    data,
  )
}

export function updateKnowledgeFolder(
  knowledgeBaseId: string,
  folderId: string,
  data: { parent_id?: string; name?: string; description?: string; sort_order?: number },
) {
  return put<{ success: boolean; data: KnowledgeFolder }>(
    `${base(knowledgeBaseId)}/folders/${encodeURIComponent(folderId)}`,
    data,
  )
}

export function deleteKnowledgeFolder(
  knowledgeBaseId: string,
  folderId: string,
) {
	return del<{ success: boolean; data: KnowledgeFolderDeleteOperation }>(
		`${base(knowledgeBaseId)}/folders/${encodeURIComponent(folderId)}`,
  )
}

export function getKnowledgeFolderDeleteOperation(
	knowledgeBaseId: string,
	operationId: string,
) {
	return get<{ success: boolean; data: KnowledgeFolderDeleteOperation }>(
		`${base(knowledgeBaseId)}/folder-delete-operations/${encodeURIComponent(operationId)}`,
	)
}

export function getKnowledgeBaseTaskStats(knowledgeBaseId: string) {
	return get<{ success: boolean; data: KnowledgeBaseTaskStats }>(
		`${base(knowledgeBaseId)}/task-stats`,
	)
}

export function moveKnowledgeDocumentsToFolder(
  knowledgeBaseId: string,
  knowledgeIds: string[],
  targetFolderId: string,
) {
  return put<{ success: boolean; data: { affected: number } }>(
    `${base(knowledgeBaseId)}/documents/locations`,
    { knowledge_ids: knowledgeIds, target_folder_id: targetFolderId },
  )
}

export function uploadKnowledgeFolderFile(
  knowledgeBaseId: string,
  data: {
    file: File
    folder_id?: string
    relative_path?: string
    fileName?: string
    tag_ids?: string[]
    process_config?: KnowledgeProcessOverrides | string
    [key: string]: any
  },
  onProgress?: (progressEvent: any) => void,
) {
  const formData = new FormData()
  Object.entries(data).forEach(([key, value]) => {
    if (value === undefined || value === null || value === '') return
    if (key === 'tag_ids' && Array.isArray(value)) {
      formData.append(key, value.join(','))
    } else if (key === 'process_config' && typeof value !== 'string') {
      formData.append(key, JSON.stringify(value))
    } else {
      formData.append(key, value as string | Blob)
    }
  })
  return postUpload(`${base(knowledgeBaseId)}/files`, formData, onProgress)
}

export function createKnowledgeFolderURL(
  knowledgeBaseId: string,
  data: {
    url: string
    folder_id?: string
    enable_multimodel?: boolean
    tag_ids?: string[]
    process_config?: KnowledgeProcessOverrides
    [key: string]: any
  },
) {
  return post(`${base(knowledgeBaseId)}/urls`, data)
}

export function createManualKnowledgeInFolder(
  knowledgeBaseId: string,
  data: {
    title: string
    content: string
    status: string
    folder_id?: string
    tag_ids?: string[]
    process_config?: KnowledgeProcessOverrides
  },
) {
  return post(`${base(knowledgeBaseId)}/manual`, data)
}
