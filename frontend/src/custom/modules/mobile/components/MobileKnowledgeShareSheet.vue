<script setup lang="ts">
import { ref, watch } from "vue";
import type { MobileKnowledgeShareSpaceRow } from "../knowledgeSharingPolicy";

const props = defineProps<{
  visible: boolean;
  knowledgeBaseName: string;
  sourceOwned: boolean;
  rows: MobileKnowledgeShareSpaceRow[];
  query: string;
  loading?: boolean;
  loadingMore?: boolean;
  hasMore?: boolean;
  busyId?: string;
}>();

const emit = defineEmits<{
  (event: "close"): void;
  (event: "select", row: MobileKnowledgeShareSpaceRow): void;
  (event: "remove", row: MobileKnowledgeShareSpaceRow): void;
  (event: "search", value: string): void;
  (event: "load-more"): void;
}>();

const pendingRemovalId = ref("");

watch(() => props.visible, (visible) => {
  if (!visible) pendingRemovalId.value = "";
});

watch(() => props.rows, (rows) => {
  if (pendingRemovalId.value && !rows.some((row) => row.shareId === pendingRemovalId.value)) {
    pendingRemovalId.value = "";
  }
}, { deep: true });

function handleSearchInput(event: Event) {
  emit("search", (event.target as HTMLInputElement).value);
}

function handleListScroll(event: Event) {
  const target = event.currentTarget as HTMLElement;
  const distanceToBottom = target.scrollHeight - target.scrollTop - target.clientHeight;
  if (distanceToBottom <= 80 && props.hasMore && !props.loading && !props.loadingMore) {
    emit("load-more");
  }
}
</script>

<template>
  <div v-if="visible" class="share-dialog-backdrop" @click.self="emit('close')">
    <section
      class="share-dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="mobile-share-dialog-title"
      data-testid="mobile-share-dialog"
    >
      <header>
        <div>
          <strong id="mobile-share-dialog-title">共享</strong>
          <span>{{ knowledgeBaseName }}</span>
        </div>
        <button type="button" aria-label="关闭" @click="emit('close')"><MobileIcon name="close" /></button>
      </header>

      <p v-if="sourceOwned" class="share-hint">选择有权限的共享空间后，将以只读权限直接共享。</p>
      <p v-else class="share-hint">该知识库来自共享空间，不能再次转共享；空间管理员可以取消当前共享。</p>

      <label class="share-search">
        <MobileIcon name="search" />
        <input
          :value="query"
          type="search"
          inputmode="search"
          autocomplete="off"
          aria-label="搜索共享空间"
          placeholder="搜索共享空间"
          data-testid="mobile-share-search"
          @input="handleSearchInput"
        >
      </label>

      <div
        class="share-results"
        data-testid="mobile-share-results"
        @scroll.passive="handleListScroll"
      >
        <div v-if="loading" class="share-empty">正在加载共享空间</div>
        <div v-else-if="!rows.length" class="share-empty">
          {{ query.trim() ? '没有匹配的共享空间' : (sourceOwned ? '暂无可共享的空间' : '暂无可管理的共享关系') }}
        </div>
        <div v-else class="space-list">
          <article v-for="row in rows" :key="`${row.id}-${row.shareId || 'new'}`" class="space-row">
            <button type="button" class="space-main" :disabled="!!busyId" @click="emit('select', row)">
              <span class="space-icon"><MobileIcon name="share" /></span>
              <span class="space-text">
                <strong>{{ row.name }}</strong>
                <small>{{ row.roleLabel }}</small>
              </span>
              <em v-if="row.shareId" class="shared-badge">已共享</em>
              <MobileIcon v-else name="chevron-right" />
            </button>
            <div v-if="row.shareId && row.canRemove && pendingRemovalId === row.shareId" class="remove-confirm">
              <span>确定取消该空间中的共享？</span>
              <button type="button" :disabled="!!busyId" @click="pendingRemovalId = ''">保留</button>
              <button type="button" class="confirm-remove" :disabled="!!busyId" @click="emit('remove', row)">
                {{ busyId === row.shareId ? '正在移除' : '确认取消' }}
              </button>
            </div>
            <button v-else-if="row.shareId && row.canRemove" type="button" class="remove-share"
              :disabled="!!busyId" @click="pendingRemovalId = row.shareId">
              <MobileIcon name="delete" />
              <span>取消共享</span>
            </button>
          </article>
          <div v-if="loadingMore" class="load-more-state">正在加载更多</div>
          <div v-else-if="!hasMore" class="load-more-state">已经到底了</div>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.share-dialog-backdrop {
  position: fixed;
  z-index: 1120;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgb(16 30 23 / 42%);
  padding: 18px;
}

.share-dialog {
  display: flex;
  width: min(100%, 420px);
  max-height: min(76dvh, 680px);
  flex-direction: column;
  overflow: hidden;
  border-radius: 18px;
  background: #fff;
  padding: 13px 14px 14px;
  box-shadow: 0 18px 56px rgb(20 42 31 / 24%);
}

header { display: flex; min-height: 48px; align-items: center; justify-content: space-between; gap: 10px; }
header > div { display: flex; min-width: 0; flex-direction: column; gap: 3px; }
header strong { color: #17261f; font-size: 18px; }
header span { overflow: hidden; color: #71837a; font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
header button {
  display: grid;
  width: 34px;
  height: 34px;
  flex: 0 0 auto;
  place-items: center;
  border: 0;
  border-radius: 17px;
  background: #eef3f0;
  color: #64766d;
}

.share-hint { margin: 7px 0 10px; color: #65776e; font-size: 12px; line-height: 1.55; }
.share-search {
  display: grid;
  min-height: 42px;
  flex: 0 0 auto;
  grid-template-columns: 20px minmax(0, 1fr);
  align-items: center;
  gap: 7px;
  border: 1px solid #dce7e1;
  border-radius: 11px;
  background: #f8fbf9;
  color: #73847b;
  padding: 0 11px;
}
.share-search input { min-width: 0; border: 0; outline: 0; background: transparent; color: #1c2d25; font-size: 14px; }
.share-search input::placeholder { color: #92a098; }
.share-results { min-height: 128px; margin-top: 10px; overflow-y: auto; overscroll-behavior: contain; }
.share-empty { display: grid; min-height: 128px; place-items: center; color: #819087; font-size: 13px; }
.space-list { display: flex; flex-direction: column; gap: 8px; }
.space-row { overflow: hidden; border: 1px solid #dfe9e4; border-radius: 10px; background: #fbfdfc; }
.space-main {
  display: grid;
  width: 100%;
  min-height: 60px;
  grid-template-columns: 36px minmax(0, 1fr) auto;
  align-items: center;
  gap: 9px;
  border: 0;
  background: transparent;
  color: #1b2d24;
  padding: 9px 11px;
  text-align: left;
}
.space-icon { display: grid; width: 34px; height: 34px; place-items: center; border-radius: 9px; background: #ebf9f1; color: #07994f; font-size: 18px; }
.space-text { display: flex; min-width: 0; flex-direction: column; gap: 4px; }
.space-text strong,
.space-text small { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.space-text strong { font-size: 15px; }
.space-text small { color: #788982; font-size: 12px; }
.shared-badge { border-radius: 999px; background: #eaf8f0; color: #078f49; font-size: 12px; font-style: normal; padding: 4px 8px; }
.remove-share {
  display: flex;
  width: 100%;
  height: 38px;
  align-items: center;
  justify-content: center;
  gap: 5px;
  border: 0;
  border-top: 1px solid #edf1ef;
  background: #fffafa;
  color: #c33b3b;
  font-size: 13px;
}
.remove-confirm {
  display: grid;
  min-height: 48px;
  grid-template-columns: minmax(0, 1fr) auto auto;
  align-items: center;
  gap: 7px;
  border-top: 1px solid #edf1ef;
  background: #fffafa;
  padding: 7px 9px;
}
.remove-confirm span { color: #9f3030; font-size: 12px; }
.remove-confirm button {
  height: 32px;
  border: 1px solid #e1d9d9;
  border-radius: 16px;
  background: #fff;
  color: #716363;
  padding: 0 10px;
  font-size: 12px;
}
.remove-confirm .confirm-remove { border-color: #d94b4b; background: #d94b4b; color: #fff; }
.load-more-state { color: #8a9891; font-size: 12px; padding: 6px 0 3px; text-align: center; }
button:disabled { opacity: 0.62; }
</style>
