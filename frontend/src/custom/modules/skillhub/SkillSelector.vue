<template>
  <div v-if="visible" class="skill-overlay" @click="close">
    <div class="skill-dropdown" @click.stop @wheel.stop :style="dropdownStyle">
      <div class="skill-search">
        <input
          ref="searchInput"
          v-model="searchQuery"
          type="text"
          :placeholder="$t('skill.selector.searchPlaceholder')"
          class="skill-search-input"
          @keydown.down.prevent="moveSelection(1)"
          @keydown.up.prevent="moveSelection(-1)"
          @keydown.enter.prevent="toggleSelection"
          @keydown.esc.stop.prevent="close"
        />
      </div>

      <div v-if="showSkillTabs" class="skill-tabs" role="tablist">
        <button
          type="button"
          class="skill-tab"
          :class="{ active: activeTab === 'professional' }"
          role="tab"
          :aria-selected="activeTab === 'professional'"
          @click="setActiveTab('professional')"
        >
          {{ $t('skill.selector.professionalTab') }}
        </button>
        <button
          type="button"
          class="skill-tab"
          :class="{ active: activeTab === 'lightweight' }"
          role="tab"
          :aria-selected="activeTab === 'lightweight'"
          @click="setActiveTab('lightweight')"
        >
          {{ $t('skill.selector.lightweightTab') }}
        </button>
      </div>

      <div class="skill-list" ref="skillList" @wheel.stop>
        <div
          v-for="(skill, index) in filteredSkills"
          :key="`${activeTab}-${skill.name}`"
          :class="['skill-item', { selected: isSelected(skill.name), highlighted: highlightedIndex === index }]"
          @click="toggleSkill(skill.name)"
          @mouseenter="highlightedIndex = index"
        >
          <div class="checkbox" :class="{ checked: isSelected(skill.name) }">
            <svg v-if="isSelected(skill.name)" width="12" height="12" viewBox="0 0 12 12" fill="none">
              <path d="M10 3L4.5 8.5L2 6" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
            </svg>
          </div>
          <div class="skill-icon">
            <t-icon :name="activeTab === 'professional' ? 'tools' : 'lightbulb'" />
          </div>
          <div class="skill-body">
            <div class="skill-name">{{ skill.name }}</div>
            <div class="skill-desc">{{ skill.description }}</div>
          </div>
          <div class="skill-item-actions" @click.stop>
            <t-tooltip :content="isPinned(skill.name) ? $t('menu.unpin') : $t('menu.pin')" placement="top">
              <button
                type="button"
                class="skill-pin"
                :class="{ pinned: isPinned(skill.name) }"
                :aria-label="isPinned(skill.name) ? $t('menu.unpin') : $t('menu.pin')"
                @click.stop="togglePinned(skill.name)"
              >
                <t-icon :name="isPinned(skill.name) ? 'pin-filled' : 'pin'" size="13px" />
              </button>
            </t-tooltip>
          </div>
        </div>

        <div v-if="skillsLoading" class="skill-empty">
          {{ $t('common.loading') }}
        </div>
        <div v-else-if="!currentSkillsAvailable" class="skill-empty">
          {{ activeTab === 'professional' ? $t('skill.selector.professionalUnavailable') : $t('skill.selector.sandboxUnavailable') }}
        </div>
        <div v-else-if="filteredSkills.length === 0" class="skill-empty">
          {{ emptyText }}
        </div>
      </div>

      <div class="skill-actions">
        <button @click="selectAll" class="skill-btn">{{ $t('common.selectAll') }}</button>
        <button @click="clearAll" class="skill-btn">{{ $t('common.clear') }}</button>
        <button @click="goManage" class="skill-btn skill-btn-manage">{{ $t('skill.selector.manage') }}</button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useSettingsStore } from '@/stores/settings'
import { listSkills, type SkillInfo } from '@/api/skill'
import { getRootZoom, rectToCssPx, cssViewportSize } from '@/utils/zoom'
import { useChatSkillPins, type SkillPinKind } from './skillPins'

type SkillSelectionMode = 'all' | 'selected' | 'none'
type SkillTab = SkillPinKind

const props = defineProps<{
  visible: boolean
  anchorEl?: any | null
  dropdownWidth?: number
  offsetY?: number
  professionalSelectionMode?: SkillSelectionMode | null
  allowedProfessionalSkillNames?: string[]
  selectedSkillNames?: string[]
  selectedProfessionalSkillNames?: string[]
}>()

const emit = defineEmits(['close', 'update:visible', 'update:selectedSkillNames', 'update:selectedProfessionalSkillNames'])
const router = useRouter()
const { t } = useI18n()
const settingsStore = useSettingsStore()
const lightweightPins = useChatSkillPins('lightweight')
const professionalPins = useChatSkillPins('professional')

const searchQuery = ref('')
const highlightedIndex = ref(0)
const activeTab = ref<SkillTab>('lightweight')
const skills = ref<SkillInfo[]>([])
const professionalSkills = ref<SkillInfo[]>([])
const skillsAvailable = ref(true)
const professionalSkillsAvailable = ref(false)
const skillsLoading = ref(false)
const searchInput = ref<HTMLInputElement | null>(null)
const skillList = ref<HTMLElement | null>(null)
const dropdownStyle = ref<Record<string, string>>({})
const dropdownWidth = props.dropdownWidth ?? 320
const offsetY = props.offsetY ?? 8
let loadRequestID = 0

const selectedNames = computed(() => Array.isArray(props.selectedSkillNames)
  ? props.selectedSkillNames
  : (settingsStore.settings.selectedSkillNames || []))
const selectedProfessionalNames = computed(() => Array.isArray(props.selectedProfessionalSkillNames)
  ? props.selectedProfessionalSkillNames
  : (settingsStore.settings.selectedProfessionalSkillNames || []))
const normalizedProfessionalMode = computed<SkillSelectionMode>(() => props.professionalSelectionMode || 'none')

const allowedProfessionalNameSet = computed(() => {
  const names = props.allowedProfessionalSkillNames || []
  return new Set(names.map((name) => String(name || '').trim()).filter(Boolean))
})

const professionalOptions = computed(() => {
  if (normalizedProfessionalMode.value === 'none') return []
  if (normalizedProfessionalMode.value === 'selected') {
    return professionalSkills.value.filter((skill) => allowedProfessionalNameSet.value.has(skill.name))
  }
  return professionalSkills.value
})

const hasProfessionalTab = computed(() =>
  professionalSkillsAvailable.value
  && normalizedProfessionalMode.value !== 'none'
  && professionalOptions.value.length > 0,
)
const showSkillTabs = computed(() => hasProfessionalTab.value)

const sortedLightweightSkills = computed(() => lightweightPins.sortPinnedFirst(skills.value, (skill) => skill.name))
const sortedProfessionalSkills = computed(() =>
  professionalPins.sortPinnedFirst(professionalOptions.value, (skill) => skill.name),
)

const currentSkills = computed(() => activeTab.value === 'professional' && hasProfessionalTab.value
  ? sortedProfessionalSkills.value
  : sortedLightweightSkills.value)
const currentSkillsAvailable = computed(() => activeTab.value === 'professional'
  ? professionalSkillsAvailable.value
  : skillsAvailable.value)

const filteredSkills = computed(() => {
  const source = currentSkills.value
  if (!searchQuery.value) return source
  const q = searchQuery.value.toLowerCase()
  return source.filter((skill) =>
    skill.name.toLowerCase().includes(q) || (skill.description || '').toLowerCase().includes(q),
  )
})

const emptyText = computed(() => {
  if (searchQuery.value) return t('skill.selector.noMatch')
  return activeTab.value === 'professional'
    ? t('skill.selector.professionalEmpty')
    : t('skill.selector.empty')
})

const resolveAnchorEl = () => {
  const resolve = (value: any, depth = 0): any | null => {
    if (!value || depth > 4) return null
    if (typeof Element !== 'undefined' && value instanceof Element) return value
    if (typeof value === 'object' && '$el' in value && value.$el) return resolve(value.$el, depth + 1)
    if (typeof value === 'object' && 'value' in value && value.value) return resolve(value.value, depth + 1)
    return value
  }
  return resolve(props.anchorEl)
}

const activeSelectedNames = computed(() =>
  activeTab.value === 'professional' ? selectedProfessionalNames.value : selectedNames.value,
)

const isSelected = (name: string) => activeSelectedNames.value.includes(name)

const setLightweightSelection = (names: string[]) => {
  if (Array.isArray(props.selectedSkillNames)) {
    emit('update:selectedSkillNames', normalizeNames(names))
    return
  }
  settingsStore.selectSkillNames(names)
}

const setProfessionalSelection = (names: string[]) => {
  if (Array.isArray(props.selectedProfessionalSkillNames)) {
    emit('update:selectedProfessionalSkillNames', normalizeNames(names))
    return
  }
  settingsStore.selectProfessionalSkillNames(names)
}

const normalizeNames = (names: string[]) => {
  const seen = new Set<string>()
  return (names || [])
    .map((name) => String(name || '').trim())
    .filter((name) => {
      if (!name || seen.has(name)) return false
      seen.add(name)
      return true
    })
}

const toggleSkill = (name: string) => {
  const selected = activeSelectedNames.value
  const removing = isSelected(name)
  const next = isSelected(name)
    ? selected.filter((item) => item !== name)
    : [...selected, name]
  if (activeTab.value === 'professional') {
    setProfessionalSelection(next)
    if (!removing) {
      setLightweightSelection(selectedNames.value.filter((item) => item !== name))
    }
    return
  }
  setLightweightSelection(next)
  if (!removing) {
    setProfessionalSelection(selectedProfessionalNames.value.filter((item) => item !== name))
  }
}

const toggleSelection = () => {
  const skill = filteredSkills.value[highlightedIndex.value]
  if (skill) toggleSkill(skill.name)
}

const setActiveTab = (tab: SkillTab) => {
  if (tab === 'professional' && !hasProfessionalTab.value) return
  activeTab.value = tab
  highlightedIndex.value = 0
  nextTick(() => searchInput.value?.focus())
}

const activePins = computed(() => activeTab.value === 'professional' ? professionalPins : lightweightPins)
const isPinned = (name: string) => activePins.value.isPinned(name)
const togglePinned = (name: string) => activePins.value.togglePinned(name)

const moveSelection = (dir: number) => {
  const max = filteredSkills.value.length
  if (max === 0) return
  highlightedIndex.value = Math.max(0, Math.min(max - 1, highlightedIndex.value + dir))
  nextTick(() => {
    const items = skillList.value?.querySelectorAll('.skill-item')
    items?.[highlightedIndex.value]?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  })
}

const selectAll = () => {
  const names = filteredSkills.value.map((skill) => skill.name)
  if (activeTab.value === 'professional') {
    setProfessionalSelection(names)
    return
  }
  setLightweightSelection(names)
}

const clearAll = () => {
  if (activeTab.value === 'professional') {
    setProfessionalSelection([])
    return
  }
  setLightweightSelection([])
}

const close = () => {
  emit('update:visible', false)
  emit('close')
}

const goManage = () => {
  close()
  router.push('/platform/skills')
}

const loadSkillOptions = async () => {
  const requestID = ++loadRequestID
  skillsLoading.value = true
  try {
    const res = await listSkills()
    if (requestID !== loadRequestID) return
    skillsAvailable.value = res?.skills_available !== false
    skills.value = Array.isArray(res?.data)
      ? res.data.filter((skill) => skill.kind !== 'professional')
      : []
    professionalSkills.value = Array.isArray(res?.professional_data) ? res.professional_data : []
    professionalSkillsAvailable.value = res?.professional_skills_available === true && professionalSkills.value.length > 0
  } catch (e) {
    if (requestID !== loadRequestID) return
    console.error('[SkillSelector] failed to load skills', e)
    skillsAvailable.value = false
    professionalSkillsAvailable.value = false
    skills.value = []
    professionalSkills.value = []
  } finally {
    if (requestID === loadRequestID) {
      skillsLoading.value = false
    }
  }
}

const updateDropdownPosition = () => {
  const anchor = resolveAnchorEl()
  const zoom = getRootZoom()
  const { width: vwFallback, height: vhFallback } = cssViewportSize(zoom)
  const viewportMargin = 16
  const effectiveWidth = Math.max(160, Math.min(dropdownWidth, vwFallback - viewportMargin * 2))

  const applyFallback = () => {
    const topFallback = Math.max(80, vhFallback / 2 - 160)
    dropdownStyle.value = {
      position: 'fixed',
      width: `${effectiveWidth}px`,
      left: `${Math.max(viewportMargin, Math.round((vwFallback - effectiveWidth) / 2))}px`,
      top: `${Math.round(topFallback)}px`,
      transform: 'none',
      margin: '0',
      padding: '0',
    }
  }

  if (!anchor) {
    applyFallback()
    return
  }

  let rawRect: { top: number; left: number; right: number; bottom: number; width: number; height: number } | null = null
  try {
    if (typeof anchor.getBoundingClientRect === 'function') {
      const r = anchor.getBoundingClientRect()
      rawRect = { top: r.top, left: r.left, right: r.right, bottom: r.bottom, width: r.width, height: r.height }
    } else if (anchor.width !== undefined && anchor.left !== undefined) {
      rawRect = anchor as DOMRect
    }
  } catch (e) {
    console.error('[SkillSelector] failed to get anchor rect', e)
  }

  if (!rawRect || rawRect.width === 0 || rawRect.height === 0) {
    applyFallback()
    return
  }

  const rect = rectToCssPx(rawRect, zoom)
  const vw = vwFallback
  const vh = vhFallback
  let left = Math.floor(rect.left)
  const minLeft = viewportMargin
  const maxLeft = Math.max(viewportMargin, vw - effectiveWidth - viewportMargin)
  left = Math.max(minLeft, Math.min(maxLeft, left))

  const preferredDropdownHeight = 330
  const minDropdownHeight = 230
  const topMargin = 20
  const spaceBelow = vh - rect.bottom
  const spaceAbove = rect.top
  let actualHeight: number
  let shouldOpenBelow: boolean

  if (spaceBelow >= minDropdownHeight + offsetY) {
    actualHeight = Math.min(preferredDropdownHeight, spaceBelow - offsetY - 16)
    shouldOpenBelow = true
  } else {
    const availableHeight = spaceAbove - offsetY - topMargin
    actualHeight = availableHeight >= preferredDropdownHeight
      ? preferredDropdownHeight
      : Math.max(minDropdownHeight, availableHeight)
    shouldOpenBelow = false
  }

  if (shouldOpenBelow) {
    dropdownStyle.value = {
      position: 'fixed',
      width: `${effectiveWidth}px`,
      left: `${left}px`,
      top: `${Math.floor(rect.bottom + offsetY)}px`,
      maxHeight: `${actualHeight}px`,
      transform: 'none',
      margin: '0',
      padding: '0',
    }
  } else {
    dropdownStyle.value = {
      position: 'fixed',
      width: `${effectiveWidth}px`,
      left: `${left}px`,
      bottom: `${vh - rect.top + offsetY}px`,
      maxHeight: `${actualHeight}px`,
      transform: 'none',
      margin: '0',
      padding: '0',
    }
  }
}

let resizeHandler: (() => void) | null = null
let scrollHandler: (() => void) | null = null

watch(filteredSkills, (items) => {
  if (highlightedIndex.value >= items.length) {
    highlightedIndex.value = Math.max(0, items.length - 1)
  }
})

watch(hasProfessionalTab, (hasTab) => {
  if (!hasTab && activeTab.value === 'professional') {
    activeTab.value = 'lightweight'
    highlightedIndex.value = 0
  }
})

watch(() => props.visible, async (v) => {
  if (v) {
    await nextTick()
    updateDropdownPosition()
    requestAnimationFrame(() => {
      updateDropdownPosition()
      requestAnimationFrame(() => updateDropdownPosition())
    })
    nextTick(() => searchInput.value?.focus())
    resizeHandler = () => updateDropdownPosition()
    scrollHandler = () => updateDropdownPosition()
    window.addEventListener('resize', resizeHandler, { passive: true })
    window.addEventListener('scroll', scrollHandler, { passive: true, capture: true })
    void loadSkillOptions().then(async () => {
      if (!props.visible) return
      activeTab.value = hasProfessionalTab.value ? 'professional' : 'lightweight'
      highlightedIndex.value = 0
      await nextTick()
      updateDropdownPosition()
    })
  } else {
    loadRequestID += 1
    skillsLoading.value = false
    searchQuery.value = ''
    highlightedIndex.value = 0
    if (resizeHandler) {
      window.removeEventListener('resize', resizeHandler)
      resizeHandler = null
    }
    if (scrollHandler) {
      window.removeEventListener('scroll', scrollHandler, { capture: true })
      scrollHandler = null
    }
  }
})
</script>

<style scoped lang="less">
.skill-overlay,
.skill-overlay *,
.skill-overlay *::before,
.skill-overlay *::after {
  box-sizing: border-box;
}

.skill-overlay {
  position: fixed;
  inset: 0;
  z-index: 9999;
  background: transparent;
  touch-action: none;
}

.skill-dropdown {
  position: fixed !important;
  max-width: calc(100vw - 32px);
  max-height: calc(100vh - 32px);
  background: var(--td-bg-color-container);
  border: .5px solid var(--td-component-border);
  border-radius: 10px;
  box-shadow: var(--td-shadow-2);
  overflow: hidden;
  animation: fadeIn 0.15s ease-out;
  z-index: 10000;
  display: flex;
  flex-direction: column;
}

.skill-search {
  padding: 8px 10px;
  border-bottom: .5px solid var(--td-component-stroke);
}

.skill-search-input {
  width: 100%;
  padding: 6px 10px;
  font-size: 12px;
  border: .5px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  outline: none;
  transition: border 0.12s;

  &:focus {
    border-color: var(--td-success-color);
    background: var(--td-bg-color-container);
  }
}

.skill-tabs {
  display: flex;
  gap: 4px;
  padding: 4px 8px 0;
  border-bottom: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-container);
}

.skill-tab {
  flex: 1;
  height: 28px;
  padding: 0 8px;
  border: none;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  transition: color 0.12s, border-color 0.12s;

  &:hover,
  &.active {
    color: var(--td-brand-color);
  }

  &.active {
    border-bottom-color: var(--td-brand-color);
  }
}

.skill-list {
  flex: 1;
  min-height: 0;
  max-height: 280px;
  overflow-y: auto;
  padding: 6px 8px;
  overscroll-behavior: contain;
}

.skill-item {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 7px 6px 7px 8px;
  border-radius: 6px;
  cursor: pointer;
  transition: background 0.12s;
  margin-bottom: 4px;

  &:hover,
  &.highlighted {
    background: var(--td-bg-color-secondarycontainer);
  }

  &.selected {
    background: var(--td-brand-color-light);
  }
}

.checkbox {
  width: 16px;
  height: 16px;
  margin-top: 2px;
  border-radius: 3px;
  border: 1.5px solid var(--td-component-border);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;

  &.checked {
    background: var(--td-success-color);
    border-color: var(--td-success-color);
  }
}

.skill-icon {
  width: 18px;
  height: 18px;
  margin-top: 1px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  color: var(--td-brand-color);
}

.skill-body {
  min-width: 0;
  flex: 1;
}

.skill-name {
  font-size: 12px;
  font-weight: 600;
  line-height: 18px;
  color: var(--td-text-color-primary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.skill-desc {
  margin-top: 2px;
  font-size: 11px;
  line-height: 16px;
  color: var(--td-text-color-secondary);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.skill-item-actions {
  display: flex;
  align-items: center;
  justify-content: center;
  min-width: 22px;
  height: 22px;
  flex-shrink: 0;
}

.skill-pin {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  padding: 0;
  border: none;
  border-radius: 4px;
  background: transparent;
  color: var(--td-text-color-placeholder);
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.12s, background 0.12s, color 0.12s;

  &:hover,
  &:focus-visible {
    background: var(--td-bg-color-component-hover, #e8e8e8);
    color: var(--td-brand-color);
    opacity: 1;
  }

  &.pinned {
    color: var(--td-brand-color);
    opacity: 1;
  }
}

.skill-item:hover .skill-pin,
.skill-item.selected .skill-pin {
  opacity: 1;
}

.skill-empty {
  padding: 22px 8px;
  text-align: center;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
}

.skill-actions {
  display: flex;
  gap: 8px;
  padding: 8px 10px;
  border-top: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);
}

.skill-btn {
  flex: 1;
  padding: 6px 10px;
  border-radius: 6px;
  border: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-container);
  font-size: 12px;
  color: var(--td-text-color-secondary);
  cursor: pointer;
  transition: all 0.12s;

  &:hover {
    border-color: var(--td-success-color);
    color: var(--td-success-color);
    background: var(--td-brand-color-light);
  }
}

.skill-btn-manage {
  flex: 1.2;
}

@keyframes fadeIn {
  from { opacity: 0; transform: scale(0.98); }
  to { opacity: 1; transform: scale(1); }
}
</style>
