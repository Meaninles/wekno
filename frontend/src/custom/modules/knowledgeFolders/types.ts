export interface KnowledgeFolderStats {
  folder_id: string
  direct_child_folder_count: number
  subtree_document_count: number
  parse_pending_count: number
  parse_running_count: number
  enrichment_pending_task_count: number
  wiki_pending_task_count: number
  abnormal_document_count: number
  failed_document_count: number
  stats_updated_at?: string
}

export interface KnowledgeFolder {
  id: string
  tenant_id: number
  knowledge_base_id: string
  parent_id: string
  name: string
  description?: string
  path: string
  depth: number
  sort_order: number
  created_by?: string
  updated_by?: string
  created_at?: string
  updated_at?: string
  stats: KnowledgeFolderStats
}

export interface KnowledgeFolderBreadcrumb {
  id: string
  name: string
}

export interface KnowledgeFolderNode {
  node_type: 'folder' | 'document'
  knowledge_base_id?: string
  knowledge_base_name?: string
  folder?: KnowledgeFolder
  document?: Record<string, any>
}

export interface KnowledgeFolderNodePage {
  success: boolean
  data: KnowledgeFolderNode[]
  total: number
  page: number
  page_size: number
  current?: KnowledgeFolder
  breadcrumbs?: KnowledgeFolderBreadcrumb[]
}

export interface KnowledgeFolderOption {
  id: string
  parent_id: string
  name: string
  path: string
  depth: number
  stats: KnowledgeFolderStats
}

export interface KnowledgeFolderListParams {
  folder_id?: string
  page?: number
  page_size?: number
  tag_ids?: string
  keyword?: string
  file_type?: string
  parse_status?: string
  workflow_status?: string
  source?: string
  start_time?: string
  end_time?: string
}
