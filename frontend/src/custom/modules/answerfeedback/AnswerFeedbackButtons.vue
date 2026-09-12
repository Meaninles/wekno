<template>
  <span class="answer-feedback-buttons" @click.stop>
    <t-tooltip :content="likeTitle" placement="top">
      <t-button
        size="small"
        variant="outline"
        shape="round"
        class="feedback-btn"
        :class="{ 'is-active': current === 'solved', 'is-like': current === 'solved' }"
        :title="likeTitle"
        @click.stop="toggleLike"
      >
        <t-icon name="thumb-up" />
      </t-button>
    </t-tooltip>

    <t-popup
      v-model:visible="dislikePopupVisible"
      trigger="click"
      :disabled="true"
      placement="top"
      :show-arrow="true"
      destroy-on-close
      @visible-change="handleDislikePopupVisibleChange"
    >
      <t-tooltip :content="dislikeTitle" placement="top">
        <t-button
          size="small"
          variant="outline"
          shape="round"
          class="feedback-btn"
          :class="{ 'is-active': isNegativeFeedback(current), 'is-dislike': isNegativeFeedback(current) }"
          :title="dislikeTitle"
          @click.stop="toggleDislike"
        >
          <t-icon name="thumb-down" />
        </t-button>
      </t-tooltip>

      <template #content>
        <div class="feedback-reason-card" @click.stop>
          <button
            v-for="reason in negativeReasons"
            :key="reason.value"
            type="button"
            class="feedback-reason-option"
            :class="{ 'is-selected': current === reason.value }"
            @click.stop="selectNegativeReason(reason.value)"
          >
            <span>{{ reason.label }}</span>
            <span v-if="current === reason.value" class="feedback-reason-check">
              <t-icon name="check" />
            </span>
          </button>
        </div>
      </template>
    </t-popup>
  </span>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

import { setAnswerFeedback, type AnswerFeedbackValue } from './api';

type NegativeFeedback = 'off_topic' | 'inaccurate';

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
const dislikePopupVisible = ref(false);
const popupChoiceMade = ref(false);
const popupOriginalReason = ref<NegativeFeedback | null>(null);
let persistChain: Promise<unknown> = Promise.resolve();

const negativeReasons = computed<Array<{ value: NegativeFeedback; label: string }>>(() => [
  { value: 'off_topic', label: t('answerFeedback.offTopic') },
  { value: 'inaccurate', label: t('answerFeedback.inaccurate') },
]);

const likeTitle = computed(() => (
  current.value === 'solved' ? t('answerFeedback.liked') : t('answerFeedback.like')
));

const dislikeTitle = computed(() => {
  if (current.value === 'off_topic') return t('answerFeedback.offTopic');
  if (current.value === 'inaccurate') return t('answerFeedback.inaccurate');
  return current.value === 'unsolved' ? t('answerFeedback.disliked') : t('answerFeedback.dislike');
});

watch(
  () => props.initialFeedback,
  value => {
    current.value = normalize(value);
  },
);

function toggleLike() {
  popupChoiceMade.value = true;
  dislikePopupVisible.value = false;
  commit(current.value === 'solved' ? '' : 'solved');
}

function toggleDislike() {
  if (isNegativeFeedback(current.value)) {
    popupChoiceMade.value = true;
    popupOriginalReason.value = null;
    dislikePopupVisible.value = false;
    commit('');
    return;
  }

  prepareDislikePopup();
  dislikePopupVisible.value = true;
}

function prepareDislikePopup() {
  popupChoiceMade.value = false;
  popupOriginalReason.value = isReasonFeedback(current.value) ? current.value : null;
  // A plain point-down is already a valid negative signal. Preserve an
  // existing reason while the picker is reopened so the user can edit it.
  if (!popupOriginalReason.value) {
    commitIfDifferent('unsolved');
  }
}

function selectNegativeReason(value: NegativeFeedback) {
  popupChoiceMade.value = true;
  commit(value);
  dislikePopupVisible.value = false;
}

function handleDislikePopupVisibleChange(visible: boolean) {
  if (visible || popupChoiceMade.value || popupOriginalReason.value) return;
  commitIfDifferent('unsolved');
}

function commitIfDifferent(value: AnswerFeedbackValue) {
  if (current.value !== value) {
    commit(value);
  }
}

function commit(value: AnswerFeedbackValue) {
  current.value = value;
  syncTarget(value);
  emit('change', value);
  if (!props.sessionId || !props.messageId) return;

  // Serialize writes so a fast sequence of clicks cannot let an older PUT
  // overwrite the user's latest choice on the server.
  persistChain = persistChain
    .catch(() => undefined)
    .then(() => setAnswerFeedback(props.sessionId!, props.messageId!, value))
    .catch(error => {
      console.warn('[answerfeedback] failed to persist answer feedback', error);
    });
}

function normalize(value?: string | null): AnswerFeedbackValue {
  switch (value) {
    case 'like':
    case 'solved':
      return 'solved';
    case 'dislike':
    case 'unsolved':
      return 'unsolved';
    case 'off_topic':
      return 'off_topic';
    case 'inaccurate':
      return 'inaccurate';
    default:
      return '';
  }
}

function isNegativeFeedback(value: AnswerFeedbackValue): value is NegativeFeedback | 'unsolved' {
  return value === 'off_topic' || value === 'inaccurate' || value === 'unsolved';
}

function isReasonFeedback(value: AnswerFeedbackValue): value is NegativeFeedback {
  return value === 'off_topic' || value === 'inaccurate';
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

.feedback-btn.is-dislike {
  color: #e34d59 !important;
  border-color: #e34d59 !important;
  background: rgba(227, 77, 89, 0.08) !important;
}

.feedback-btn.is-active:hover,
.feedback-btn.is-dislike:hover {
  color: #e34d59 !important;
  border-color: #e34d59 !important;
  background: rgba(227, 77, 89, 0.14) !important;
}

.feedback-btn.is-like.is-active,
.feedback-btn.is-like.is-active:hover {
  color: #07c05f !important;
  border-color: #07c05f !important;
  background: rgba(7, 192, 95, 0.16) !important;
}

.feedback-btn.is-active :deep(.t-icon),
.feedback-btn.is-dislike :deep(.t-icon) {
  color: currentColor;
}

.feedback-reason-card {
  display: grid;
  grid-template-columns: repeat(2, minmax(96px, 1fr));
  gap: 6px;
  width: 224px;
  padding: 6px;
  border: 0;
  border-radius: 10px;
  background: var(--td-bg-color-container);
  box-shadow: none;
}

.feedback-reason-option {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 44px;
  padding: 8px 6px;
  border: 0;
  border-radius: 8px;
  color: var(--td-text-color-primary);
  background: var(--td-bg-color-container);
  cursor: pointer;
  font-size: 12px;
  font-weight: 500;
  line-height: 18px;
  letter-spacing: 0.1px;
  text-align: center;
  box-shadow: 0 2px 6px rgba(0, 0, 0, 0.07);
  transition: color 0.15s ease, background 0.15s ease, box-shadow 0.15s ease,
    transform 0.15s ease;
}

.feedback-reason-option:hover,
.feedback-reason-option.is-selected {
  color: var(--td-brand-color);
  background: var(--td-bg-color-container-hover);
  box-shadow: 0 3px 8px rgba(0, 0, 0, 0.1);
  transform: translateY(-1px);
}

.feedback-reason-option.is-selected {
  background: var(--td-brand-color-light);
  box-shadow: 0 3px 8px rgba(7, 192, 95, 0.16);
}

.feedback-reason-check {
  position: absolute;
  top: 4px;
  right: 5px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 14px;
  height: 14px;
  border-radius: 50%;
  color: #fff;
  background: var(--td-brand-color);
  font-size: 10px;
}
</style>
