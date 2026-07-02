<template>
  <span class="answer-feedback-buttons" @click.stop>
    <t-tooltip :content="likeTitle" placement="top">
      <t-button
        size="small"
        variant="outline"
        shape="round"
        class="feedback-btn"
        :class="{ 'is-active': current === 'like', 'is-like': current === 'like' }"
        :title="likeTitle"
        @click.stop="toggleFeedback('like')"
      >
        <t-icon name="thumb-up" />
      </t-button>
    </t-tooltip>
    <t-tooltip :content="dislikeTitle" placement="top">
      <t-button
        size="small"
        variant="outline"
        shape="round"
        class="feedback-btn"
        :class="{ 'is-active': current === 'dislike', 'is-dislike': current === 'dislike' }"
        :title="dislikeTitle"
        @click.stop="toggleFeedback('dislike')"
      >
        <t-icon name="thumb-down" />
      </t-button>
    </t-tooltip>
  </span>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import { setAnswerFeedback, type AnswerFeedbackValue } from './api';

const props = defineProps<{
  sessionId?: string;
  messageId?: string;
  initialFeedback?: AnswerFeedbackValue | null;
  target?: Record<string, any>;
}>();

const emit = defineEmits<{
  (e: 'change', value: AnswerFeedbackValue): void;
}>();

const { t } = useI18n();
const current = ref<AnswerFeedbackValue>(normalize(props.initialFeedback));
let requestSeq = 0;

const likeTitle = computed(() => current.value === 'like' ? t('answerFeedback.liked') : t('answerFeedback.like'));
const dislikeTitle = computed(() => current.value === 'dislike' ? t('answerFeedback.disliked') : t('answerFeedback.dislike'));

watch(
  () => props.initialFeedback,
  value => {
    current.value = normalize(value);
  },
);

async function toggleFeedback(value: Exclude<AnswerFeedbackValue, ''>) {
  if (!props.sessionId || !props.messageId) return;
  const next = current.value === value ? '' : value;
  const seq = ++requestSeq;
  current.value = next;
  syncTarget(next);
  emit('change', next);

  try {
    await setAnswerFeedback(props.sessionId, props.messageId, next);
  } catch (error) {
    console.warn('[answerfeedback] failed to persist answer feedback', error);
  } finally {
    if (seq === requestSeq) {
      syncTarget(current.value);
    }
  }
}

function normalize(value?: string | null): AnswerFeedbackValue {
  return value === 'like' || value === 'dislike' ? value : '';
}

function syncTarget(value: AnswerFeedbackValue) {
  if (props.target) {
    props.target.answer_feedback = value;
  }
}
</script>

<style scoped>
.answer-feedback-buttons {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}

.feedback-btn {
  color: var(--td-text-color-secondary);
}

.feedback-btn.is-active {
  color: #07c05f !important;
  border-color: #07c05f !important;
  background: rgba(7, 192, 95, 0.1) !important;
}

.feedback-btn.is-active:hover {
  color: #07c05f !important;
  border-color: #07c05f !important;
  background: rgba(7, 192, 95, 0.16) !important;
}

.feedback-btn.is-active :deep(.t-icon) {
  color: #07c05f;
}
</style>
