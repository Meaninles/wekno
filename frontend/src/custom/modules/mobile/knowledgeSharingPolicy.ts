import type { KnowledgeBaseShare, Organization } from "@/api/organization";
import type { MobileKnowledgeBase } from "./knowledgeCatalog";

const ROLE_RANK: Record<string, number> = { owner: 4, admin: 3, editor: 2, viewer: 1 };

export interface MobileKnowledgeShareSpaceRow {
  id: string;
  name: string;
  roleLabel: string;
  owned?: boolean;
  shareId?: string;
  permission?: string;
  canRemove?: boolean;
}

export function organizationCanReceiveKnowledgeBase(org: Organization) {
  return org.is_owner === true || org.my_role === "admin" || org.my_role === "editor";
}

export function eligibleKnowledgeShareOrganizations(organizations: readonly Organization[]) {
  return organizations
    .filter(organizationCanReceiveKnowledgeBase)
    .slice()
    .sort((a, b) => {
      const aRole = a.is_owner ? "owner" : String(a.my_role || "viewer");
      const bRole = b.is_owner ? "owner" : String(b.my_role || "viewer");
      const rankDiff = (ROLE_RANK[bRole] || 0) - (ROLE_RANK[aRole] || 0);
      return rankDiff || a.name.localeCompare(b.name, "zh-CN");
    });
}

export function canShareKnowledgeBase(
  kb: MobileKnowledgeBase | null,
  context: { currentUserId?: string; currentTenantRole?: string },
) {
  if (!kb || kb.origin === "organization") return false;
  const creatorID = String(kb.creator_id || "");
  if (creatorID && creatorID === String(context.currentUserId || "")) return true;
  return context.currentTenantRole === "admin" || context.currentTenantRole === "owner";
}

export function canRemoveKnowledgeBaseShare(
  share: KnowledgeBaseShare,
  context: {
    currentUserId?: string;
    currentTenantId?: string | number;
    currentTenantRole?: string;
    organizations?: readonly Organization[];
  },
) {
  if (share.shared_by_user_id === String(context.currentUserId || "")) return true;
  if (
    Number(share.source_tenant_id) === Number(context.currentTenantId) &&
    (context.currentTenantRole === "admin" || context.currentTenantRole === "owner")
  ) return true;
  const target = (context.organizations || []).find((org) => org.id === share.organization_id);
  return target?.is_owner === true || target?.my_role === "admin";
}
