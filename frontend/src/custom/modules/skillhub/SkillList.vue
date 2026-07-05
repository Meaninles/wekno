<template>
  <div class="skill-list-container">
    <ListSpaceSidebar
      v-if="!authStore.isLiteMode"
      v-model="spaceSelection"
      collapsed-key="skill-list-space-sidebar"
      :count-all="allSkillsCount"
      :count-mine="mineSkills.length"
      :count-by-org="skillCountByOrg"
      :show-favorites="false"
      :show-recents="false"
    />

    <div class="skill-list-content">
      <div class="header" style="--wails-draggable: drag">
        <div class="header-title" style="--wails-draggable: drag">
          <div class="title-row" style="--wails-draggable: drag">
            <h2 style="--wails-draggable: drag">{{ $t('skill.title') }}</h2>
            <t-tooltip v-if="authStore.hasRole('contributor')" :content="createActionLabel" placement="bottom">
              <t-button
                variant="text"
                theme="default"
                size="small"
                class="header-action-btn"
                style="--wails-draggable: no-drag"
                @click="openCreateForActiveTab"
              >
                <template #icon><t-icon name="add" /></template>
              </t-button>
            </t-tooltip>
          </div>
          <p class="header-subtitle" style="--wails-draggable: drag">{{ $t('skill.subtitle') }}</p>
        </div>
      </div>

      <div class="skill-type-tabs" aria-label="技能类型">
        <button
          type="button"
          class="skill-type-tab"
          :class="{ active: activeSkillKind === 'professional' }"
          @click="activeSkillKind = 'professional'"
        >
          专业技能
          <span>{{ professionalSkills.length }}</span>
        </button>
        <button
          type="button"
          class="skill-type-tab"
          :class="{ active: activeSkillKind === 'lightweight' }"
          @click="activeSkillKind = 'lightweight'"
        >
          轻量技能
          <span>{{ displayedSkills.length }}</span>
        </button>
      </div>

      <div class="skill-list-main">
        <template v-if="activeSkillKind === 'lightweight'">
        <div v-if="loading && displayedSkills.length === 0" class="skill-card-wrap">
          <div v-for="n in 6" :key="`skill-skeleton-${n}`" class="skill-card skill-card-skeleton">
            <t-skeleton animation="gradient" :row-col="[{ width: '60%', height: '18px' }, { width: '100%', height: '14px' }, { width: '72%', height: '14px' }]" />
          </div>
        </div>

        <div v-else-if="displayedSkills.length === 0" class="empty-state">
          <t-icon name="lightbulb" class="empty-icon" />
          <span class="empty-title">{{ $t('skill.empty.title') }}</span>
          <span class="empty-desc">{{ $t('skill.empty.desc') }}</span>
          <t-button v-if="authStore.hasRole('contributor')" theme="primary" @click="openCreateDialog">
            {{ $t('skill.create') }}
          </t-button>
        </div>

        <div v-else class="skill-card-wrap">
          <div v-for="skill in displayedSkills" :key="skillCardKey(skill)" class="skill-card" @click="openSkillDialog(skill)">
            <div class="card-header">
              <div class="card-title-wrap">
                <span class="skill-icon"><t-icon name="lightbulb" /></span>
                <span class="card-title" :title="skill.name">{{ skill.name }}</span>
              </div>
              <t-popup
                v-if="canManageSkill(skill)"
                :visible="openMoreId === skillCardKey(skill)"
                trigger="hover"
                overlayClassName="card-more-popup"
                destroy-on-close
                placement="bottom-right"
                @update:visible="(v: boolean) => { if (!v) openMoreId = '' }"
              >
                <div class="more-wrap" :class="{ 'active-more': openMoreId === skillCardKey(skill) }" @click.stop="toggleMore(skillCardKey(skill))">
                  <img class="more-icon" src="@/assets/img/more.png" alt="" />
                </div>
                <template #content>
                  <div class="popup-menu">
                    <div class="popup-menu-item" @click.stop="openEditDialog(skill)">
                      <t-icon class="menu-icon" name="edit" />
                      <span>{{ $t('common.edit') }}</span>
                    </div>
                    <div class="popup-menu-item" @click.stop="openShareDialog(skill)">
                      <t-icon class="menu-icon" name="share" />
                      <span>{{ $t('skill.share.title') }}</span>
                    </div>
                    <div class="popup-menu-item delete" @click.stop="confirmDelete(skill)">
                      <t-icon class="menu-icon" name="delete" />
                      <span>{{ $t('common.delete') }}</span>
                    </div>
                  </div>
                </template>
              </t-popup>
            </div>

            <p class="card-description">{{ skill.description || $t('skill.noDescription') }}</p>

            <div class="card-meta">
              <t-tag v-if="skill.is_mine" size="small" theme="success" variant="light">{{ $t('skill.origin.mine') }}</t-tag>
              <t-tag v-else-if="skill.share_type === 'organization'" size="small" theme="primary" variant="light">
                {{ skill.organization_name || $t('skill.origin.space') }}
              </t-tag>
              <t-tag v-else-if="skill.share_type === 'user'" size="small" theme="default" variant="light">
                {{ $t('skill.origin.userShared') }}
              </t-tag>
              <t-tag :theme="skill.enabled ? 'success' : 'default'" size="small" variant="light">
                {{ skill.enabled ? $t('skill.enabled') : $t('skill.disabled') }}
              </t-tag>
            </div>

            <div class="card-bottom">
              <span>{{ formatTime(skill.updated_at || skill.created_at) }}</span>
              <t-button v-if="canManageSkill(skill)" variant="text" size="small" @click.stop="openShareDialog(skill)">
                <template #icon><t-icon name="share" /></template>
                {{ $t('skill.share.action') }}
              </t-button>
            </div>
          </div>
        </div>
        </template>

        <template v-else>
          <div v-if="loadingProfessional && professionalSkills.length === 0" class="skill-card-wrap">
            <div v-for="n in 4" :key="`professional-skill-skeleton-${n}`" class="skill-card skill-card-skeleton">
              <t-skeleton animation="gradient" :row-col="[{ width: '62%', height: '18px' }, { width: '100%', height: '14px' }, { width: '60%', height: '14px' }]" />
            </div>
          </div>

          <div v-else-if="professionalSkills.length === 0" class="empty-state">
            <t-icon name="tools" class="empty-icon" />
            <span class="empty-title">暂无专业技能</span>
            <span class="empty-desc">专业技能会以通用智能体专业技能形式加载。</span>
            <t-button v-if="authStore.hasRole('contributor')" theme="primary" @click="openProfessionalImportDialog">
              新增专业技能
            </t-button>
          </div>

          <div v-else class="skill-card-wrap">
            <div v-for="skill in professionalSkills" :key="professionalCardKey(skill)" class="skill-card professional-card" @click="openProfessionalViewDialog(skill)">
              <div class="card-header">
                <div class="card-title-wrap">
                  <span class="skill-icon professional-icon"><t-icon name="tools" /></span>
                  <span class="card-title" :title="skill.name">{{ skill.name }}</span>
                </div>
                <t-popup
                  v-if="canManageProfessionalSkill(skill)"
                  :visible="openMoreId === professionalCardKey(skill)"
                  trigger="hover"
                  overlayClassName="card-more-popup"
                  destroy-on-close
                  placement="bottom-right"
                  @update:visible="(v: boolean) => { if (!v) openMoreId = '' }"
                >
                  <div class="more-wrap" :class="{ 'active-more': openMoreId === professionalCardKey(skill) }" @click.stop="toggleMore(professionalCardKey(skill))">
                    <img class="more-icon" src="@/assets/img/more.png" alt="" />
                  </div>
                  <template #content>
                    <div class="popup-menu">
                      <div class="popup-menu-item" @click.stop="openProfessionalEditDialog(skill)">
                        <t-icon class="menu-icon" name="edit" />
                        <span>{{ $t('common.edit') }}</span>
                      </div>
                      <div v-if="skill.can_download" class="popup-menu-item" @click.stop="downloadProfessional(skill)">
                        <t-icon class="menu-icon" name="download" />
                        <span>下载</span>
                      </div>
                      <div class="popup-menu-item" @click.stop="openProfessionalShareDialog(skill)">
                        <t-icon class="menu-icon" name="share" />
                        <span>{{ $t('skill.share.title') }}</span>
                      </div>
                      <div class="popup-menu-item delete" @click.stop="confirmDeleteProfessional(skill)">
                        <t-icon class="menu-icon" name="delete" />
                        <span>{{ $t('common.delete') }}</span>
                      </div>
                    </div>
                  </template>
                </t-popup>
              </div>

              <p class="card-description">{{ skill.description || $t('skill.noDescription') }}</p>

              <div class="card-meta">
                <t-tag theme="primary" size="small" variant="light">专业技能</t-tag>
                <t-tag size="small" variant="light">{{ skill.file_count || 0 }} 个文件</t-tag>
                <t-tag v-if="skill.system_reserved" size="small" theme="warning" variant="light">系统保留</t-tag>
                <t-tag v-if="skill.managed" size="small" theme="success" variant="light">本地导入</t-tag>
                <t-tag v-if="skill.share_type === 'organization'" size="small" theme="primary" variant="light">
                  {{ skill.organization_name || $t('skill.origin.space') }}
                </t-tag>
                <t-tag v-else-if="skill.share_type === 'user'" size="small" theme="default" variant="light">
                  {{ $t('skill.origin.userShared') }}
                </t-tag>
              </div>

              <div class="card-bottom">
                <span>{{ formatTime(skill.updated_at) }}</span>
                <t-button v-if="canManageProfessionalSkill(skill)" variant="text" size="small" @click.stop="openProfessionalShareDialog(skill)">
                  <template #icon><t-icon name="share" /></template>
                  {{ $t('skill.share.action') }}
                </t-button>
              </div>
            </div>
          </div>
        </template>
      </div>
    </div>

    <t-dialog v-model:visible="editorVisible" :header="editorHeader" width="820px" :footer="false" @close="resetEditor">
      <div class="editor-layout">
        <div class="editor-tip">
          <t-icon name="info-circle" />
          <span>{{ editorReadOnly ? '当前轻量技能为只读状态。' : '轻量技能会作为预置提示词注入上下文，不要求 SKILL.md 格式。' }}</span>
          <t-tooltip :content="lightweightRuleText" placement="bottom">
            <button type="button" class="rule-help-button" aria-label="轻量技能校验规则">
              <t-icon name="help-circle" />
            </button>
          </t-tooltip>
        </div>
        <t-form :data="editorForm" label-align="top">
          <t-form-item :label="$t('skill.editor.enabled')" name="enabled">
            <t-switch v-model="editorForm.enabled" :disabled="editorReadOnly" />
          </t-form-item>
          <t-form-item label="技能名称" name="name">
            <t-input
              v-model="editorForm.name"
              :maxlength="LIGHTWEIGHT_NAME_MAX_CHARS"
              placeholder="例如：报告摘要助手"
              :readonly="editorReadOnly"
            />
          </t-form-item>
          <t-form-item label="技能描述" name="description">
            <t-textarea
              v-model="editorForm.description"
              :maxlength="LIGHTWEIGHT_DESCRIPTION_MAX_CHARS"
              :autosize="{ minRows: 3, maxRows: 5 }"
              placeholder="简要说明这个预置提示词适合什么场景。"
              :readonly="editorReadOnly"
            />
          </t-form-item>
          <t-form-item label="预置提示词" name="instructions">
            <t-textarea
              v-model="editorForm.instructions"
              class="skill-md-editor"
              :maxlength="LIGHTWEIGHT_PROMPT_MAX_CHARS"
              :autosize="{ minRows: 14, maxRows: 24 }"
              placeholder="写入希望智能体遵循的角色、步骤、边界或输出格式。"
              :readonly="editorReadOnly"
            />
          </t-form-item>
        </t-form>
        <div class="dialog-actions">
          <t-button theme="default" @click="editorVisible = false">
            {{ editorReadOnly ? $t('common.close') : $t('common.cancel') }}
          </t-button>
          <t-button v-if="!editorReadOnly" theme="primary" :loading="saving" @click="saveSkill">{{ $t('common.save') }}</t-button>
        </div>
      </div>
    </t-dialog>

    <t-dialog v-model:visible="professionalImportVisible" :header="professionalEditorHeader" width="640px" :footer="false" @close="resetProfessionalImport">
      <div class="professional-import-dialog">
        <div class="editor-tip">
          <t-icon name="info-circle" />
          <span>{{ professionalImportTip }}</span>
          <t-tooltip :content="professionalRuleText" placement="bottom">
            <button type="button" class="rule-help-button" aria-label="专业技能校验规则">
              <t-icon name="help-circle" />
            </button>
          </t-tooltip>
        </div>
        <t-form :data="professionalImportForm" label-align="top">
          <t-form-item label="运行标识（可选）" name="name">
            <t-input v-model="professionalImportForm.name" placeholder="例如：word-docx；留空自动从包内 slug 推导" />
            <p class="form-hint">前端展示、内部目录和选择值都使用这个 slug；只允许小写字母、数字和连字符。</p>
          </t-form-item>
          <t-form-item label="技能描述（可选）" name="description">
            <t-textarea
              v-model="professionalImportForm.description"
              :maxlength="LIGHTWEIGHT_DESCRIPTION_MAX_CHARS"
              :autosize="{ minRows: 3, maxRows: 5 }"
              placeholder="仅用于技能列表展示，不影响 SKILL.md 触发规则。"
            />
          </t-form-item>
          <t-form-item label="技能包" name="package">
            <label class="package-drop">
              <input type="file" accept=".zip,.tar,.gz,.tgz,.7z,.rar" @change="handleProfessionalPackageChange" />
              <t-icon name="upload" />
              <span>{{ professionalPackageLabel }}</span>
              <small>支持 .zip、.tar、.tar.gz、.tgz、.7z、.rar；最大 30MB，解压限时 1 分钟；解压后总文件不超过 50MB，单文件不超过 10MB。</small>
              <small>压缩包内必须包含且只包含一个 SKILL.md；原始技能文件会保持不变，运行时按内部 slug 临时适配。</small>
            </label>
          </t-form-item>
        </t-form>
        <div class="dialog-actions">
          <t-button theme="default" @click="professionalImportVisible = false">{{ $t('common.cancel') }}</t-button>
          <t-button theme="primary" :loading="savingProfessional" @click="saveProfessionalSkill">{{ $t('common.save') }}</t-button>
        </div>
      </div>
    </t-dialog>

    <t-dialog v-model:visible="professionalDetailVisible" header="专业技能" width="560px" :footer="false">
      <div v-if="currentProfessionalSkill" class="professional-detail">
        <div class="share-skill-name">
          <t-icon name="tools" />
          <span>{{ currentProfessionalSkill.name }}</span>
        </div>
        <p>{{ currentProfessionalSkill.description }}</p>
        <div class="card-meta">
          <t-tag theme="primary" size="small" variant="light">专业技能</t-tag>
          <t-tag size="small" variant="light">{{ currentProfessionalSkill.file_count || 0 }} 个文件</t-tag>
          <t-tag v-if="currentProfessionalSkill.system_reserved" size="small" theme="warning" variant="light">系统保留</t-tag>
          <t-tag v-if="currentProfessionalSkill.managed" size="small" theme="success" variant="light">本地导入</t-tag>
        </div>
        <div class="dialog-actions">
          <t-button v-if="canManageProfessionalSkill(currentProfessionalSkill)" theme="default" @click="openProfessionalEditDialog(currentProfessionalSkill)">
            {{ $t('common.edit') }}
          </t-button>
          <t-button v-if="canManageProfessionalSkill(currentProfessionalSkill) && currentProfessionalSkill.can_download" theme="default" @click="downloadProfessional(currentProfessionalSkill)">
            下载
          </t-button>
          <t-button v-if="canManageProfessionalSkill(currentProfessionalSkill)" theme="default" @click="openProfessionalShareDialog(currentProfessionalSkill)">
            {{ $t('skill.share.title') }}
          </t-button>
          <t-button v-if="canManageProfessionalSkill(currentProfessionalSkill)" theme="danger" variant="outline" @click="confirmDeleteProfessional(currentProfessionalSkill)">
            {{ $t('common.delete') }}
          </t-button>
          <t-button theme="primary" @click="professionalDetailVisible = false">{{ $t('common.close') }}</t-button>
        </div>
      </div>
    </t-dialog>

    <t-dialog v-model:visible="shareVisible" :header="$t('skill.share.title')" width="640px" :footer="false" @close="resetShareDialog">
      <div v-if="currentShareSkillName" class="share-dialog">
        <div class="share-skill-name">
          <t-icon :name="currentShareSkillIcon" />
          <span>{{ currentShareSkillName }}</span>
        </div>

        <t-tabs v-model="shareTab">
          <t-tab-panel value="organization" :label="$t('skill.share.spaceTab')">
            <div class="share-form">
              <t-select v-model="orgShareForm.organization_id" :placeholder="$t('organization.share.selectOrgPlaceholder')" :loading="loadingOrganizations">
                <t-option v-for="org in availableOrganizations" :key="org.id" :value="org.id" :label="org.name" />
              </t-select>
              <t-radio-group v-model="orgShareForm.permission">
                <t-radio-button value="viewer">{{ $t('organization.share.permissionReadonly') }}</t-radio-button>
                <t-radio-button value="editor">{{ $t('organization.share.permissionEditable') }}</t-radio-button>
              </t-radio-group>
              <t-button theme="primary" :loading="sharing" @click="shareToOrganization">{{ $t('skill.share.action') }}</t-button>
            </div>

            <div class="share-list">
              <div class="share-list-title">{{ $t('skill.share.sharedSpaces') }}</div>
              <div v-if="loadingShares" class="share-empty"><t-loading size="small" /></div>
              <div v-else-if="organizationShares.length === 0" class="share-empty">{{ $t('organization.share.noShares') }}</div>
              <div v-else v-for="share in organizationShares" :key="share.share_id" class="share-row">
                <div class="share-row-main">
                  <SpaceAvatar :name="share.organization_name || ''" :avatar="orgStore.organizations.find(o => o.id === share.organization_id)?.avatar" size="small" />
                  <span>{{ share.organization_name || share.organization_id }}</span>
                  <t-tag size="small" :theme="share.permission === 'editor' ? 'warning' : 'default'">
                    {{ share.permission === 'editor' ? $t('organization.share.permissionEditable') : $t('organization.share.permissionReadonly') }}
                  </t-tag>
                </div>
                <t-button variant="text" theme="danger" size="small" @click="removeOrganizationShare(share)">
                  <t-icon name="close" />
                </t-button>
              </div>
            </div>
          </t-tab-panel>

          <t-tab-panel value="user" :label="$t('skill.share.userTab')">
            <div class="share-form share-form-user">
              <div class="user-search-row">
                <t-input v-model="userSearchQuery" :placeholder="$t('skill.share.userSearchPlaceholder')" @enter="searchUsers" />
                <t-button theme="default" :loading="searchingUsers" @click="searchUsers">{{ $t('common.search') }}</t-button>
              </div>
              <div v-if="userCandidates.length > 0" class="user-candidates">
                <button
                  v-for="user in userCandidates"
                  :key="user.id"
                  type="button"
                  class="user-candidate"
                  :class="{ selected: userShareForm.user_id === user.id }"
                  @click="selectShareUser(user)"
                >
                  <span>{{ user.username }}</span>
                  <small>#{{ user.tenant_id }}</small>
                </button>
              </div>
              <t-radio-group v-model="userShareForm.permission">
                <t-radio-button value="viewer">{{ $t('organization.share.permissionReadonly') }}</t-radio-button>
                <t-radio-button value="editor">{{ $t('organization.share.permissionEditable') }}</t-radio-button>
              </t-radio-group>
              <t-button theme="primary" :disabled="!userShareForm.user_id" :loading="sharing" @click="shareToUser">
                {{ $t('skill.share.action') }}
              </t-button>
            </div>

            <div class="share-list">
              <div class="share-list-title">{{ $t('skill.share.sharedUsers') }}</div>
              <div v-if="loadingShares" class="share-empty"><t-loading size="small" /></div>
              <div v-else-if="userShares.length === 0" class="share-empty">{{ $t('skill.share.noUserShares') }}</div>
              <div v-else v-for="share in userShares" :key="share.share_id" class="share-row">
                <div class="share-row-main">
                  <t-icon name="user" />
                  <span>{{ share.target_username || share.target_user_id }}</span>
                  <t-tag size="small" :theme="share.permission === 'editor' ? 'warning' : 'default'">
                    {{ share.permission === 'editor' ? $t('organization.share.permissionEditable') : $t('organization.share.permissionReadonly') }}
                  </t-tag>
                </div>
                <t-button variant="text" theme="danger" size="small" @click="removeUserShare(share)">
                  <t-icon name="close" />
                </t-button>
              </div>
            </div>
          </t-tab-panel>
        </t-tabs>
      </div>
    </t-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { MessagePlugin } from 'tdesign-vue-next'
import ListSpaceSidebar from '@/components/ListSpaceSidebar.vue'
import SpaceAvatar from '@/components/SpaceAvatar.vue'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationStore } from '@/stores/organization'
import { useEditorResourcesStore } from '@/stores/editorResources'
import { useConfirmDelete } from '@/components/settings/useConfirmDelete'
import {
  createManagedSkill,
  deleteManagedSkill,
  deleteProfessionalSkill,
  downloadProfessionalSkill,
  importProfessionalSkill,
  listManagedProfessionalSkills,
  listManagedSkills,
  listProfessionalSkillShares,
  listSkillShares,
  listSkillsByOrganization,
  removeProfessionalSkillOrganizationShare,
  removeProfessionalSkillUserShare,
  removeSkillOrganizationShare,
  removeSkillUserShare,
  searchSkillShareUsers,
  shareProfessionalSkillToOrganization,
  shareProfessionalSkillToUser,
  shareSkillToOrganization,
  shareSkillToUser,
  updateManagedSkill,
  updateProfessionalSkill,
  type ManagedSkill,
  type ManagedProfessionalSkill,
  type SkillPayload,
  type SkillPermission,
  type SkillShareUser,
} from '@/api/skill'

type ShareListItem = ManagedSkill | ManagedProfessionalSkill

const { t } = useI18n()
const showConfirmDelete = useConfirmDelete()
const authStore = useAuthStore()
const orgStore = useOrganizationStore()
const editorResources = useEditorResourcesStore()

const loading = ref(false)
const loadingOrganizations = ref(false)
const loadingProfessional = ref(false)
const skills = ref<ManagedSkill[]>([])
const professionalSkills = ref<ManagedProfessionalSkill[]>([])
const orgSkillMap = ref<Record<string, ManagedSkill[]>>({})
const spaceSelection = ref('all')
const activeSkillKind = ref<'lightweight' | 'professional'>('professional')
const openMoreId = ref('')

const editorVisible = ref(false)
const editorMode = ref<'create' | 'edit' | 'view'>('create')
const currentSkill = ref<ManagedSkill | null>(null)
const saving = ref(false)
const editorForm = ref({ name: '', description: '', instructions: '', enabled: true })

const professionalImportVisible = ref(false)
const professionalDetailVisible = ref(false)
const professionalEditorMode = ref<'create' | 'edit'>('create')
const savingProfessional = ref(false)
const professionalPackageFile = ref<File | null>(null)
const currentProfessionalSkill = ref<ManagedProfessionalSkill | null>(null)
const professionalImportForm = ref({
  name: '',
  description: '',
})

const shareVisible = ref(false)
const shareSkillKind = ref<'lightweight' | 'professional'>('lightweight')
const shareTab = ref<'organization' | 'user'>('organization')
const loadingShares = ref(false)
const sharing = ref(false)
const searchingUsers = ref(false)
const organizationShares = ref<ShareListItem[]>([])
const userShares = ref<ShareListItem[]>([])
const userCandidates = ref<SkillShareUser[]>([])
const userSearchQuery = ref('')
const orgShareForm = ref<{ organization_id: string; permission: SkillPermission }>({ organization_id: '', permission: 'viewer' })
const userShareForm = ref<{ user_id: string; username: string; permission: SkillPermission }>({ user_id: '', username: '', permission: 'viewer' })

const RESERVED_SCOPES = new Set(['all', 'mine'])
const PROFESSIONAL_PACKAGE_MAX_BYTES = 30 * 1024 * 1024
const PROFESSIONAL_NAME_PATTERN = /^[\p{L}\p{N}-]{1,64}$/u
const LIGHTWEIGHT_NAME_MAX_CHARS = 64
const LIGHTWEIGHT_DESCRIPTION_MAX_CHARS = 1024
const LIGHTWEIGHT_PROMPT_MAX_CHARS = 20000
const lightweightRuleText = '名称 1-64 字且不含换行；提示词必填，最多 2 万字；描述可选。'
const professionalRuleText = '名称统一使用包内 slug；缺失 slug 时从合法 name、目录名或包名推导；包不超过 30MB 且需含唯一 SKILL.md。'
const mineSkills = computed(() => skills.value.filter((skill) => skill.is_mine))
const allSkillsCount = computed(() => skills.value.length)

const skillCountByOrg = computed<Record<string, number>>(() => {
  const out: Record<string, number> = {}
  Object.entries(orgSkillMap.value).forEach(([orgId, list]) => {
    out[orgId] = list.length
  })
  return out
})

const displayedSkills = computed(() => {
  if (spaceSelection.value === 'mine') return mineSkills.value
  if (!RESERVED_SCOPES.has(spaceSelection.value)) {
    return orgSkillMap.value[spaceSelection.value] || []
  }
  return skills.value
})

const availableOrganizations = computed(() => {
  const sharedOrgIds = new Set(organizationShares.value.map((share) => share.organization_id))
  return orgStore.organizations.filter((org) =>
    !sharedOrgIds.has(org.id) &&
    (org.is_owner === true || org.my_role === 'admin' || org.my_role === 'editor')
  )
})

const editorReadOnly = computed(() => editorMode.value === 'view')
const editorHeader = computed(() => {
  if (editorMode.value === 'create') return t('skill.editor.createTitle')
  if (editorMode.value === 'view') return t('skill.editor.viewTitle')
  return t('skill.editor.editTitle')
})
const createActionLabel = computed(() => activeSkillKind.value === 'professional' ? '新增专业技能' : t('skill.create'))
const professionalEditorHeader = computed(() => professionalEditorMode.value === 'edit' ? '编辑专业技能' : '新增专业技能')
const professionalImportTip = computed(() =>
  professionalEditorMode.value === 'edit'
    ? '可更新 slug 和展示描述；上传新压缩包时会替换当前技能目录，并保留原始包用于下载。'
    : '上传时会立即解压、校验并保存为专业技能目录；原始技能文件保持不变，名称优先使用包内 slug。',
)
const professionalPackageLabel = computed(() => {
  if (professionalPackageFile.value) return professionalPackageFile.value.name
  return professionalEditorMode.value === 'edit' ? '不替换技能包' : '选择压缩包'
})
const currentShareSkillName = computed(() =>
  shareSkillKind.value === 'professional' ? currentProfessionalSkill.value?.name : currentSkill.value?.name,
)
const currentShareSkillIcon = computed(() => shareSkillKind.value === 'professional' ? 'tools' : 'lightbulb')

function skillCardKey(skill: ManagedSkill) {
  return skill.share_id ? `${skill.id}-${skill.share_type}-${skill.share_id}` : skill.id
}

function professionalCardKey(skill: ManagedProfessionalSkill) {
  return skill.share_id ? `${skill.id || skill.name}-${skill.share_type}-${skill.share_id}` : (skill.id || skill.name)
}

function canManageSkill(skill: ManagedSkill) {
  return skill.is_mine === true && (authStore.hasRole('contributor') || authStore.hasRole('admin'))
}

function canManageProfessionalSkill(skill: ManagedProfessionalSkill | null) {
  return skill?.can_manage === true && (authStore.hasRole('contributor') || authStore.hasRole('admin'))
}

function toggleMore(id: string) {
  openMoreId.value = openMoreId.value === id ? '' : id
}

function formatTime(raw?: string) {
  if (!raw) return ''
  try {
    return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(raw))
  } catch {
    return raw
  }
}

async function loadSkills() {
  loading.value = true
  try {
    const res = await listManagedSkills()
    skills.value = Array.isArray(res?.data) ? res.data : []
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('skill.messages.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function loadProfessionalSkills() {
  loadingProfessional.value = true
  try {
    const res = await listManagedProfessionalSkills()
    professionalSkills.value = Array.isArray(res?.data) ? res.data : []
  } catch (e: any) {
    MessagePlugin.error(e?.message || '专业技能加载失败')
  } finally {
    loadingProfessional.value = false
  }
}

async function loadOrganizations() {
  loadingOrganizations.value = true
  try {
    await orgStore.fetchOrganizations({ force: true })
  } finally {
    loadingOrganizations.value = false
  }
}

async function loadOrgSkill(orgId: string) {
  try {
    const res = await listSkillsByOrganization(orgId)
    orgSkillMap.value = { ...orgSkillMap.value, [orgId]: Array.isArray(res?.data) ? res.data : [] }
  } catch (e) {
    console.error('[SkillList] failed to load organization skills', e)
  }
}

async function loadOrgSkillCounts() {
  await loadOrganizations()
  const orgIds = orgStore.organizations.map((org) => org.id)
  await Promise.all(orgIds.map((orgId) => loadOrgSkill(orgId)))
}

function openCreateForActiveTab() {
  if (activeSkillKind.value === 'professional') {
    openProfessionalImportDialog()
    return
  }
  openCreateDialog()
}

function openCreateDialog() {
  editorMode.value = 'create'
  currentSkill.value = null
  editorForm.value = { name: '', description: '', instructions: '', enabled: true }
  editorVisible.value = true
}

function openProfessionalImportDialog() {
  resetProfessionalImport()
  professionalEditorMode.value = 'create'
  professionalImportVisible.value = true
}

function openProfessionalViewDialog(skill: ManagedProfessionalSkill) {
  currentProfessionalSkill.value = skill
  professionalDetailVisible.value = true
}

function openProfessionalEditDialog(skill: ManagedProfessionalSkill) {
  if (!canManageProfessionalSkill(skill)) return
  professionalDetailVisible.value = false
  professionalEditorMode.value = 'edit'
  currentProfessionalSkill.value = skill
  professionalPackageFile.value = null
  professionalImportForm.value = {
    name: skill.name,
    description: skill.description || '',
  }
  professionalImportVisible.value = true
  openMoreId.value = ''
}

function resetProfessionalImport() {
  savingProfessional.value = false
  professionalPackageFile.value = null
  professionalImportForm.value = {
    name: '',
    description: '',
  }
}

function isValidProfessionalName(name: string) {
  return PROFESSIONAL_NAME_PATTERN.test(name)
}

function professionalNameFromPackage(filename: string) {
  const raw = filename.split(/[\\/]/).pop() || ''
  const lower = raw.toLowerCase()
  const extensions = ['.tar.gz', '.tar.bz2', '.tar.xz', '.tgz', '.zip', '.tar', '.7z', '.rar', '.gz']
  let base = raw
  for (const ext of extensions) {
    if (lower.endsWith(ext)) {
      base = raw.slice(0, -ext.length)
      break
    }
  }
  if (base === raw) {
    const dotIndex = raw.lastIndexOf('.')
    base = dotIndex > 0 ? raw.slice(0, dotIndex) : raw
  }
  return slugFromText(base)
}

function slugFromText(value: string) {
  return value
    .toLowerCase()
    .replace(/\s*\(\d+\)\s*$/g, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .replace(/-{2,}/g, '-')
}

function handleProfessionalPackageChange(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0] || null
  if (!file) {
    professionalPackageFile.value = null
    return
  }
  if (file.size > PROFESSIONAL_PACKAGE_MAX_BYTES) {
    MessagePlugin.warning('技能包不能超过 30MB；专业技能应只包含 SKILL.md 和必要资源。')
    input.value = ''
    professionalPackageFile.value = null
    return
  }
  professionalPackageFile.value = file
  if (professionalEditorMode.value === 'create') {
    const candidate = professionalNameFromPackage(file.name).trim()
    if (isValidProfessionalName(candidate)) professionalImportForm.value.name = candidate
  }
}

async function saveProfessionalSkill() {
  const name = professionalImportForm.value.name.trim()
  const description = professionalImportForm.value.description.trim()
  if (name && !isValidProfessionalName(name)) {
    MessagePlugin.warning('运行标识只能使用字母、数字和连字符，最长 64 个字符。')
    return
  }
  if (professionalEditorMode.value === 'edit' && !name) {
    MessagePlugin.warning('编辑专业技能时运行标识不能为空')
    return
  }
  if (charCount(description) > LIGHTWEIGHT_DESCRIPTION_MAX_CHARS) {
    MessagePlugin.warning(`技能描述不能超过 ${LIGHTWEIGHT_DESCRIPTION_MAX_CHARS} 字`)
    return
  }
  if (professionalEditorMode.value === 'create' && !professionalPackageFile.value) {
    MessagePlugin.warning('请选择专业技能压缩包')
    return
  }
  if (professionalPackageFile.value && professionalPackageFile.value.size > PROFESSIONAL_PACKAGE_MAX_BYTES) {
    MessagePlugin.warning('技能包不能超过 30MB')
    return
  }
  savingProfessional.value = true
  try {
    if (professionalEditorMode.value === 'edit' && currentProfessionalSkill.value?.id) {
      const res = await updateProfessionalSkill(currentProfessionalSkill.value.id, {
        name,
        description,
        package: professionalPackageFile.value,
      })
      if (res?.data) currentProfessionalSkill.value = res.data
    } else {
      if (!professionalPackageFile.value) {
        MessagePlugin.warning('请选择专业技能压缩包')
        return
      }
      await importProfessionalSkill({
        name,
        description,
        package: professionalPackageFile.value,
      })
    }
    MessagePlugin.success('专业技能已保存')
    professionalImportVisible.value = false
    editorResources.invalidate('skills')
    await loadProfessionalSkills()
  } catch (e: any) {
    MessagePlugin.error(e?.message || '专业技能保存失败')
  } finally {
    savingProfessional.value = false
  }
}

function confirmDeleteProfessional(skill: ManagedProfessionalSkill) {
  if (!skill.id || !canManageProfessionalSkill(skill)) return
  professionalDetailVisible.value = false
  showConfirmDelete({
    body: `确认删除专业技能“${skill.name}”？`,
    onConfirm: async () => {
      await deleteProfessionalSkill(skill.id!)
      MessagePlugin.success(t('skill.messages.deleted'))
      professionalDetailVisible.value = false
      editorResources.invalidate('skills')
      await loadProfessionalSkills()
    },
  })
}

async function downloadProfessional(skill: ManagedProfessionalSkill) {
  if (!skill.id || !skill.can_download) return
  try {
    const blob = await downloadProfessionalSkill(skill.id)
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = skill.archive_file_name || `${skill.name}.zip`
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
  } catch (e: any) {
    MessagePlugin.error(e?.message || '专业技能下载失败')
  }
}

function openSkillDialog(skill: ManagedSkill) {
  if (canManageSkill(skill)) {
    openEditDialog(skill)
    return
  }
  openViewDialog(skill)
}

function openEditDialog(skill: ManagedSkill) {
  editorMode.value = 'edit'
  currentSkill.value = skill
  editorForm.value = {
    name: skill.name,
    description: skill.description || '',
    instructions: skill.instructions || '',
    enabled: skill.enabled,
  }
  editorVisible.value = true
  openMoreId.value = ''
}

function openViewDialog(skill: ManagedSkill) {
  editorMode.value = 'view'
  currentSkill.value = skill
  editorForm.value = {
    name: skill.name,
    description: skill.description || '',
    instructions: skill.instructions || '',
    enabled: skill.enabled,
  }
  editorVisible.value = true
  openMoreId.value = ''
}

function resetEditor() {
  saving.value = false
}

function charCount(value: string) {
  return Array.from(value).length
}

function buildLightweightPayload(): SkillPayload {
  const name = editorForm.value.name.trim()
  const description = editorForm.value.description.trim()
  const instructions = editorForm.value.instructions.trim()
  if (!name) {
    throw new Error('请填写技能名称')
  }
  if (charCount(name) > LIGHTWEIGHT_NAME_MAX_CHARS) {
    throw new Error(`技能名称不能超过 ${LIGHTWEIGHT_NAME_MAX_CHARS} 字`)
  }
  if (/[\r\n\t]/.test(name)) {
    throw new Error('技能名称不能包含换行或制表符')
  }
  if (charCount(description) > LIGHTWEIGHT_DESCRIPTION_MAX_CHARS) {
    throw new Error(`技能描述不能超过 ${LIGHTWEIGHT_DESCRIPTION_MAX_CHARS} 字`)
  }
  if (!instructions) {
    throw new Error('请填写预置提示词')
  }
  if (charCount(instructions) > LIGHTWEIGHT_PROMPT_MAX_CHARS) {
    throw new Error(`预置提示词不能超过 ${LIGHTWEIGHT_PROMPT_MAX_CHARS} 字`)
  }
  return {
    name,
    description,
    instructions,
    enabled: editorForm.value.enabled,
  }
}

async function saveSkill() {
  if (editorReadOnly.value) return

  let payload: SkillPayload
  try {
    payload = buildLightweightPayload()
  } catch (e: any) {
    MessagePlugin.warning(e?.message || '轻量技能校验失败')
    return
  }

  saving.value = true
  try {
    if (editorMode.value === 'create') {
      await createManagedSkill(payload)
      MessagePlugin.success(t('skill.messages.created'))
    } else if (currentSkill.value) {
      await updateManagedSkill(currentSkill.value.id, payload)
      MessagePlugin.success(t('skill.messages.updated'))
    }
    editorVisible.value = false
    editorResources.invalidate('skills')
    await Promise.all([loadSkills(), loadOrgSkillCounts()])
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('skill.messages.saveFailed'))
  } finally {
    saving.value = false
  }
}

function confirmDeleteSkillBody(skill: ManagedSkill) {
  return t('skill.deleteConfirm', { name: skill.name }) as string
}

function confirmDelete(skill: ManagedSkill) {
  showConfirmDelete({
    body: confirmDeleteSkillBody(skill),
    onConfirm: async () => {
      await deleteManagedSkill(skill.id)
      MessagePlugin.success(t('skill.messages.deleted'))
      editorResources.invalidate('skills')
      await Promise.all([loadSkills(), loadOrgSkillCounts()])
    },
  })
}

async function openShareDialog(skill: ManagedSkill) {
  shareSkillKind.value = 'lightweight'
  currentSkill.value = skill
  currentProfessionalSkill.value = null
  shareVisible.value = true
  shareTab.value = 'organization'
  openMoreId.value = ''
  await Promise.all([loadOrganizations(), loadShares(skill.id)])
}

async function openProfessionalShareDialog(skill: ManagedProfessionalSkill) {
  if (!skill.id || !canManageProfessionalSkill(skill)) return
  professionalDetailVisible.value = false
  shareSkillKind.value = 'professional'
  currentSkill.value = null
  currentProfessionalSkill.value = skill
  shareVisible.value = true
  shareTab.value = 'organization'
  openMoreId.value = ''
  await Promise.all([loadOrganizations(), loadShares(skill.id)])
}

function resetShareDialog() {
  orgShareForm.value = { organization_id: '', permission: 'viewer' }
  userShareForm.value = { user_id: '', username: '', permission: 'viewer' }
  userCandidates.value = []
  userSearchQuery.value = ''
}

async function loadShares(skillId: string) {
  loadingShares.value = true
  try {
    const res = shareSkillKind.value === 'professional'
      ? await listProfessionalSkillShares(skillId)
      : await listSkillShares(skillId)
    organizationShares.value = res?.data?.organization_shares || []
    userShares.value = res?.data?.user_shares || []
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('skill.share.loadFailed'))
  } finally {
    loadingShares.value = false
  }
}

async function shareToOrganization() {
  const skillId = shareSkillKind.value === 'professional' ? currentProfessionalSkill.value?.id : currentSkill.value?.id
  if (!skillId || !orgShareForm.value.organization_id) {
    MessagePlugin.warning(t('organization.share.selectOrgPlaceholder'))
    return
  }
  sharing.value = true
  try {
    if (shareSkillKind.value === 'professional') {
      await shareProfessionalSkillToOrganization(skillId, orgShareForm.value)
    } else {
      await shareSkillToOrganization(skillId, orgShareForm.value)
    }
    MessagePlugin.success(t('organization.share.shareSuccess'))
    orgShareForm.value = { organization_id: '', permission: 'viewer' }
    const reloads: Promise<unknown>[] = [loadShares(skillId)]
    if (shareSkillKind.value === 'lightweight') reloads.push(loadOrgSkillCounts())
    await Promise.all(reloads)
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('organization.share.shareFailed'))
  } finally {
    sharing.value = false
  }
}

async function searchUsers() {
  searchingUsers.value = true
  try {
    const res = await searchSkillShareUsers(userSearchQuery.value.trim())
    userCandidates.value = Array.isArray(res?.data) ? res.data : []
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('skill.share.searchUserFailed'))
  } finally {
    searchingUsers.value = false
  }
}

function selectShareUser(user: SkillShareUser) {
  userShareForm.value.user_id = user.id
  userShareForm.value.username = user.username
}

async function shareToUser() {
  const skillId = shareSkillKind.value === 'professional' ? currentProfessionalSkill.value?.id : currentSkill.value?.id
  if (!skillId || !userShareForm.value.user_id) return
  sharing.value = true
  try {
    const payload = {
      user_id: userShareForm.value.user_id,
      permission: userShareForm.value.permission,
    }
    if (shareSkillKind.value === 'professional') {
      await shareProfessionalSkillToUser(skillId, payload)
    } else {
      await shareSkillToUser(skillId, payload)
    }
    MessagePlugin.success(t('skill.share.userShareSuccess'))
    userShareForm.value = { user_id: '', username: '', permission: 'viewer' }
    await loadShares(skillId)
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('skill.share.userShareFailed'))
  } finally {
    sharing.value = false
  }
}

async function removeOrganizationShare(share: ShareListItem) {
  const skillId = shareSkillKind.value === 'professional' ? currentProfessionalSkill.value?.id : currentSkill.value?.id
  if (!skillId || !share.share_id) return
  if (shareSkillKind.value === 'professional') {
    await removeProfessionalSkillOrganizationShare(skillId, share.share_id)
  } else {
    await removeSkillOrganizationShare(skillId, share.share_id)
  }
  MessagePlugin.success(t('organization.share.unshareSuccess'))
  const reloads: Promise<unknown>[] = [loadShares(skillId)]
  if (shareSkillKind.value === 'lightweight') reloads.push(loadOrgSkillCounts())
  await Promise.all(reloads)
}

async function removeUserShare(share: ShareListItem) {
  const skillId = shareSkillKind.value === 'professional' ? currentProfessionalSkill.value?.id : currentSkill.value?.id
  if (!skillId || !share.share_id) return
  if (shareSkillKind.value === 'professional') {
    await removeProfessionalSkillUserShare(skillId, share.share_id)
  } else {
    await removeSkillUserShare(skillId, share.share_id)
  }
  MessagePlugin.success(t('organization.share.unshareSuccess'))
  await loadShares(skillId)
}

watch(spaceSelection, async (value) => {
  if (!RESERVED_SCOPES.has(value) && !orgSkillMap.value[value]) {
    await loadOrgSkill(value)
  }
})

onMounted(async () => {
  await Promise.all([loadSkills(), loadProfessionalSkills(), loadOrgSkillCounts()])
})
</script>

<style scoped lang="less">
.skill-list-container {
  display: flex;
  width: 100%;
  height: 100%;
  min-height: 0;
  background: var(--td-bg-color-page);
}

.skill-list-content {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.header {
  flex-shrink: 0;
  padding: 28px 32px 18px;
}

.title-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.header-title h2 {
  margin: 0;
  font-size: 24px;
  line-height: 32px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.header-subtitle {
  margin: 6px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 14px;
}

.header-action-btn {
  width: 28px;
  height: 28px;
}

.skill-type-tabs {
  --wails-draggable: no-drag;
  display: inline-flex;
  align-items: center;
  align-self: flex-start;
  gap: 22px;
  margin: 0 32px 18px;
  border-bottom: 1px solid var(--td-component-stroke);
  font-family: var(--app-font-family);
}

.skill-type-tab {
  position: relative;
  height: 32px;
  padding: 0 0 9px;
  border: 0;
  background: transparent;
  color: var(--td-text-color-secondary);
  font-family: inherit;
  font-size: 14px;
  font-weight: 500;
  line-height: 20px;
  cursor: pointer;
  transition: color 0.18s ease;

  span {
    margin-left: 6px;
    color: var(--td-text-color-placeholder);
    font-size: 12px;
  }

  &:hover {
    color: var(--td-text-color-primary);
  }

  &.active {
    color: var(--td-brand-color);
    font-weight: 600;

    &::after {
      content: '';
      position: absolute;
      right: 0;
      bottom: -1px;
      left: 0;
      height: 2px;
      border-radius: 1px;
      background: var(--td-brand-color);
    }
  }
}

.skill-list-main {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 0 32px 32px;
}

.skill-card-wrap {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
  gap: 14px;
}

.skill-card {
  min-height: 168px;
  display: flex;
  flex-direction: column;
  padding: 14px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
  cursor: pointer;
  transition: border-color 0.15s, box-shadow 0.15s;

  &:hover {
    border-color: var(--td-brand-color-light);
    box-shadow: var(--td-shadow-1);
  }
}

.skill-card-skeleton {
  justify-content: center;
}

.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}

.card-title-wrap {
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 8px;
}

.skill-icon {
  width: 28px;
  height: 28px;
  border-radius: 7px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: #2563eb;
  background: rgba(37, 99, 235, 0.1);
  flex-shrink: 0;
}

.professional-icon {
  color: #7c3aed;
  background: rgba(124, 58, 237, 0.1);
}

.professional-card {
  cursor: default;
}

.card-title {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 16px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.more-wrap {
  width: 26px;
  height: 26px;
  border-radius: 6px;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;

  &:hover,
  &.active-more {
    background: var(--td-bg-color-container-hover);
  }
}

.more-icon {
  width: 16px;
  height: 16px;
}

.card-description {
  margin: 12px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 20px;
  display: -webkit-box;
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.card-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 12px;
}

.card-bottom {
  margin-top: auto;
  padding-top: 12px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
}

.empty-state {
  min-height: 320px;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: var(--td-text-color-secondary);
}

.empty-icon {
  font-size: 34px;
  color: var(--td-brand-color);
}

.empty-title {
  font-size: 16px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.empty-desc {
  font-size: 13px;
  color: var(--td-text-color-secondary);
}

.editor-layout {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.editor-tip {
  display: flex;
  gap: 8px;
  align-items: flex-start;
  padding: 10px 12px;
  border-radius: 7px;
  background: var(--td-bg-color-container-hover);
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 20px;
}

.rule-help-button {
  display: inline-flex;
  flex: 0 0 auto;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  margin-top: 2px;
  padding: 0;
  border: 0;
  background: transparent;
  color: var(--td-text-color-placeholder);
  font-size: 15px;
  cursor: help;

  &:hover,
  &:focus-visible {
    color: var(--td-brand-color);
    outline: none;
  }
}

.skill-md-editor {
  :deep(.t-textarea__inner) {
    font-family: ui-monospace, SFMono-Regular, Consolas, "Liberation Mono", monospace;
    font-size: 13px;
    line-height: 20px;
  }
}

.dialog-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  padding-top: 8px;
}

.professional-import-dialog,
.professional-detail {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.form-hint {
  margin: 6px 0 0;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}

.package-drop {
  display: flex;
  min-height: 118px;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 18px;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container-hover);
  color: var(--td-text-color-secondary);
  cursor: pointer;
  text-align: center;
  transition: border-color 0.15s, background 0.15s;

  &:hover {
    border-color: var(--td-brand-color);
    background: var(--td-brand-color-light);
  }

  input {
    display: none;
  }

  .t-icon {
    color: var(--td-brand-color);
    font-size: 24px;
  }

  span {
    color: var(--td-text-color-primary);
    font-weight: 600;
  }

  small {
    max-width: 500px;
    color: var(--td-text-color-placeholder);
    font-size: 12px;
    line-height: 18px;
  }
}

.share-dialog {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.share-skill-name {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  color: var(--td-text-color-primary);
  font-weight: 600;
}

.share-form {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto auto;
  gap: 10px;
  align-items: center;
  padding: 12px 0;
}

.share-form-user {
  grid-template-columns: 1fr;
  align-items: stretch;
}

.user-search-row {
  display: grid;
  grid-template-columns: 1fr auto;
  gap: 8px;
}

.user-candidates {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.user-candidate {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  color: var(--td-text-color-primary);
  padding: 6px 9px;
  cursor: pointer;

  &.selected {
    border-color: var(--td-brand-color);
    color: var(--td-brand-color);
    background: var(--td-brand-color-light);
  }

  small {
    color: var(--td-text-color-placeholder);
  }
}

.share-list {
  margin-top: 8px;
  border-top: 1px solid var(--td-component-stroke);
  padding-top: 12px;
}

.share-list-title {
  margin-bottom: 8px;
  font-size: 13px;
  font-weight: 600;
  color: var(--td-text-color-secondary);
}

.share-empty {
  padding: 20px 0;
  text-align: center;
  color: var(--td-text-color-placeholder);
  font-size: 13px;
}

.share-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 8px;
  border-radius: 7px;

  &:hover {
    background: var(--td-bg-color-container-hover);
  }
}

.share-row-main {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  color: var(--td-text-color-primary);
}

:global(.card-more-popup .popup-menu) {
  min-width: 120px;
  padding: 6px;
}

:global(.card-more-popup .popup-menu-item) {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border-radius: 6px;
  cursor: pointer;
  color: var(--td-text-color-primary);
  font-size: 13px;
}

:global(.card-more-popup .popup-menu-item:hover) {
  background: var(--td-bg-color-container-hover);
}

:global(.card-more-popup .popup-menu-item.delete) {
  color: var(--td-error-color);
}
</style>
