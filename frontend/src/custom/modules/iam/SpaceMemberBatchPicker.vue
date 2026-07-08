<template>
  <div class="iam-space-member-picker">
    <div v-if="mode === 'member'" class="picker-field">
      <label>{{ $t('organization.addMember.selectMembers') }}</label>
      <t-input v-model="memberQuery" :placeholder="$t('organization.addMember.memberSearchPlaceholder')" clearable>
        <template #prefix-icon>
          <t-icon name="search" />
        </template>
      </t-input>
      <p class="field-hint">{{ $t('organization.addMember.memberSelectHint') }}</p>
      <t-loading :loading="memberLoading">
        <div class="candidate-list">
          <label v-for="candidate in memberResults" :key="candidateKey(candidate)"
            :class="['candidate-row', { 'is-disabled': candidateSelectionDisabled(candidate) }]">
            <t-checkbox :checked="isCandidateChecked(candidate)" :disabled="candidateSelectionDisabled(candidate)"
              @change="(checked: boolean) => toggleMember(candidate, checked)" />
            <div class="candidate-main">
              <span class="candidate-title">
                <span>{{ candidateDisplayName(candidate) }}</span>
                <t-tag v-if="candidate.already_selected" size="small" theme="default" variant="light">已选择</t-tag>
              </span>
              <span class="candidate-meta">{{ candidateMeta(candidate) }}</span>
            </div>
          </label>
          <div v-if="!memberLoading && memberResults.length === 0" class="candidate-empty">
            {{ $t('organization.addMember.noCandidates') }}
          </div>
        </div>
      </t-loading>
    </div>

    <div v-else class="picker-field">
      <label>{{ $t('organization.addMember.selectOrganization') }}</label>
      <t-loading :loading="orgTreeLoading">
        <div class="iam-org-tree">
          <t-tree v-model:expanded="expandedOrgIds" :data="orgTreeData" hover lazy
            :load="loadOrganizationChildren" :label="renderTreeLabel"
            :empty="$t('organization.addMember.noOrganizations')" />
        </div>
      </t-loading>
      <p class="field-hint">{{ organizationSelectionHint }}</p>
      <t-loading :loading="orgMemberLoading">
        <div v-if="hasAddableSelection" class="candidate-preview">
          <div class="candidate-preview-title">
            {{ $t('organization.addMember.organizationPreview', { count: selectedCandidates.length }) }}
          </div>
          <div v-for="candidate in selectedCandidates.slice(0, 6)" :key="candidateKey(candidate)" class="candidate-preview-row">
            <span>{{ candidateDisplayName(candidate) }}</span>
            <span>{{ candidateTenantLabel(candidate) }}</span>
          </div>
        </div>
      </t-loading>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref, resolveComponent, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  listIAMSpaceMemberCandidates,
  listIAMSpaceMemberOrganizations,
  type IAMSpaceMemberCandidate,
  type IAMSpaceMemberOrganization,
} from './api'

type PickerMode = 'member' | 'organization'
type IAMOrgTreeNode = {
  value: string
  label: string
  nodeType: 'organization' | 'user'
  externalId?: string
  iamExternalId?: string
  username?: string
  displayName?: string
  hasLocalUser?: boolean
  alreadySelected?: boolean
  selectionDisabled?: boolean
  children?: true | IAMOrgTreeNode[]
}

const props = defineProps<{
  spaceId: string
  mode: PickerMode
}>()

const emit = defineEmits<{
  (e: 'update:candidates', value: IAMSpaceMemberCandidate[]): void
  (e: 'update:duplicateCount', value: number): void
}>()

const { t } = useI18n()

const memberLoading = ref(false)
const memberQuery = ref('')
const memberResults = ref<IAMSpaceMemberCandidate[]>([])
const selectedMemberKeys = ref<string[]>([])
const orgTreeLoading = ref(false)
const orgTreeLoaded = ref(false)
const orgTreeData = ref<IAMOrgTreeNode[]>([])
const selectedOrgIds = ref<string[]>([])
const expandedOrgIds = ref<string[]>([])
const orgMemberLoading = ref(false)
const orgMemberResults = ref<IAMSpaceMemberCandidate[]>([])
const candidateCache = ref<Record<string, IAMSpaceMemberCandidate>>({})
const lockedTreeSelectedKeys = ref<string[]>([])

const selectedMemberCandidates = computed(() =>
  selectedMemberKeys.value
    .map((key) => candidateCache.value[key])
    .filter((candidate): candidate is IAMSpaceMemberCandidate => !!candidate)
)

const selectedOrgExternalIds = computed(() =>
  selectedRootOrgValues.value
    .map((value) => value.slice(4))
    .filter(Boolean)
)

const selectedRootOrgValues = computed(() => selectedLoadedRootOrgValues(selectedOrgIds.value, orgTreeData.value))

const selectedTreeUserCandidates = computed(() =>
  selectedOrgIds.value
    .filter((value) => !value.startsWith('org:'))
    .map((value) => candidateCache.value[value])
    .filter((candidate): candidate is IAMSpaceMemberCandidate => !!candidate && !candidateSelectionDisabled(candidate))
)

const sourceCandidates = computed(() =>
  (props.mode === 'member'
    ? selectedMemberCandidates.value
    : [...orgMemberResults.value, ...selectedTreeUserCandidates.value]
  ).filter((candidate) => !candidateSelectionDisabled(candidate))
)

const selectedCandidates = computed(() => {
  const byIdentity = new Map<string, IAMSpaceMemberCandidate>()
  for (const candidate of sourceCandidates.value) {
    const key = candidateDedupKey(candidate)
    if (!key || byIdentity.has(key)) continue
    byIdentity.set(key, candidate)
  }
  return Array.from(byIdentity.values())
})

const duplicateTenantCount = computed(() => Math.max(0, sourceCandidates.value.length - selectedCandidates.value.length))

const hasAddableSelection = computed(() =>
  selectedOrgExternalIds.value.length > 0 || selectedCandidates.value.length > 0
)

const organizationSelectionHint = computed(() => {
  if (!hasAddableSelection.value) return t('organization.addMember.organizationTreeHint')
  return t('organization.addMember.organizationSelectedHint', {
    orgCount: selectedOrgExternalIds.value.length,
    memberCount: selectedCandidates.value.length,
  })
})

function rememberCandidates(candidates: IAMSpaceMemberCandidate[]) {
  const next = { ...candidateCache.value }
  candidates.forEach((candidate) => {
    next[candidateKey(candidate)] = candidate
  })
  candidateCache.value = next
}

function candidateKey(candidate: IAMSpaceMemberCandidate) {
  if (candidate.tenant_id) return `tenant:${candidate.tenant_id}`
  if (candidate.iam_external_id) return `iam:${candidate.iam_external_id}`
  return `user:${candidate.user_id || candidate.username}`
}

function candidateDedupKey(candidate: IAMSpaceMemberCandidate) {
  if (candidate.tenant_id) return `tenant:${candidate.tenant_id}`
  if (candidate.iam_external_id) return `iam:${candidate.iam_external_id}`
  return candidateKey(candidate)
}

function toCandidateFromTreeUser(node: IAMSpaceMemberOrganization): IAMSpaceMemberCandidate {
  const iamExternalId = node.iam_external_id || node.external_id
  return {
    iam_external_id: iamExternalId,
    user_id: node.user_id || '',
    username: node.username || iamExternalId,
    display_name: node.display_name || node.name || node.username || iamExternalId,
    avatar: node.avatar || '',
    tenant_id: Number(node.tenant_id || 0),
    tenant_name: node.tenant_name || '',
    iam_organization_external_id: node.parent_external_id,
    has_local_user: node.has_local_user === true,
    access_enabled: node.access_enabled === true,
    already_selected: node.already_selected === true,
    selection_disabled: node.selection_disabled === true,
  }
}

let memberSearchTimer: ReturnType<typeof setTimeout> | null = null
function scheduleMemberSearch() {
  if (memberSearchTimer) clearTimeout(memberSearchTimer)
  memberSearchTimer = setTimeout(fetchMemberCandidates, 250)
}

let memberRequestSeq = 0
async function fetchMemberCandidates() {
  if (!props.spaceId) return
  const seq = ++memberRequestSeq
  memberLoading.value = true
  try {
    const res = await listIAMSpaceMemberCandidates({
      spaceId: props.spaceId,
      query: memberQuery.value.trim(),
      limit: 80,
    })
    if (seq !== memberRequestSeq) return
    memberResults.value = res.success && Array.isArray(res.data) ? res.data : []
    rememberCandidates(memberResults.value)
  } catch (error) {
    console.error('Failed to load IAM member candidates:', error)
    if (seq === memberRequestSeq) memberResults.value = []
  } finally {
    if (seq === memberRequestSeq) memberLoading.value = false
  }
}

async function fetchOrganizations() {
  if (!props.spaceId || orgTreeLoading.value || orgTreeLoaded.value) return
  orgTreeLoading.value = true
  try {
    const res = await listIAMSpaceMemberOrganizations({
      spaceId: props.spaceId,
      parentId: '',
      limit: 200,
      includeUsers: true,
    })
    if (res.success && Array.isArray(res.data)) {
      orgTreeData.value = res.data.map(toOrgTreeNode)
      expandedOrgIds.value = []
      orgTreeLoaded.value = true
    } else {
      orgTreeData.value = []
      orgTreeLoaded.value = false
    }
  } catch (error) {
    console.error('Failed to load IAM organization tree:', error)
    orgTreeData.value = []
    orgTreeLoaded.value = false
  } finally {
    orgTreeLoading.value = false
  }
}

function resetOrganizationTree() {
  orgTreeLoaded.value = false
  orgTreeData.value = []
  expandedOrgIds.value = []
  selectedOrgIds.value = []
  orgMemberResults.value = []
  lockedTreeSelectedKeys.value = []
}

function toOrgTreeNode(org: IAMSpaceMemberOrganization): IAMOrgTreeNode {
  if (org.node_type === 'user') {
    const candidate = toCandidateFromTreeUser(org)
    rememberCandidates([candidate])
    rememberLockedTreeSelection(candidate)
    const label = candidateDisplayName(candidate)
    return {
      value: candidateKey(candidate),
      label,
      nodeType: 'user',
      externalId: org.external_id,
      iamExternalId: candidate.iam_external_id,
      username: candidate.username,
      displayName: candidate.display_name,
      hasLocalUser: candidate.has_local_user === true,
      alreadySelected: candidate.already_selected === true,
      selectionDisabled: candidateSelectionDisabled(candidate),
    }
  }
  return {
    value: `org:${org.external_id}`,
    label: `${org.name || org.external_id} (${t('organization.addMember.organizationUserCount', { count: org.user_count || 0 })})`,
    nodeType: 'organization',
    externalId: org.external_id,
    children: org.has_children ? true : undefined,
  }
}

async function loadOrganizationChildren(input: any) {
  const data = treeNodeData(input)
  const parentId = nodeOrgExternalId(data?.value || input?.value)
  if (!props.spaceId || !parentId) return []
  const res = await listIAMSpaceMemberOrganizations({
    spaceId: props.spaceId,
    parentId,
    limit: 500,
    includeUsers: true,
  })
  const children = res.success && Array.isArray(res.data) ? res.data.map(toOrgTreeNode) : []
  if (data) data.children = children
  const next = new Set(selectedOrgIds.value)
  if (data && next.has(data.value)) {
    collectLoadedTreeValues(children).forEach((value) => next.add(value))
  }
  const normalized = normalizeLoadedCascadeSelection(Array.from(next), selectedOrgIds.value)
  if (!sameStringArray(normalized, selectedOrgIds.value)) selectedOrgIds.value = normalized
  return children
}

function nodeOrgExternalId(value?: string) {
  const raw = String(value || '').trim()
  if (!raw.startsWith('org:')) return ''
  return raw.slice(4)
}

function renderTreeLabel(_createElement: any, nodeModel?: any) {
  const data = treeNodeData(nodeModel)
  if (!data) return ''
  const Checkbox = resolveComponent('t-checkbox')
  const Tooltip = resolveComponent('t-tooltip')
  const Icon = resolveComponent('t-icon')
  const Tag = resolveComponent('t-tag')
  const children = [
    h('span', { class: 'iam-tree-node-text' }, data.label),
  ]
  if (data.nodeType === 'user' && data.alreadySelected) {
    children.push(h(Tag as any, { size: 'small', theme: 'default', variant: 'light', class: 'iam-tree-selected-tag' }, {
      default: () => '已选择',
    }) as any)
  }
  if (data.nodeType === 'user' && !data.hasLocalUser) {
    children.push(h(Tooltip as any, { content: '用户尚未登录过', placement: 'top' }, {
      default: () => h(Icon as any, { name: 'info-circle', size: '14px', class: 'iam-tree-mirror-icon' }),
    }) as any)
  }
  return h('span', { class: 'iam-tree-node-label' }, [
    h(Checkbox as any, {
      checked: isTreeNodeChecked(data),
      indeterminate: isTreeNodeIndeterminate(data),
      disabled: isTreeNodeSelectionDisabled(data),
      onChange: (checked: boolean) => toggleTreeNodeSelection(data, checked),
      onClick: (event: Event) => event.stopPropagation(),
    }, {
      default: () => children,
    }),
  ])
}

function treeNodeData(input: any): IAMOrgTreeNode | undefined {
  return input?.node?.data || input?.data || input
}

function candidateSelectionDisabled(candidate?: IAMSpaceMemberCandidate) {
  return candidate?.already_selected === true || candidate?.selection_disabled === true
}

function isCandidateChecked(candidate: IAMSpaceMemberCandidate) {
  return candidateSelectionDisabled(candidate) || selectedMemberKeys.value.includes(candidateKey(candidate))
}

function rememberLockedTreeSelection(candidate: IAMSpaceMemberCandidate) {
  if (!candidateSelectionDisabled(candidate)) return
  const key = candidateKey(candidate)
  if (!lockedTreeSelectedKeys.value.includes(key)) {
    lockedTreeSelectedKeys.value = [...lockedTreeSelectedKeys.value, key]
  }
  if (!selectedOrgIds.value.includes(key)) {
    selectedOrgIds.value = [...selectedOrgIds.value, key]
  }
}

function collectLoadedTreeValues(nodes?: true | IAMOrgTreeNode[], options: { includeDisabled?: boolean } = {}) {
  const values: string[] = []
  const visit = (items?: true | IAMOrgTreeNode[]) => {
    if (!Array.isArray(items)) return
    for (const item of items) {
      if (options.includeDisabled || item.selectionDisabled !== true) {
        values.push(item.value)
      }
      visit(item.children)
    }
  }
  visit(nodes)
  return values
}

function collectLoadedSubtreeValues(value: string, nodes: IAMOrgTreeNode[], options: { includeDisabled?: boolean } = {}) {
  const node = findLoadedTreeNode(value, nodes)
  if (!node || !Array.isArray(node.children)) return []
  return collectLoadedTreeValues(node.children, options)
}

function findLoadedTreeNode(value: string, nodes: IAMOrgTreeNode[]): IAMOrgTreeNode | undefined {
  for (const node of nodes) {
    if (node.value === value) return node
    if (Array.isArray(node.children)) {
      const child = findLoadedTreeNode(value, node.children)
      if (child) return child
    }
  }
  return undefined
}

function selectedLoadedRootOrgValues(values: string[], nodes: IAMOrgTreeNode[]) {
  const selected = new Set(values)
  const roots: string[] = []
  const seen = new Set<string>()
  const visit = (items: IAMOrgTreeNode[], selectedAncestor: boolean) => {
    for (const item of items) {
      const isOrg = item.nodeType === 'organization'
      const isSelected = selected.has(item.value)
      if (isOrg) {
        seen.add(item.value)
        if (isSelected && !selectedAncestor) roots.push(item.value)
      }
      if (Array.isArray(item.children)) {
        visit(item.children, selectedAncestor || (isOrg && isSelected))
      }
    }
  }
  visit(nodes, false)
  for (const value of values) {
    if (value.startsWith('org:') && !seen.has(value) && !roots.includes(value)) roots.push(value)
  }
  return roots
}

function isTreeNodeChecked(data: IAMOrgTreeNode) {
  return data.selectionDisabled === true || selectedOrgIds.value.includes(data.value)
}

function isTreeNodeIndeterminate(data: IAMOrgTreeNode) {
  if (data.nodeType !== 'organization' || isTreeNodeChecked(data)) return false
  const descendants = collectLoadedSubtreeValues(data.value, orgTreeData.value, { includeDisabled: true })
  if (descendants.length === 0) return false
  const selected = new Set(selectedOrgIds.value)
  return descendants.some((value) => selected.has(value))
}

function isTreeNodeSelectionDisabled(data: IAMOrgTreeNode) {
  return data.selectionDisabled === true
}

function toggleTreeNodeSelection(data: IAMOrgTreeNode, checked: boolean) {
  if (isTreeNodeSelectionDisabled(data)) return
  const next = new Set(selectedOrgIds.value.filter((value) => !lockedTreeSelectedKeys.value.includes(value)))
  const affectedValues = [
    data.value,
    ...(data.nodeType === 'organization' ? collectLoadedSubtreeValues(data.value, orgTreeData.value) : []),
  ]
  if (checked) {
    affectedValues.forEach((value) => next.add(value))
    selectedOrgIds.value = normalizeLoadedCascadeSelection(Array.from(next), selectedOrgIds.value)
    return
  }
  affectedValues.forEach((value) => next.delete(value))
  selectedOrgIds.value = normalizeLoadedCascadeSelection(Array.from(next), selectedOrgIds.value)
}

function normalizeLoadedCascadeSelection(ids: string[], _oldIds: string[]) {
  const next = new Set([...ids, ...lockedTreeSelectedKeys.value])
  syncLoadedParentSelection(orgTreeData.value, next)
  for (const lockedKey of lockedTreeSelectedKeys.value) next.add(lockedKey)
  return Array.from(next)
}

function syncLoadedParentSelection(nodes: IAMOrgTreeNode[], selected: Set<string>) {
  for (const node of nodes) {
    if (!Array.isArray(node.children)) continue
    syncLoadedParentSelection(node.children, selected)
    const childValues = node.children
      .filter((child) => child.selectionDisabled !== true)
      .map((child) => child.value)
      .filter(Boolean)
    if (childValues.length === 0) continue
    if (childValues.every((value) => selected.has(value))) {
      selected.add(node.value)
    } else {
      selected.delete(node.value)
    }
  }
}

function sameStringArray(a: string[], b: string[]) {
  if (a.length !== b.length) return false
  const set = new Set(a)
  return b.every((item) => set.has(item))
}

let orgMemberRequestSeq = 0
async function fetchOrgMemberCandidates() {
  const orgIds = selectedOrgExternalIds.value
  if (!props.spaceId || orgIds.length === 0) {
    orgMemberResults.value = []
    return
  }
  const seq = ++orgMemberRequestSeq
  orgMemberLoading.value = true
  try {
    const res = await listIAMSpaceMemberCandidates({
      spaceId: props.spaceId,
      iamOrgIds: orgIds,
      direct: false,
      limit: 10000,
    })
    if (seq !== orgMemberRequestSeq) return
    orgMemberResults.value = res.success && Array.isArray(res.data) ? res.data : []
    rememberCandidates(orgMemberResults.value)
  } catch (error) {
    console.error('Failed to load IAM organization member candidates:', error)
    if (seq === orgMemberRequestSeq) orgMemberResults.value = []
  } finally {
    if (seq === orgMemberRequestSeq) orgMemberLoading.value = false
  }
}

function toggleMember(candidate: IAMSpaceMemberCandidate, checked: boolean) {
  if (candidateSelectionDisabled(candidate)) return
  rememberCandidates([candidate])
  const key = candidateKey(candidate)
  if (checked) {
    if (!selectedMemberKeys.value.includes(key)) {
      selectedMemberKeys.value = [...selectedMemberKeys.value, key]
    }
  } else {
    selectedMemberKeys.value = selectedMemberKeys.value.filter((item) => item !== key)
  }
}

function candidateDisplayName(candidate: IAMSpaceMemberCandidate) {
  const displayName = candidate.display_name || candidate.username || candidate.user_id
  if (candidate.display_name && candidate.username && candidate.display_name !== candidate.username) {
    return `${displayName}（${candidate.username}）`
  }
  return displayName
}

function candidateMeta(candidate: IAMSpaceMemberCandidate) {
  const orgName = candidate.iam_organization_name || t('organization.addMember.noOrganizationName')
  const tenantName = candidateTenantLabel(candidate)
  return `${orgName} · ${tenantName}`
}

function candidateTenantLabel(candidate: IAMSpaceMemberCandidate) {
  if (candidate.tenant_id) return candidate.tenant_name || `tenant#${candidate.tenant_id}`
  return t('organization.addMember.pendingLoginTenant')
}

watch(memberQuery, () => {
  if (props.mode === 'member') scheduleMemberSearch()
})

let normalizingOrgSelection = false
watch(selectedOrgIds, (ids, oldIds) => {
  if (normalizingOrgSelection) {
    normalizingOrgSelection = false
    if (props.mode === 'organization') fetchOrgMemberCandidates()
    return
  }
  const normalized = normalizeLoadedCascadeSelection(ids, oldIds || [])
  if (!sameStringArray(normalized, ids)) {
    normalizingOrgSelection = true
    selectedOrgIds.value = normalized
    return
  }
  if (props.mode === 'organization') fetchOrgMemberCandidates()
})

watch(() => props.mode, (mode) => {
  if (mode === 'member') fetchMemberCandidates()
  if (mode === 'organization') fetchOrganizations()
})

watch(() => props.spaceId, (spaceId, oldSpaceId) => {
  if (spaceId === oldSpaceId) return
  resetOrganizationTree()
  memberResults.value = []
  selectedMemberKeys.value = []
  if (props.mode === 'member') fetchMemberCandidates()
  if (props.mode === 'organization') fetchOrganizations()
})

watch(selectedCandidates, (value) => emit('update:candidates', value), { immediate: true })
watch(duplicateTenantCount, (value) => emit('update:duplicateCount', value), { immediate: true })

onMounted(() => {
  if (props.mode === 'member') fetchMemberCandidates()
  if (props.mode === 'organization') fetchOrganizations()
})
</script>

<style scoped lang="less">
.picker-field {
  margin-bottom: 20px;

  label {
    display: block;
    margin-bottom: 8px;
    font-size: 14px;
    font-weight: 500;
    color: var(--td-text-color-primary);
  }
}

.field-hint {
  margin: 6px 0 0;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.candidate-list,
.iam-org-tree {
  margin-top: 10px;
  max-height: 260px;
  overflow-y: auto;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
}

.candidate-row {
  min-height: 38px;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border-bottom: 1px solid var(--td-component-stroke);

  &:last-child {
    border-bottom: 0;
  }
}

.candidate-row {
  cursor: pointer;

  &.is-disabled {
    cursor: not-allowed;
    color: var(--td-text-color-disabled);
    background: var(--td-bg-color-container-disabled);
  }
}

.candidate-main {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.candidate-title {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  color: var(--td-text-color-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.candidate-meta {
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.iam-tree-node-label {
  min-width: 0;
  display: inline-flex;
  align-items: center;
  gap: 4px;

  :deep(.t-checkbox) {
    max-width: 100%;
  }

  :deep(.t-checkbox__label) {
    min-width: 0;
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
}

.iam-tree-node-text {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}

.iam-tree-mirror-icon {
  color: var(--td-warning-color);
  flex: 0 0 auto;
}

.iam-tree-selected-tag {
  flex: 0 0 auto;
}

.candidate-empty {
  padding: 20px 12px;
  text-align: center;
  font-size: 13px;
  color: var(--td-text-color-secondary);
}

.iam-org-tree {
  padding: 6px 8px;
}

.candidate-preview {
  margin-top: 10px;
  padding: 10px 12px;
  border-radius: 6px;
  background: var(--td-bg-color-container);
}

.candidate-preview-title {
  font-size: 12px;
  color: var(--td-text-color-secondary);
  margin-bottom: 6px;
}

.candidate-preview-row {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
  line-height: 1.8;

  span {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
}
</style>
