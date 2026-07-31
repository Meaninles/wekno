<template>
  <div
    v-if="rejection"
    class="queue-rejection"
    :class="personal ? 'queue-rejection--personal' : 'queue-rejection--system'"
    :data-testid="personal ? 'chat-queue-user-limit' : 'chat-queue-full'"
    role="alert"
  >
    <div class="queue-rejection__mark">{{ personal ? '!' : '×' }}</div>
    <div class="queue-rejection__body">
      <strong>{{ personal ? '你的排队会话已达上限' : '当前模型的系统队列已满' }}</strong>
      <p>{{ rejection.message }}</p>
      <small v-if="personal">
        个人等待 {{ rejection.user_waiting || 0 }} / {{ rejection.user_max_waiting || 0 }}
      </small>
      <small v-else-if="rejection.max_waiting !== undefined">
        模型队列 {{ rejection.waiting || 0 }} / {{ rejection.max_waiting }}
      </small>
    </div>
    <button type="button" aria-label="关闭提示" @click="$emit('close')">×</button>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import {
  isPersonalQueueLimit,
  type ChatQueueRejection,
} from './types'

const props = defineProps<{ rejection?: ChatQueueRejection | null }>()
defineEmits<{ close: [] }>()
const personal = computed(() => isPersonalQueueLimit(props.rejection))
</script>

<style scoped lang="less">
.queue-rejection {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  width: min(820px, calc(100% - 24px));
  margin: 0 auto 8px;
  padding: 11px 12px;
  border: 1px solid;
  border-radius: 10px;
  box-sizing: border-box;
}

.queue-rejection--personal {
  border-color: color-mix(in srgb, var(--td-warning-color) 55%, transparent);
  background: color-mix(in srgb, var(--td-warning-color-light) 68%, var(--td-bg-color-container));
}

.queue-rejection--system {
  border-color: color-mix(in srgb, var(--td-error-color) 48%, transparent);
  background: color-mix(in srgb, var(--td-error-color-light) 68%, var(--td-bg-color-container));
}

.queue-rejection__mark {
  display: grid;
  place-items: center;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  color: #fff;
  font-weight: 700;
  background: var(--td-warning-color);
}

.queue-rejection--system .queue-rejection__mark {
  background: var(--td-error-color);
}

.queue-rejection__body {
  min-width: 0;
  flex: 1;
}

.queue-rejection strong {
  display: block;
  font-size: 13px;
}

.queue-rejection p,
.queue-rejection small {
  display: block;
  margin: 3px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 1.45;
}

.queue-rejection > button {
  border: 0;
  background: transparent;
  color: var(--td-text-color-secondary);
  cursor: pointer;
  font-size: 18px;
}
</style>
