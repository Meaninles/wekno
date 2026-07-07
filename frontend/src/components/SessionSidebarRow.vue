<template>
  <div
    :class="[
      'submenu_item',
      item.path && activePath === item.path ? 'submenu_item_active' : '',
    ]"
    @mouseenter="emit('hover-in')"
    @mouseleave="emit('hover-out')"
    @click="emit('navigate')"
  >
    <span class="submenu_title" :title="item.title || ''">
      <t-icon v-if="item.is_pinned" name="pin" class="submenu_pin_icon" />
      <span class="submenu_title-text">{{ item.title || item.id }}</span>
    </span>
    <i v-if="unread" class="session-unread-dot" aria-label="有新回复"></i>
    <div class="session-row-menu-wrap" @click.stop>
      <button
        ref="triggerRef"
        type="button"
        class="menu-more-wrap"
        aria-haspopup="menu"
        :aria-expanded="menuOpen"
        @click="toggleMenu"
      >
        <t-icon name="ellipsis" class="menu-more" />
      </button>
      <Teleport to="body">
        <div
          v-if="menuOpen"
          class="session-row-menu"
          role="menu"
          :style="menuStyle"
          @click.stop
        >
          <button
            v-for="option in menuOptions"
            :key="option.value"
            type="button"
            class="session-row-menu__item"
            :class="{ 'session-row-menu__item--error': option.theme === 'error' }"
            role="menuitem"
            @click="handleMenuClick(option)"
          >
            <component
              :is="option.prefixIcon"
              v-if="option.prefixIcon"
              class="session-row-menu__icon"
            />
            <span class="session-row-menu__text">{{ option.content }}</span>
          </button>
        </div>
      </Teleport>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, nextTick, ref } from 'vue'

interface SessionMenuOption {
  content: string
  value: string
  theme?: 'default' | 'success' | 'warning' | 'error' | 'primary'
  prefixIcon?: any
}

defineProps<{
  item: { id: string; path?: string; title?: string; is_pinned?: boolean }
  activePath: string
  menuOptions: SessionMenuOption[]
  unread?: boolean
  /** 渠道文件夹下的会话（样式与聊天区会话共用文案列对齐） */
  nested?: boolean
}>()

const emit = defineEmits<{
  (e: 'navigate'): void
  (e: 'menu-click', data: { value: string }): void
  (e: 'hover-in'): void
  (e: 'hover-out'): void
}>()

const MENU_WIDTH = 132
const MENU_GAP = 4
const VIEWPORT_MARGIN = 8

const menuOpen = ref(false)
const triggerRef = ref<HTMLButtonElement | null>(null)
const menuStyle = ref<Record<string, string>>({})

const updateMenuPosition = (): void => {
  const trigger = triggerRef.value
  if (!trigger) return
  const rect = trigger.getBoundingClientRect()
  const left = Math.max(
    VIEWPORT_MARGIN,
    Math.min(rect.right - MENU_WIDTH, window.innerWidth - MENU_WIDTH - VIEWPORT_MARGIN),
  )
  menuStyle.value = {
    top: `${rect.bottom + MENU_GAP}px`,
    left: `${left}px`,
  }
}

const removeListeners = (): void => {
  document.removeEventListener('click', closeMenu)
  window.removeEventListener('resize', closeMenu)
  window.removeEventListener('scroll', closeMenu, true)
}

const closeMenu = (): void => {
  menuOpen.value = false
  removeListeners()
}

const toggleMenu = (): void => {
  if (menuOpen.value) {
    closeMenu()
    return
  }
  updateMenuPosition()
  menuOpen.value = true
  nextTick(() => {
    document.addEventListener('click', closeMenu)
    window.addEventListener('resize', closeMenu)
    // 捕获阶段监听任意滚动容器，滚动时关闭以避免菜单与触发点错位
    window.addEventListener('scroll', closeMenu, true)
  })
}

const handleMenuClick = (option: SessionMenuOption): void => {
  closeMenu()
  emit('menu-click', { value: option.value })
}

onBeforeUnmount(() => {
  removeListeners()
})
</script>

<style scoped lang="less">
.submenu_item {
  position: relative;
}

.session-row-menu-wrap {
  position: relative;
  flex: 0 0 auto;
}

.session-unread-dot {
  flex: 0 0 auto;
  margin: 0 4px 0 6px;
}

.session-unread-dot {
  width: 7px;
  height: 7px;
  border-radius: 999px;
  background: #07c160;
  box-shadow: 0 0 0 3px rgba(7, 193, 96, 0.12);
}

.menu-more-wrap {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  padding: 0;
  border: 0;
  border-radius: 6px;
  color: inherit;
  background: transparent;
  cursor: pointer;
}

.session-row-menu {
  position: fixed;
  z-index: 3000;
  min-width: 132px;
  padding: 4px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  box-shadow: var(--td-shadow-2);
}

.session-row-menu__item {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  min-height: 32px;
  padding: 0 10px;
  border: 0;
  border-radius: 4px;
  color: var(--td-text-color-primary);
  background: transparent;
  font-size: 14px;
  line-height: 20px;
  text-align: left;
  cursor: pointer;

  &:hover {
    background: var(--td-bg-color-container-hover);
  }
}

.session-row-menu__item--error {
  color: var(--td-error-color);
}

.session-row-menu__icon {
  flex: 0 0 auto;
  display: inline-flex;
}

.session-row-menu__text {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
