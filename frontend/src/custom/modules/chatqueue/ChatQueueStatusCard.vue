<template>
  <div
    v-if="props.status"
    class="chat-queue-card"
    :class="{ 'chat-queue-card--admitted': props.status.state === 'admitted' }"
    :data-testid="props.status.state === 'waiting' ? 'chat-queue-waiting' : 'chat-queue-admitted'"
    role="status"
  >
    <div class="chat-queue-card__icon">
      <span></span><span></span><span></span>
    </div>
    <div class="chat-queue-card__body">
      <template v-if="props.status.state === 'waiting'">
        <strong>当前模型正在排队</strong>
        <p>
          <template v-if="Number(props.status.active || 0) > 0">
            当前有 {{ props.status.active }} 个会话执行中，
          </template>
          你位于等待队列第 {{ props.status.position || 1 }} 位，轮到后会自动开始，无需重复发送。
        </p>
        <small>
          当前等待 {{ props.status.waiting || props.status.position || 1 }}
          <template v-if="props.status.max_waiting"> / 等待容量 {{ props.status.max_waiting }}</template>
          <template v-if="props.status.max_concurrent"> · 执行上限 {{ props.status.max_concurrent }}</template>
        </small>
      </template>
      <template v-else>
        <strong>已轮到你，正在开始</strong>
        <p>排队已经结束，模型将自动执行本次会话。</p>
      </template>
    </div>
    <button
      v-if="props.status.state === 'waiting'"
      type="button"
      class="chat-queue-card__cancel"
      @click="$emit('cancel')"
    >
      取消排队
    </button>
  </div>
</template>

<script setup lang="ts">
import type { ChatQueueStatus } from './types'

const props = defineProps<{ status?: ChatQueueStatus | null }>()
defineEmits<{ cancel: [] }>()
</script>

<style scoped lang="less">
.chat-queue-card {
  display: flex;
  align-items: center;
  gap: 12px;
  width: min(680px, 100%);
  padding: 13px 14px;
  margin: 2px 0 8px;
  border: 1px solid color-mix(in srgb, var(--td-brand-color) 36%, transparent);
  border-radius: 10px;
  background: color-mix(in srgb, var(--td-brand-color-light) 54%, var(--td-bg-color-container));
  color: var(--td-text-color-primary);
  box-sizing: border-box;
}

.chat-queue-card__icon {
  display: flex;
  gap: 3px;
  flex: 0 0 auto;
}

.chat-queue-card__icon span {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--td-brand-color);
  animation: queue-pulse 1.2s ease-in-out infinite;
}

.chat-queue-card__icon span:nth-child(2) { animation-delay: .16s; }
.chat-queue-card__icon span:nth-child(3) { animation-delay: .32s; }

.chat-queue-card__body {
  min-width: 0;
  flex: 1;
}

.chat-queue-card__body strong {
  display: block;
  font-size: 14px;
}

.chat-queue-card__body p,
.chat-queue-card__body small {
  display: block;
  margin: 3px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 1.5;
}

.chat-queue-card__cancel {
  flex: 0 0 auto;
  padding: 5px 9px;
  border: 1px solid color-mix(in srgb, var(--td-brand-color) 45%, transparent);
  border-radius: 6px;
  background: transparent;
  color: var(--td-brand-color);
  cursor: pointer;
}

.chat-queue-card--admitted {
  border-color: color-mix(in srgb, var(--td-success-color) 42%, transparent);
  background: color-mix(in srgb, var(--td-success-color-light) 58%, var(--td-bg-color-container));
}

.chat-queue-card--admitted .chat-queue-card__icon span {
  background: var(--td-success-color);
  animation: none;
}

@keyframes queue-pulse {
  0%, 70%, 100% { opacity: .35; transform: translateY(0); }
  35% { opacity: 1; transform: translateY(-3px); }
}
</style>
