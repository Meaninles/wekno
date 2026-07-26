export type MentionItemType = 'kb' | 'file' | 'tag' | 'mcp' | 'skill';

export interface MentionItem {
  id: string;
  name: string;
  type: MentionItemType;
  group?: string;
  description?: string;
  kbType?: 'document' | 'faq';
  count?: number;
  kbName?: string;
  kbId?: string;
  orgName?: string;
  serviceId?: string;
  serviceName?: string;
  skillName?: string;
  isAgentConfigured?: boolean;
  isFolder?: boolean;
  isFolderBack?: boolean;
  folderPath?: string;
  folderId?: string;
  folderDocumentCount?: number;
}

export interface MentionRequestItem {
  id: string;
  name: string;
  type: MentionItemType;
  kb_type?: 'document' | 'faq';
  kb_id?: string;
  kb_name?: string;
  service_id?: string;
  skill_name?: string;
}
