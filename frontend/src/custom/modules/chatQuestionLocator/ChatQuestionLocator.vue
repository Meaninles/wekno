<template>
    <aside
        v-if="shouldShow"
        ref="locatorRef"
        class="chat-question-locator"
        :class="{ 'is-open': isOpen }"
        aria-label="对话问题定位"
        @mouseenter="openLocator"
        @mouseleave="scheduleCloseLocator"
        @focusin="openLocator"
        @focusout="handleFocusOut"
    >
        <div v-if="isOpen" class="locator-hover-bridge" aria-hidden="true"></div>
        <div class="locator-rail" aria-hidden="true">
            <button
                v-for="item in questionItems"
                :key="`marker-${item.key}`"
                type="button"
                class="locator-marker"
                :class="{
                    'is-current': item.key === currentKey,
                    'is-hovered': item.key === hoverKey,
                }"
                tabindex="-1"
                @click.stop="scrollToQuestion(item)"
            ></button>
        </div>
        <div class="locator-panel">
            <div ref="panelListRef" class="locator-list">
                <button
                    v-for="item in questionItems"
                    :key="item.key"
                    type="button"
                    class="locator-item"
                    :class="{ 'is-current': item.key === currentKey }"
                    :data-locator-current="item.key === currentKey ? 'true' : null"
                    @mouseenter="hoverKey = item.key"
                    @mouseleave="hoverKey = ''"
                    @click.stop="scrollToQuestion(item)"
                >
                    <span class="locator-item-index">{{ item.number }}</span>
                    <span class="locator-item-text">{{ item.title }}</span>
                </button>
            </div>
        </div>
    </aside>
</template>

<script setup>
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue';

const props = defineProps({
    messages: {
        type: Array,
        default: () => [],
    },
    scrollContainer: {
        type: Object,
        default: null,
    },
    embeddedMode: {
        type: Boolean,
        default: false,
    },
});

const MIN_SCROLL_DISTANCE = 48;
const SCROLL_OFFSET = 28;

const locatorRef = ref(null);
const panelListRef = ref(null);
const isOpen = ref(false);
const hoverKey = ref('');
const currentKey = ref('');
const hasScrollableContent = ref(false);

let animationFrame = 0;
let resizeObserver = null;
let observedScrollRoot = null;
let observedScrollContent = null;
let closeTimer = null;

const normalizeQuestionText = (message) => {
    const text = String(message?.content || '').replace(/\s+/g, ' ').trim();
    if (text) return text;
    const imageCount = Array.isArray(message?.images) ? message.images.length : 0;
    const attachmentCount = Array.isArray(message?.attachments) ? message.attachments.length : 0;
    if (imageCount && attachmentCount) return '图片和附件问题';
    if (imageCount) return '图片问题';
    if (attachmentCount) return '附件问题';
    return '空问题';
};

const getMessageKey = (message, messageIndex, number) => (
    String(message?.id || message?.request_id || `${message?.created_at || 'local'}-${messageIndex}-${number}`)
);

const questionItems = computed(() => {
    let number = 0;
    return props.messages
        .map((message, messageIndex) => {
            if (message?.role !== 'user') return null;
            number += 1;
            return {
                key: getMessageKey(message, messageIndex, number),
                messageIndex,
                number,
                title: normalizeQuestionText(message),
            };
        })
        .filter(Boolean);
});

const shouldShow = computed(() => (
    !props.embeddedMode
    && questionItems.value.length > 1
    && hasScrollableContent.value
));

const getScrollRoot = () => props.scrollContainer || null;

const getQuestionElement = (item) => {
    const scrollRoot = getScrollRoot();
    if (!scrollRoot) return null;
    return scrollRoot.querySelector(`[data-chat-question-index="${item.messageIndex}"]`);
};

const getQuestionScrollTop = (item) => {
    const scrollRoot = getScrollRoot();
    const target = getQuestionElement(item);
    if (!scrollRoot || !target) return 0;
    const rootRect = scrollRoot.getBoundingClientRect();
    const targetRect = target.getBoundingClientRect();
    return Math.max(0, scrollRoot.scrollTop + targetRect.top - rootRect.top - SCROLL_OFFSET);
};

const updateCurrentQuestion = () => {
    const scrollRoot = getScrollRoot();
    const items = questionItems.value;
    if (!scrollRoot || items.length === 0) {
        currentKey.value = '';
        return;
    }

    const rootRect = scrollRoot.getBoundingClientRect();
    const referenceTop = rootRect.top + Math.min(scrollRoot.clientHeight * 0.34, 220);
    let currentItem = items[0];

    for (const item of items) {
        const target = getQuestionElement(item);
        if (!target) continue;
        if (target.getBoundingClientRect().top <= referenceTop) {
            currentItem = item;
        } else {
            break;
        }
    }

    if (scrollRoot.scrollHeight - scrollRoot.scrollTop - scrollRoot.clientHeight < 16) {
        currentItem = items[items.length - 1];
    }

    currentKey.value = currentItem?.key || '';
};

const updateLayoutState = () => {
    animationFrame = 0;
    const scrollRoot = getScrollRoot();
    if (!scrollRoot) {
        hasScrollableContent.value = false;
        currentKey.value = '';
        return;
    }
    observeScrollContent(scrollRoot);
    hasScrollableContent.value = scrollRoot.scrollHeight - scrollRoot.clientHeight > MIN_SCROLL_DISTANCE;
    updateCurrentQuestion();
};

const requestLayoutUpdate = () => {
    if (animationFrame) cancelAnimationFrame(animationFrame);
    animationFrame = requestAnimationFrame(updateLayoutState);
};

const scrollActiveItemIntoView = () => {
    if (!isOpen.value) return;
    nextTick(() => {
        const active = panelListRef.value?.querySelector('[data-locator-current="true"]');
        active?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    });
};

const bindScrollRoot = (scrollRoot) => {
    if (observedScrollRoot === scrollRoot) return;
    if (observedScrollRoot) {
        observedScrollRoot.removeEventListener('scroll', requestLayoutUpdate);
    }
    if (resizeObserver) {
        resizeObserver.disconnect();
        resizeObserver = null;
    }
    observedScrollContent = null;

    observedScrollRoot = scrollRoot;
    if (!scrollRoot) {
        updateLayoutState();
        return;
    }

    scrollRoot.addEventListener('scroll', requestLayoutUpdate, { passive: true });
    resizeObserver = new ResizeObserver(requestLayoutUpdate);
    resizeObserver.observe(scrollRoot);
    observeScrollContent(scrollRoot);
    requestLayoutUpdate();
};

function observeScrollContent(scrollRoot) {
    if (!resizeObserver || !scrollRoot) return;
    const content = scrollRoot.querySelector('.msg_list');
    if (!content || observedScrollContent === content) return;
    if (observedScrollContent) {
        resizeObserver.unobserve(observedScrollContent);
    }
    resizeObserver.observe(content);
    observedScrollContent = content;
}

const openLocator = () => {
    if (closeTimer) {
        clearTimeout(closeTimer);
        closeTimer = null;
    }
    isOpen.value = true;
    scrollActiveItemIntoView();
};

const closeLocator = () => {
    if (closeTimer) {
        clearTimeout(closeTimer);
        closeTimer = null;
    }
    isOpen.value = false;
    hoverKey.value = '';
};

const scheduleCloseLocator = () => {
    if (closeTimer) clearTimeout(closeTimer);
    closeTimer = setTimeout(() => {
        closeLocator();
    }, 320);
};

const handleFocusOut = (event) => {
    if (!locatorRef.value?.contains(event.relatedTarget)) {
        closeLocator();
    }
};

const scrollToQuestion = (item) => {
    const scrollRoot = getScrollRoot();
    const target = getQuestionElement(item);
    if (!scrollRoot || !target) return;
    scrollRoot.scrollTo({
        top: getQuestionScrollTop(item),
        behavior: 'smooth',
    });
    currentKey.value = item.key;
};

watch(
    () => props.scrollContainer,
    (scrollRoot) => bindScrollRoot(scrollRoot),
    { immediate: true },
);

watch(
    questionItems,
    () => {
        nextTick(requestLayoutUpdate);
    },
    { deep: true },
);

watch([currentKey, isOpen], scrollActiveItemIntoView);

onBeforeUnmount(() => {
    if (animationFrame) cancelAnimationFrame(animationFrame);
    if (closeTimer) clearTimeout(closeTimer);
    if (observedScrollRoot) {
        observedScrollRoot.removeEventListener('scroll', requestLayoutUpdate);
    }
    if (resizeObserver) {
        resizeObserver.disconnect();
    }
});
</script>

<style scoped lang="less">
.chat-question-locator {
    position: absolute;
    right: 14px;
    top: 50%;
    z-index: 12;
    width: 34px;
    transform: translateY(-50%);
    display: flex;
    justify-content: flex-end;
    pointer-events: auto;
}

.locator-hover-bridge {
    position: absolute;
    top: 50%;
    right: 0;
    z-index: 2;
    width: 64px;
    height: max(100%, 360px);
    transform: translateY(-50%);
}

.locator-rail {
    position: relative;
    z-index: 3;
    width: 34px;
    padding: 4px 0;
    box-sizing: border-box;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 7px;
    overflow: visible;
}

.locator-marker {
    position: relative;
    flex: 0 0 14px;
    width: 26px;
    height: 14px;
    padding: 0;
    border: 0;
    border-radius: 999px;
    background: transparent;
    cursor: pointer;
    transition:
        background 0.18s ease,
        transform 0.18s ease;
}

.locator-marker::before {
    content: "";
    position: absolute;
    left: 50%;
    top: 50%;
    width: 15px;
    height: 2px;
    border-radius: 999px;
    background: var(--td-text-color-placeholder);
    opacity: 0.76;
    transform: translate(-50%, -50%);
    transition:
        width 0.2s ease,
        height 0.2s ease,
        opacity 0.2s ease,
        background 0.2s ease,
        box-shadow 0.2s ease;
}

.locator-marker.is-current::before {
    width: 19px;
    height: 4px;
    opacity: 1;
    background: var(--td-brand-color);
}

.locator-marker.is-hovered::before {
    width: 21px;
    opacity: 1;
    background: var(--td-brand-color);
}

.locator-panel {
    position: absolute;
    right: 38px;
    top: 50%;
    z-index: 4;
    width: min(360px, calc(100vw - 560px));
    min-width: 280px;
    max-width: 360px;
    max-height: min(420px, 58vh);
    padding: 8px;
    border-radius: 12px;
    background: color-mix(in srgb, var(--td-bg-color-container) 92%, transparent);
    border: 1px solid color-mix(in srgb, var(--td-component-stroke) 76%, transparent);
    box-shadow: 0 18px 56px rgba(0, 0, 0, 0.18);
    backdrop-filter: blur(18px);
    opacity: 0;
    pointer-events: none;
    transform: translate(16px, -50%) scale(0.96);
    transform-origin: right center;
    transition:
        opacity 0.22s ease,
        transform 0.3s cubic-bezier(0.22, 1, 0.36, 1);
}

.chat-question-locator.is-open .locator-panel {
    opacity: 1;
    pointer-events: auto;
    transform: translate(0, -50%) scale(1);
}

.locator-list {
    max-height: calc(min(420px, 58vh) - 16px);
    overflow-y: auto;
    scrollbar-width: thin;
    display: flex;
    flex-direction: column;
    gap: 2px;
}

.locator-item {
    width: 100%;
    min-height: 42px;
    display: grid;
    grid-template-columns: 24px minmax(0, 1fr);
    align-items: center;
    gap: 8px;
    padding: 8px 10px;
    border: 0;
    border-radius: 8px;
    background: transparent;
    color: var(--td-text-color-primary);
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition:
        background 0.18s ease,
        color 0.18s ease,
        transform 0.18s ease;
}

.locator-item:hover,
.locator-item:focus-visible {
    outline: none;
    background: var(--td-bg-color-container-hover);
    transform: translateX(-2px);
}

.locator-item.is-current {
    background: color-mix(in srgb, var(--td-brand-color) 14%, var(--td-bg-color-container));
    color: var(--td-text-color-primary);
}

.locator-item-index {
    width: 24px;
    height: 24px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: 999px;
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-text-color-secondary);
    font-size: 12px;
    line-height: 1;
    font-variant-numeric: tabular-nums;
}

.locator-item.is-current .locator-item-index {
    background: var(--td-brand-color);
    color: var(--td-text-color-anti);
}

.locator-item-text {
    min-width: 0;
    overflow: hidden;
    max-height: 40px;
    display: -webkit-box;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;
    line-clamp: 2;
    text-overflow: ellipsis;
    overflow-wrap: anywhere;
    word-break: break-word;
    font-size: 13px;
    line-height: 20px;
}

@media (max-width: 1023px) {
    .chat-question-locator {
        display: none;
    }
}

@media (prefers-reduced-motion: reduce) {
    .locator-rail,
    .locator-marker,
    .locator-panel,
    .locator-item {
        transition: none;
    }
}
</style>
