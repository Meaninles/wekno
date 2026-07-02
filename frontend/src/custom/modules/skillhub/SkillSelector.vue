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
          @keydown.esc="close"
        />
      </div>

      <div class="skill-list" ref="skillList" @wheel.stop>
        <div
          v-for="(skill, index) in filteredSkills"
          :key="skill.name"
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
            <t-icon name="lightbulb" />
          </div>
          <div class="skill-body">
            <div class="skill-name">{{ skill.name }}</div>
            <div class="skill-desc">{{ skill.description }}</div>
          </div>
        </div>

        <div v-if="!skillsAvailable" class="skill-empty">
          {{ $t('skill.selector.sandboxUnavailable') }}
        </div>
        <div v-else-if="filteredSkills.length === 0" class="skill-empty">
          {{ searchQuery ? $t('skill.selector.noMatch') : $t('skill.selector.empty') }}
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
import { useSettingsStore } from '@/stores/settings'
import { listSkills, type SkillInfo } from '@/api/skill'
import { getRootZoom, rectToCssPx, cssViewportSize } from '@/utils/zoom'

const props = defineProps<{
  visible: boolean
  anchorEl?: any | null
  dropdownWidth?: number
  offsetY?: number
}>()

const emit = defineEmits(['close', 'update:visible'])
const router = useRouter()
const settingsStore = useSettingsStore()

const searchQuery = ref('')
const highlightedIndex = ref(0)
const skills = ref<SkillInfo[]>([])
const skillsAvailable = ref(true)
const searchInput = ref<HTMLInputElement | null>(null)
const skillList = ref<HTMLElement | null>(null)
const dropdownStyle = ref<Record<string, string>>({})
const dropdownWidth = props.dropdownWidth ?? 320
const offsetY = props.offsetY ?? 8

const selectedNames = computed(() => settingsStore.settings.selectedSkillNames || [])

const filteredSkills = computed(() => {
  if (!searchQuery.value) return skills.value
  const q = searchQuery.value.toLowerCase()
  return skills.value.filter((skill) =>
    skill.name.toLowerCase().includes(q) || (skill.description || '').toLowerCase().includes(q),
  )
})

const resolveAnchorEl = () => {
  const a = props.anchorEl
  if (!a) return null
  if (typeof a === 'object' && 'value' in a) return a.value ?? null
  if (typeof a === 'object' && '$el' in a) return a.$el ?? null
  return a
}

const isSelected = (name: string) => selectedNames.value.includes(name)

const toggleSkill = (name: string) => {
  isSelected(name) ? settingsStore.removeSkillName(name) : settingsStore.addSkillName(name)
}

const toggleSelection = () => {
  const skill = filteredSkills.value[highlightedIndex.value]
  if (skill) toggleSkill(skill.name)
}

const moveSelection = (dir: number) => {
  const max = filteredSkills.value.length
  if (max === 0) return
  highlightedIndex.value = Math.max(0, Math.min(max - 1, highlightedIndex.value + dir))
  nextTick(() => {
    const items = skillList.value?.querySelectorAll('.skill-item')
    items?.[highlightedIndex.value]?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  })
}

const selectAll = () => settingsStore.selectSkillNames(filteredSkills.value.map((skill) => skill.name))
const clearAll = () => settingsStore.clearSkillNames()

const close = () => {
  emit('update:visible', false)
  emit('close')
}

const goManage = () => {
  close()
  router.push('/platform/skills')
}

const loadSkillOptions = async () => {
  try {
    const res = await listSkills()
    skillsAvailable.value = res?.skills_available !== false
    skills.value = Array.isArray(res?.data) ? res.data : []
  } catch (e) {
    console.error('[SkillSelector] failed to load skills', e)
    skillsAvailable.value = false
    skills.value = []
  }
}

const updateDropdownPosition = () => {
  const anchor = resolveAnchorEl()
  const zoom = getRootZoom()
  const { width: vwFallback, height: vhFallback } = cssViewportSize(zoom)

  const applyFallback = () => {
    const topFallback = Math.max(80, vhFallback / 2 - 160)
    dropdownStyle.value = {
      position: 'fixed',
      width: `${dropdownWidth}px`,
      left: `${Math.round((vwFallback - dropdownWidth) / 2)}px`,
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
  const minLeft = 16
  const maxLeft = Math.max(16, vw - dropdownWidth - 16)
  left = Math.max(minLeft, Math.min(maxLeft, left))

  const preferredDropdownHeight = 300
  const minDropdownHeight = 210
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
      width: `${dropdownWidth}px`,
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
      width: `${dropdownWidth}px`,
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

watch(() => props.visible, async (v) => {
  if (v) {
    await loadSkillOptions()
    await nextTick()
    requestAnimationFrame(() => {
      updateDropdownPosition()
      requestAnimationFrame(() => updateDropdownPosition())
    })
    nextTick(() => searchInput.value?.focus())
    resizeHandler = () => updateDropdownPosition()
    scrollHandler = () => updateDropdownPosition()
    window.addEventListener('resize', resizeHandler, { passive: true })
    window.addEventListener('scroll', scrollHandler, { passive: true, capture: true })
  } else {
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
  padding: 7px 8px;
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
