import type {
  KnowledgeManagementConfig,
  KnowledgeManagementPermissionSet,
} from '@/api/agent';
import type { ModelConfig } from '@/api/model';

export const DEFAULT_KNOWLEDGE_MANAGEMENT_PERMISSIONS: KnowledgeManagementPermissionSet = {
  add: true,
  modify: true,
  delete: true,
};

export const KNOWLEDGE_MANAGEMENT_TOOL_NAMES = [
  'kb_list_documents',
  'kb_add_document',
  'kb_replace_document',
  'kb_delete_document',
  'kb_mutation_status',
] as const;

export const KNOWLEDGE_MANAGEMENT_TOOL_OPTIONS = [
  { value: 'kb_list_documents', label: '文档清单', description: '按当前有效范围列出完整文档及元数据。', group: 'knowledge_management', fixed: true },
  { value: 'kb_add_document', label: '新增文档', description: '从本轮上传附件或新产物使用原生解析新增完整文档。', group: 'knowledge_management', fixed: true },
  { value: 'kb_replace_document', label: '替换文档（先增后删）', description: '确认新文档写入后，由智能体立即调用删除工具删除旧文档。', group: 'knowledge_management', fixed: true },
  { value: 'kb_delete_document', label: '删除文档', description: '删除明确范围内的完整文档及其派生内容。', group: 'knowledge_management', fixed: true, danger: true },
  { value: 'kb_mutation_status', label: '操作状态', description: '跟踪新增或替换直到终态。', group: 'knowledge_management', fixed: true },
] as const;

export const pickDefaultRerankModelID = (
  models: Pick<ModelConfig, 'id' | 'type' | 'is_default'>[],
): string => {
  const rerank = models.find((model) => model.type === 'Rerank' && model.is_default)
    || models.find((model) => model.type === 'Rerank');
  return rerank?.id || '';
};

export interface KnowledgeBaseManagerSelectableOption {
  value: string;
  label: string;
  disabled?: boolean;
  disabledReason?: string;
}

export const normalizeKnowledgeManagementPermission = (
  value?: Partial<KnowledgeManagementPermissionSet>,
): KnowledgeManagementPermissionSet => {
  let add = value?.add === true;
  let remove = value?.delete === true;
  if (value?.modify === true) {
    add = true;
    remove = true;
  }
  return { add, modify: add && remove, delete: remove };
};

export const normalizeKnowledgeManagementConfig = (
  raw: KnowledgeManagementConfig | undefined,
  knowledgeBaseIds: string[],
): KnowledgeManagementConfig => {
  const selected = new Set(knowledgeBaseIds);
  const overrides: Record<string, KnowledgeManagementPermissionSet> = {};
  for (const [kbId, permission] of Object.entries(raw?.knowledge_base_overrides || {})) {
    if (selected.has(kbId)) {
      overrides[kbId] = normalizeKnowledgeManagementPermission(permission);
    }
  }
  return {
    default_permissions: normalizeKnowledgeManagementPermission(
      raw?.default_permissions || DEFAULT_KNOWLEDGE_MANAGEMENT_PERMISSIONS,
    ),
    knowledge_base_overrides: overrides,
  };
};

export const syncKnowledgeManagementToolNames = (
  currentTools: string[],
  knowledgeBaseIds: string[],
  config: KnowledgeManagementConfig,
): string[] => {
  const managementTools = new Set<string>(KNOWLEDGE_MANAGEMENT_TOOL_NAMES);
  const result = currentTools.filter((tool) => !managementTools.has(tool));
  const effective = knowledgeBaseIds.map((kbId) => (
    config.knowledge_base_overrides[kbId] || config.default_permissions
  ));

  result.push('kb_list_documents', 'kb_mutation_status');
  if (effective.some((permission) => permission.add)) result.push('kb_add_document');
  if (effective.some((permission) => permission.modify)) result.push('kb_replace_document');
  if (effective.some((permission) => permission.delete)) result.push('kb_delete_document');
  return [...new Set(result)];
};

export const validateKnowledgeBaseManagerSelection = (
  knowledgeBaseIds: string[],
  options: KnowledgeBaseManagerSelectableOption[],
  config: KnowledgeManagementConfig,
): string | null => {
  if (knowledgeBaseIds.length === 0) {
    return '知识库管理智能体至少需要指定一个知识库';
  }

  const optionMap = new Map(options.map((option) => [option.value, option]));
  for (const kbId of knowledgeBaseIds) {
    const option = optionMap.get(kbId);
    if (!option) return '所选知识库不存在或已不可访问';
    if (option.disabled) {
      return `${option.label}：${option.disabledReason || '当前智能体类型不可使用'}`;
    }
    const permission = config.knowledge_base_overrides[kbId] || config.default_permissions;
    if (!permission.add && !permission.modify && !permission.delete) {
      return `${option.label} 至少需要启用新增、修改或删除中的一项权限`;
    }
  }
  return null;
};
