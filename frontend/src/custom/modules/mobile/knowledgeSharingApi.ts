import { get } from "@/utils/request";

export interface MobileKnowledgeShareTarget {
  id: string;
  name: string;
  description?: string;
  avatar?: string;
  my_role: "admin" | "editor" | "viewer";
  is_owner: boolean;
  share_id?: string;
  permission?: string;
  can_remove: boolean;
}

export interface MobileKnowledgeShareTargetPage {
  items: MobileKnowledgeShareTarget[];
  mode: "share" | "manage";
  page: number;
  page_size: number;
  total: number;
  has_more: boolean;
}

export function listMobileKnowledgeShareTargets(
  knowledgeBaseId: string,
  params: { q?: string; page?: number; page_size?: number } = {},
) {
  const query = new URLSearchParams();
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== null && value !== "") {
      query.set(key, String(value));
    }
  });
  const suffix = query.toString();
  return get<{ success: boolean; data: MobileKnowledgeShareTargetPage; message?: string }>(
    `/api/v1/custom/mobile-knowledge/knowledge-bases/${encodeURIComponent(knowledgeBaseId)}/share-targets${suffix ? `?${suffix}` : ""}`,
  );
}
