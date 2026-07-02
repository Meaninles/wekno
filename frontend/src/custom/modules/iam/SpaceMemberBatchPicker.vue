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
          <label v-for="candidate in memberResults" :key="candidate.user_id" class="candidate-row">
            <t-checkbox :checked="selectedMemberUserIds.includes(candidate.user_id)"
              @change="(checked: boolean) => toggleMember(candidate, checked)" />
            <div class="candidate-main">
              <span class="candidate-title">{{ candidateDisplayName(candidate) }}</span>
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
          <t-tree v-model="selectedOrgIds" v-model:expanded="expandedOrgIds" :data="orgTreeData" checkable hover
            value-mode="all" :empty="$t('organization.addMember.noOrganizations')" />
        </div>
      </t-loading>
      <p class="field-hint">{{ organizationSelectionHint }}</p>
      <t-loading :loading="orgMemberLoading">
        <div v-if="selectedOrgIds.length > 0" class="candidate-preview">
          <div class="candidate-preview-title">
            {{ $t('organization.addMember.organizationPreview', { count: selectedCandidates.length }) }}
          </div>
          <div v-for="candidate in selectedCandidates.slice(0, 6)" :key="candidate.user_id" class="candidate-preview-row">
            <span>{{ candidateDisplayName(candidate) }}</span>
            <span>{{ candidate.tenant_name || `tenant#${candidate.tenant_id}` }}</span>
          </div>
        </div>
      </t-loading>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
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
  children?: IAMOrgTreeNode[]
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
const selectedMemberUserIds = ref<string[]>([])
const organizations = ref<IAMSpaceMemberOrganization[]>([])
const orgTreeLoading = ref(false)
const selectedOrgIds = ref<string[]>([])
const expandedOrgIds = ref<string[]>([])
const orgMemberLoading = ref(false)
const orgMemberResults = ref<IAMSpaceMemberCandidate[]>([])
const candidateCache = ref<Record<string, IAMSpaceMemberCandidate>>({})

const orgParentById = computed(() => {
  const parents = new Map<string, string>()
  for (const org of organizations.value) {
    parents.set(org.external_id, org.parent_external_id || '')
  }
  return parents
})

const effectiveSelectedOrgIds = computed(() => {
  const selected = new Set(selectedOrgIds.value)
  return selectedOrgIds.value.filter((id) => {
    const seen = new Set<string>()
    let parent = orgParentById.value.get(id) || ''
    while (parent) {
      if (selected.has(parent)) return false
      if (seen.has(parent)) break
      seen.add(parent)
      parent = orgParentById.value.get(parent) || ''
    }
    return true
  })
})

const orgTreeData = computed<IAMOrgTreeNode[]>(() => {
  const nodes = new Map<string, IAMOrgTreeNode>()
  for (const org of organizations.value) {
    nodes.set(org.external_id, {
      value: org.external_id,
      label: `${org.name || org.external_id} (${t('organization.addMember.organizationUserCount', { count: org.user_count || 0 })})`,
      children: [],
    })
  }

  const roots: IAMOrgTreeNode[] = []
  for (const org of organizations.value) {
    const node = nodes.get(org.external_id)
    if (!node) continue
    const parent = org.parent_external_id ? nodes.get(org.parent_external_id) : undefined
    if (parent) parent.children?.push(node)
    else roots.push(node)
  }

  const sortNodes = (items: IAMOrgTreeNode[]) => {
    items.sort((a, b) => a.label.localeCompare(b.label, 'zh-Hans-CN'))
    items.forEach((item) => {
      if (!item.children?.length) {
        delete item.children
        return
      }
      sortNodes(item.children)
    })
  }
  sortNodes(roots)
  return roots
})

const selectedMemberCandidates = computed(() =>
  selectedMemberUserIds.value
    .map((id) => candidateCache.value[id])
    .filter((candidate): candidate is IAMSpaceMemberCandidate => !!candidate)
)

const sourceCandidates = computed(() => (props.mode === 'member' ? selectedMemberCandidates.value : orgMemberResults.value))

const selectedCandidates = computed(() => {
  const byTenant = new Map<number, IAMSpaceMemberCandidate>()
  for (const candidate of sourceCandidates.value) {
    if (!candidate.tenant_id || byTenant.has(candidate.tenant_id)) continue
    byTenant.set(candidate.tenant_id, candidate)
  }
  return Array.from(byTenant.values())
})

const duplicateTenantCount = computed(() => Math.max(0, sourceCandidates.value.length - selectedCandidates.value.length))

const organizationSelectionHint = computed(() => {
  if (selectedOrgIds.value.length === 0) return t('organization.addMember.organizationTreeHint')
  return t('organization.addMember.organizationSelectedHint', {
    orgCount: effectiveSelectedOrgIds.value.length,
    memberCount: selectedCandidates.value.length,
  })
})

function rememberCandidates(candidates: IAMSpaceMemberCandidate[]) {
  const next = { ...candidateCache.value }
  candidates.forEach((candidate) => {
    if (candidate.user_id) next[candidate.user_id] = candidate
  })
  candidateCache.value = next
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
  if (!props.spaceId || orgTreeLoading.value || organizations.value.length > 0) return
  orgTreeLoading.value = true
  try {
    const res = await listIAMSpaceMemberOrganizations(props.spaceId, 5000)
    organizations.value = res.success && Array.isArray(res.data) ? res.data : []
    expandedOrgIds.value = []
  } catch (error) {
    console.error('Failed to load IAM organization tree:', error)
    organizations.value = []
  } finally {
    orgTreeLoading.value = false
  }
}

let orgMemberRequestSeq = 0
async function fetchOrgMemberCandidates() {
  const orgIds = effectiveSelectedOrgIds.value
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
  rememberCandidates([candidate])
  if (checked) {
    if (!selectedMemberUserIds.value.includes(candidate.user_id)) {
      selectedMemberUserIds.value = [...selectedMemberUserIds.value, candidate.user_id]
    }
  } else {
    selectedMemberUserIds.value = selectedMemberUserIds.value.filter((id) => id !== candidate.user_id)
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
  const tenantName = candidate.tenant_name || `tenant#${candidate.tenant_id}`
  return `${orgName} · ${tenantName}`
}

watch(memberQuery, () => {
  if (props.mode === 'member') scheduleMemberSearch()
})

watch(selectedOrgIds, () => {
  if (props.mode === 'organization') fetchOrgMemberCandidates()
})

watch(() => props.mode, (mode) => {
  if (mode === 'member') fetchMemberCandidates()
  if (mode === 'organization') fetchOrganizations()
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
}

.candidate-main {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.candidate-title {
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
