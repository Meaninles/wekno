export type MobileKnowledgeAccess = 'manage' | 'edit' | 'view'
export type MobileKnowledgeGroup = 'personal' | 'shared'
export type MobileKnowledgeOrigin = 'personal' | 'tenant' | 'organization'

export interface KnowledgeBaseLike {
  id?: unknown
  name?: unknown
  creator_id?: unknown
  creator_name?: unknown
  [key: string]: unknown
}

export interface SharedKnowledgeBaseLike {
  knowledge_base?: KnowledgeBaseLike | null
  permission?: unknown
  org_name?: unknown
  share_id?: unknown
  shared_at?: unknown
  source_tenant_id?: unknown
  [key: string]: unknown
}

export interface MobileKnowledgeBase extends KnowledgeBaseLike {
  id: string
  name: string
  group: MobileKnowledgeGroup
  origin: MobileKnowledgeOrigin
  access: MobileKnowledgeAccess
  permissionLabel: string
  originLabel: string
  canEditContent: boolean
  canManage: boolean
  org_name?: string
  share_id?: string
  shared_at?: string
  source_tenant_id?: unknown
}

const ACCESS_RANK: Record<MobileKnowledgeAccess, number> = {
  manage: 3,
  edit: 2,
  view: 1,
}

function idOf(value: unknown): string {
  return value === undefined || value === null ? '' : String(value).trim()
}

function nameOf(value: unknown): string {
  const name = value === undefined || value === null ? '' : String(value).trim()
  return name || '未命名知识库'
}

function accessFromShare(permission: unknown): MobileKnowledgeAccess {
  if (permission === 'admin') return 'manage'
  if (permission === 'editor') return 'edit'
  return 'view'
}

function permissionLabel(access: MobileKnowledgeAccess): string {
  if (access === 'manage') return '可管理'
  if (access === 'edit') return '可编辑'
  return '仅查看'
}

function makeRow(
  kb: KnowledgeBaseLike,
  meta: {
    group: MobileKnowledgeGroup
    origin: MobileKnowledgeOrigin
    access: MobileKnowledgeAccess
    originLabel: string
    shared?: SharedKnowledgeBaseLike
  },
): MobileKnowledgeBase {
  const shared = meta.shared
  const orgName = idOf(shared?.org_name)
  return {
    ...kb,
    id: idOf(kb.id),
    name: nameOf(kb.name),
    group: meta.group,
    origin: meta.origin,
    access: meta.access,
    permissionLabel: permissionLabel(meta.access),
    originLabel: meta.originLabel,
    canEditContent: meta.access === 'manage' || meta.access === 'edit',
    canManage: meta.access === 'manage',
    ...(shared
      ? {
          permission: shared.permission,
          org_name: orgName || undefined,
          share_id: idOf(shared.share_id) || undefined,
          shared_at: idOf(shared.shared_at) || undefined,
          source_tenant_id: shared.source_tenant_id,
        }
      : {}),
  }
}

/**
 * Builds the two-section mobile catalogue.
 *
 * "个人" is deliberately creator-based, not tenant-based. Knowledge bases
 * created by teammates are shown in "共享" because they are usable resources
 * that the current user does not own. A local row wins when the same KB is also
 * shared back through an organization; repeated organization shares keep the
 * strongest effective permission returned by the server.
 */
export function buildMobileKnowledgeCatalog(
  localKnowledgeBases: readonly KnowledgeBaseLike[] = [],
  sharedKnowledgeBases: readonly SharedKnowledgeBaseLike[] = [],
  context: { currentUserId?: string; currentTenantRole?: string } = {},
): MobileKnowledgeBase[] {
  const currentUserId = idOf(context.currentUserId)
  const tenantCanManage = context.currentTenantRole === 'owner' || context.currentTenantRole === 'admin'
  const personal: MobileKnowledgeBase[] = []
  const tenantShared: MobileKnowledgeBase[] = []
  const localIds = new Set<string>()

  for (const kb of localKnowledgeBases) {
    const id = idOf(kb?.id)
    if (!id || localIds.has(id)) continue
    localIds.add(id)

    const creatorId = idOf(kb.creator_id)
    const isPersonal = !!creatorId && !!currentUserId && creatorId === currentUserId
    const access: MobileKnowledgeAccess = isPersonal || tenantCanManage ? 'manage' : 'view'
    const creatorName = nameOf(kb.creator_name)
    const row = makeRow(kb, {
      group: isPersonal ? 'personal' : 'shared',
      origin: isPersonal ? 'personal' : 'tenant',
      access,
      originLabel: isPersonal
        ? '我创建的'
        : idOf(kb.creator_name)
          ? `${creatorName} 创建`
          : '本空间成员创建',
    })
    if (isPersonal) personal.push(row)
    else tenantShared.push(row)
  }

  const bestSharedByKb = new Map<string, SharedKnowledgeBaseLike>()
  for (const shared of sharedKnowledgeBases) {
    const kb = shared?.knowledge_base
    const id = idOf(kb?.id)
    if (!kb || !id || localIds.has(id)) continue
    const existing = bestSharedByKb.get(id)
    if (
      !existing ||
      ACCESS_RANK[accessFromShare(shared.permission)] > ACCESS_RANK[accessFromShare(existing.permission)]
    ) {
      bestSharedByKb.set(id, shared)
    }
  }

  const organizationShared = [...bestSharedByKb.values()]
    .map((shared) => {
      const access = accessFromShare(shared.permission)
      const orgName = idOf(shared.org_name)
      return makeRow(shared.knowledge_base!, {
        group: 'shared',
        origin: 'organization',
        access,
        originLabel: orgName ? `${orgName} 共享` : '组织共享',
        shared,
      })
    })
    .sort((a, b) => ACCESS_RANK[b.access] - ACCESS_RANK[a.access])

  return [...personal, ...tenantShared, ...organizationShared]
}

/** Merge direct organization shares into the mobile chat KB selector. */
export function mergeMobileChatKnowledgeBases(
  localKnowledgeBases: readonly KnowledgeBaseLike[] = [],
  sharedKnowledgeBases: readonly SharedKnowledgeBaseLike[] = [],
): KnowledgeBaseLike[] {
  const result: KnowledgeBaseLike[] = []
  const localIds = new Set<string>()
  for (const kb of localKnowledgeBases) {
    const id = idOf(kb?.id)
    if (!id || localIds.has(id)) continue
    localIds.add(id)
    result.push(kb)
  }

  const bestSharedByKb = new Map<string, SharedKnowledgeBaseLike>()
  for (const shared of sharedKnowledgeBases) {
    const kb = shared?.knowledge_base
    const id = idOf(kb?.id)
    if (!kb || !id || localIds.has(id)) continue
    const existing = bestSharedByKb.get(id)
    if (
      !existing ||
      ACCESS_RANK[accessFromShare(shared.permission)] > ACCESS_RANK[accessFromShare(existing.permission)]
    ) {
      bestSharedByKb.set(id, shared)
    }
  }

  for (const shared of bestSharedByKb.values()) {
    result.push({
      ...shared.knowledge_base!,
      permission: shared.permission,
      org_name: idOf(shared.org_name) || undefined,
      share_id: idOf(shared.share_id) || undefined,
      shared_at: idOf(shared.shared_at) || undefined,
      source_tenant_id: shared.source_tenant_id,
      is_shared: true,
    })
  }
  return result
}
