<template>
  <t-dialog
    :visible="visible"
    :footer="false"
    :header="false"
    :close-btn="false"
    :close-on-overlay-click="false"
    :destroy-on-close="false"
    width="1000px"
    top="7vh"
    dialog-class-name="desktop-knowledge-search-dialog"
    @close="closeDialog"
    @opened="focusSearchInput"
  >
    <div class="desktop-knowledge-search" data-testid="desktop-kb-search-dialog">
      <header class="dialog-header">
        <div class="dialog-title-wrap">
          <span class="dialog-title-icon"><t-icon name="search" size="20px" /></span>
          <div>
            <h3>搜索知识库和文档</h3>
            <p>搜索全部个人及共享知识库，不受左侧空间筛选影响</p>
          </div>
        </div>
        <t-button
          variant="text"
          shape="square"
          theme="default"
          aria-label="关闭知识库搜索"
          class="dialog-close"
          @click="closeDialog"
        >
          <template #icon><t-icon name="close" size="24px" /></template>
        </t-button>
      </header>

      <div class="search-field">
        <t-icon name="search" size="18px" />
        <input
          ref="searchInputRef"
          v-model="searchQuery"
          type="search"
          autocomplete="off"
          spellcheck="false"
          aria-label="搜索全部知识库及文档"
          placeholder="输入知识库名称或文档名称"
        >
        <span v-if="documentSearchLoading" class="search-loading"><t-loading size="small" /></span>
        <button v-if="searchQuery" type="button" class="clear-search" aria-label="清空知识库搜索" @click="searchQuery = ''">
          <t-icon name="close-circle" size="17px" />
        </button>
      </div>

      <div class="result-grid">
        <section class="result-panel knowledge-panel" aria-label="知识库搜索结果">
          <div class="panel-heading">
            <div>
              <strong>知识库</strong>
              <span>按个人与共享分类</span>
            </div>
            <em>{{ filteredCatalog.length }}</em>
          </div>

          <div v-if="catalogLoading && !catalog.length" class="panel-state">
            <t-loading size="small" />
            <span>正在加载知识库</span>
          </div>
          <div v-else-if="catalogError && !catalog.length" class="panel-state is-error">
            {{ catalogError }}
          </div>
          <div v-else class="catalog-scroll">
            <section class="catalog-group">
              <button
                type="button"
                class="group-heading"
                :aria-expanded="personalExpanded"
                @click="personalExpanded = !personalExpanded"
              >
                <strong>个人知识库</strong>
                <em>{{ filteredPersonal.length }}</em>
                <t-icon :name="personalExpanded ? 'chevron-up' : 'chevron-down'" size="16px" />
              </button>
              <div v-if="personalExpanded" class="knowledge-rows">
                <button
                  v-for="kb in filteredPersonal"
                  :key="kb.id"
                  type="button"
                  class="knowledge-row"
                  :aria-label="`${kb.name} ${kb.permissionLabel}`"
                  @click="openKnowledgeBase(kb)"
                >
                  <span class="row-folder"><t-icon name="folder" size="18px" /></span>
                  <span class="row-copy">
                    <strong :title="kb.name">{{ kb.name }}</strong>
                    <small>{{ knowledgeDocumentCount(kb) }} 个文档 · {{ kb.originLabel }}</small>
                  </span>
                  <span class="permission-tag" :class="`is-${kb.access}`">{{ kb.permissionLabel }}</span>
                  <t-icon name="chevron-right" size="16px" class="row-chevron" />
                </button>
                <div v-if="!filteredPersonal.length" class="group-empty">
                  {{ normalizedQuery ? '没有名称匹配的个人知识库' : '暂无个人知识库' }}
                </div>
              </div>
            </section>

            <section class="catalog-group">
              <button
                type="button"
                class="group-heading"
                :aria-expanded="sharedExpanded"
                @click="sharedExpanded = !sharedExpanded"
              >
                <strong>共享知识库</strong>
                <em>{{ filteredShared.length }}</em>
                <t-icon :name="sharedExpanded ? 'chevron-up' : 'chevron-down'" size="16px" />
              </button>
              <div v-if="sharedExpanded" class="knowledge-rows">
                <button
                  v-for="kb in filteredShared"
                  :key="kb.id"
                  type="button"
                  class="knowledge-row"
                  :aria-label="`${kb.name} ${kb.permissionLabel}`"
                  @click="openKnowledgeBase(kb)"
                >
                  <span class="row-folder shared"><t-icon name="folder" size="18px" /></span>
                  <span class="row-copy">
                    <strong :title="kb.name">{{ kb.name }}</strong>
                    <small>{{ knowledgeDocumentCount(kb) }} 个文档 · {{ kb.originLabel }}</small>
                  </span>
                  <span class="permission-tag" :class="`is-${kb.access}`">{{ kb.permissionLabel }}</span>
                  <t-icon name="chevron-right" size="16px" class="row-chevron" />
                </button>
                <div v-if="!filteredShared.length" class="group-empty">
                  {{ normalizedQuery ? '没有名称匹配的共享知识库' : '暂无共享知识库' }}
                </div>
              </div>
            </section>
          </div>
        </section>

        <section class="result-panel document-panel" aria-label="文档搜索结果">
          <div class="panel-heading">
            <div>
              <strong>文档</strong>
              <span>按文档名称模糊搜索</span>
            </div>
            <em v-if="visibleDocumentResults.length">{{ visibleDocumentResults.length }}</em>
          </div>

          <div v-if="!normalizedQuery" class="document-guide panel-state">
            <span class="guide-icon"><t-icon name="file-search" size="26px" /></span>
            <strong>查找知识库文档</strong>
            <span>输入至少 {{ searchPolicy.minLength }} 个字符开始搜索，结果按页加载。</span>
          </div>
          <div v-else-if="normalizedQuery.length < searchPolicy.minLength" class="panel-state">
            再输入 {{ searchPolicy.minLength - normalizedQuery.length }} 个字符后搜索文档
          </div>
          <div v-else-if="documentSearchLoading" class="panel-state">
            <t-loading size="small" />
            <span>正在搜索文档</span>
          </div>
          <div v-else-if="documentSearchError" class="panel-state is-error">{{ documentSearchError }}</div>
          <div v-else-if="!visibleDocumentResults.length" class="panel-state">没有匹配的文档</div>
          <div v-else class="document-scroll">
            <button
              v-for="item in visibleDocumentResults"
              :key="item.id"
              type="button"
              class="document-row"
              @click="openDocument(item)"
            >
              <span class="document-icon"><t-icon name="file" size="18px" /></span>
              <span class="row-copy">
                <strong :title="documentName(item)">{{ documentName(item) }}</strong>
                <small>{{ documentKnowledgeBaseName(item) }}</small>
              </span>
              <span
                v-if="knowledgeBaseById.get(String(item.knowledge_base_id || ''))"
                class="permission-tag compact"
                :class="`is-${knowledgeBaseById.get(String(item.knowledge_base_id || ''))?.access}`"
              >
                {{ knowledgeBaseById.get(String(item.knowledge_base_id || ''))?.permissionLabel }}
              </span>
              <t-icon name="chevron-right" size="16px" class="row-chevron" />
            </button>
            <t-button
              v-if="documentSearchHasMore"
              variant="outline"
              theme="default"
              size="small"
              block
              :loading="documentSearchLoadingMore"
              class="load-more"
              @click="loadMoreDocuments"
            >
              {{ documentSearchLoadingMore ? '正在加载' : '加载更多文档' }}
            </t-button>
          </div>
        </section>
      </div>

    </div>
  </t-dialog>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { searchKnowledge } from '@/api/knowledge-base'
import { useAuthStore } from '@/stores/auth'
import { useChatResourcesStore } from '@/stores/chatResources'
import { useOrganizationStore } from '@/stores/organization'
import { useResourcePins } from '@/composables/useResourcePins'
import {
  buildMobileKnowledgeCatalog,
  type MobileKnowledgeBase,
} from '@/custom/modules/mobile/knowledgeCatalog'
import {
  filterKnowledgeBasesByName,
  KNOWLEDGE_DOCUMENT_SEARCH_POLICY,
  normalizeKnowledgeSearchQuery,
} from './searchPolicy'
import { openRouteInNewPage } from './openRouteInNewPage'

type KnowledgeDocumentRow = Record<string, any>

const props = defineProps<{ visible: boolean }>()
const emit = defineEmits<{ 'update:visible': [value: boolean] }>()

const authStore = useAuthStore()
const chatResources = useChatResourcesStore()
const organizationStore = useOrganizationStore()
const pins = useResourcePins()
const router = useRouter()

const searchInputRef = ref<HTMLInputElement | null>(null)
const searchQuery = ref('')
const catalogLoading = ref(false)
const catalogError = ref('')
const personalExpanded = ref(true)
const sharedExpanded = ref(true)
const documentSearchResults = ref<KnowledgeDocumentRow[]>([])
const documentSearchLoading = ref(false)
const documentSearchLoadingMore = ref(false)
const documentSearchHasMore = ref(false)
const documentSearchError = ref('')

let searchTimer: ReturnType<typeof setTimeout> | null = null
let requestGeneration = 0

const searchPolicy = KNOWLEDGE_DOCUMENT_SEARCH_POLICY
const normalizedQuery = computed(() => normalizeKnowledgeSearchQuery(searchQuery.value))
const catalog = computed<MobileKnowledgeBase[]>(() =>
  buildMobileKnowledgeCatalog(
    chatResources.rawKnowledgeBases,
    organizationStore.sharedKnowledgeBases,
    {
      currentUserId: authStore.currentUserId,
      currentTenantRole: authStore.currentTenantRole,
    },
  ),
)
const filteredCatalog = computed(() => filterKnowledgeBasesByName(catalog.value, normalizedQuery.value))
const filteredPersonal = computed(() => filteredCatalog.value.filter((item) => item.group === 'personal'))
const filteredShared = computed(() => filteredCatalog.value.filter((item) => item.group === 'shared'))
const knowledgeBaseById = computed(() => new Map(catalog.value.map((item) => [item.id, item])))
const visibleDocumentResults = computed(() =>
  documentSearchResults.value.filter((item) => knowledgeBaseById.value.has(String(item.knowledge_base_id || ''))),
)

const knowledgeDocumentCount = (kb: MobileKnowledgeBase) =>
  Number(kb.type === 'faq' ? kb.chunk_count || 0 : kb.document_count || kb.knowledge_count || 0)

const documentName = (item: KnowledgeDocumentRow) =>
  item.file_name || item.title || item.source || '未命名文档'

const documentKnowledgeBaseName = (item: KnowledgeDocumentRow) =>
  item.knowledge_base_name ||
  knowledgeBaseById.value.get(String(item.knowledge_base_id || ''))?.name ||
  '未知知识库'

const focusSearchInput = async () => {
  await nextTick()
  searchInputRef.value?.focus()
}

const cancelSearch = () => {
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
  requestGeneration += 1
  documentSearchLoading.value = false
  documentSearchLoadingMore.value = false
}

const clearSearchState = () => {
  cancelSearch()
  searchQuery.value = ''
  documentSearchResults.value = []
  documentSearchHasMore.value = false
  documentSearchError.value = ''
}

const closeDialog = () => {
  clearSearchState()
  emit('update:visible', false)
}

const ensureCatalog = async () => {
  catalogLoading.value = true
  catalogError.value = ''
  try {
    await Promise.all([
      chatResources.ensureKnowledgeBases(),
      organizationStore.fetchSharedKnowledgeBases(),
    ])
  } catch (error: any) {
    catalogError.value = error?.message || '加载知识库失败'
  } finally {
    catalogLoading.value = false
  }
}

const runDocumentSearch = async (
  keyword: string,
  offset: number,
  append: boolean,
  generation: number,
) => {
  if (append) documentSearchLoadingMore.value = true
  else documentSearchLoading.value = true
  documentSearchError.value = ''
  try {
    const response: any = await searchKnowledge(
      keyword,
      offset,
      searchPolicy.pageSize,
      undefined,
      { include_total: false },
    )
    if (generation !== requestGeneration || keyword !== normalizedQuery.value) return
    const rows = Array.isArray(response?.data) ? response.data : []
    if (append) {
      const merged = new Map(documentSearchResults.value.map((item) => [String(item.id), item]))
      rows.forEach((item: KnowledgeDocumentRow) => merged.set(String(item.id), item))
      documentSearchResults.value = [...merged.values()]
    } else {
      documentSearchResults.value = rows
    }
    documentSearchHasMore.value = response?.has_more === true
  } catch (error: any) {
    if (generation !== requestGeneration) return
    documentSearchError.value = error?.message || '搜索文档失败'
  } finally {
    if (generation === requestGeneration) {
      documentSearchLoading.value = false
      documentSearchLoadingMore.value = false
    }
  }
}

const scheduleDocumentSearch = () => {
  cancelSearch()
  documentSearchResults.value = []
  documentSearchHasMore.value = false
  documentSearchError.value = ''
  const keyword = normalizedQuery.value
  if (keyword) {
    personalExpanded.value = true
    sharedExpanded.value = true
  }
  if (keyword.length < searchPolicy.minLength) return
  const generation = requestGeneration
  searchTimer = setTimeout(() => {
    searchTimer = null
    void runDocumentSearch(keyword, 0, false, generation)
  }, searchPolicy.debounceMs)
}

const loadMoreDocuments = async () => {
  const keyword = normalizedQuery.value
  if (
    keyword.length < searchPolicy.minLength ||
    documentSearchLoading.value ||
    documentSearchLoadingMore.value ||
    !documentSearchHasMore.value
  ) return
  const generation = ++requestGeneration
  await runDocumentSearch(keyword, documentSearchResults.value.length, true, generation)
}

const openKnowledgeBase = (kb: MobileKnowledgeBase) => {
  pins.touchRecent('kb', kb.id)
  openRouteInNewPage(router, `/platform/knowledge-bases/${kb.id}`)
}

const openDocument = (item: KnowledgeDocumentRow) => {
  const kbId = String(item.knowledge_base_id || '')
  const knowledgeId = String(item.id || '')
  if (!kbId || !knowledgeId || !knowledgeBaseById.value.has(kbId)) return
  pins.touchRecent('kb', kbId)
  openRouteInNewPage(router, {
    path: `/platform/knowledge-bases/${kbId}`,
    query: { knowledge_id: knowledgeId },
  })
}

watch(() => props.visible, (visible) => {
  if (!visible) {
    clearSearchState()
    return
  }
  personalExpanded.value = true
  sharedExpanded.value = true
  void ensureCatalog()
  void focusSearchInput()
})

watch(searchQuery, scheduleDocumentSearch)

onBeforeUnmount(cancelSearch)
</script>

<style scoped lang="less">
.desktop-knowledge-search {
  color: var(--td-text-color-primary);
  font-family: var(--app-font-family);
}

.dialog-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 18px;
}

.dialog-title-wrap {
  display: flex;
  align-items: center;
  gap: 12px;

  h3,
  p {
    margin: 0;
  }

  h3 {
    font-size: 18px;
    font-weight: 600;
    line-height: 26px;
  }

  p {
    margin-top: 2px;
    color: var(--td-text-color-secondary);
    font-size: 13px;
  }
}

.dialog-title-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 38px;
  height: 38px;
  border-radius: 9px;
  color: var(--td-brand-color);
  background: var(--td-brand-color-light);
}

.dialog-close {
  width: 40px;
  height: 40px;
  color: var(--td-text-color-primary);

  &:hover {
    background: var(--td-bg-color-container-hover);
  }
}

.search-field {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 44px;
  padding: 0 13px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  color: var(--td-text-color-placeholder);
  background: var(--td-bg-color-container);
  transition: border-color 0.2s, box-shadow 0.2s;

  &:focus-within {
    border-color: var(--td-brand-color);
    box-shadow: 0 0 0 2px var(--td-brand-color-focus);
  }

  input {
    flex: 1;
    min-width: 0;
    border: 0;
    outline: 0;
    color: var(--td-text-color-primary);
    background: transparent;
    font: inherit;
    font-size: 14px;
  }
}

.clear-search {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 2px;
  border: 0;
  color: var(--td-text-color-placeholder);
  background: transparent;
  cursor: pointer;
}

.search-loading {
  display: inline-flex;
}

.result-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.05fr) minmax(0, 0.95fr);
  gap: 14px;
  min-height: 430px;
  max-height: 62vh;
  margin-top: 14px;
}

.result-panel {
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  background: var(--td-bg-color-container);
  overflow: hidden;
}

.panel-heading {
  display: flex;
  align-items: center;
  justify-content: space-between;
  min-height: 58px;
  padding: 0 16px;
  border-bottom: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);

  div {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  strong {
    font-size: 14px;
    font-weight: 600;
  }

  span {
    color: var(--td-text-color-placeholder);
    font-size: 12px;
  }

  em {
    min-width: 24px;
    padding: 2px 7px;
    border-radius: 10px;
    color: var(--td-text-color-secondary);
    background: var(--td-bg-color-container);
    font-size: 12px;
    font-style: normal;
    text-align: center;
  }
}

.catalog-scroll,
.document-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  scrollbar-width: thin;
}

.catalog-group + .catalog-group {
  border-top: 8px solid var(--td-bg-color-page);
}

.group-heading {
  display: grid;
  grid-template-columns: auto auto 1fr 18px;
  align-items: center;
  gap: 8px;
  width: 100%;
  min-height: 46px;
  padding: 0 13px;
  border: 0;
  color: var(--td-text-color-primary);
  background: var(--td-bg-color-container);
  cursor: pointer;
  text-align: left;

  &:hover {
    background: var(--td-bg-color-container-hover);
  }

  strong {
    font-size: 13px;
    font-weight: 600;
  }

  em {
    display: inline-grid;
    min-width: 22px;
    height: 22px;
    place-items: center;
    padding: 0 6px;
    border-radius: 11px;
    color: var(--td-text-color-secondary);
    background: var(--td-bg-color-secondarycontainer);
    font-size: 12px;
    font-style: normal;
  }

  > :last-child {
    grid-column: 4;
  }
}

.row-folder,
.document-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 7px;
  color: var(--td-brand-color);
  background: var(--td-brand-color-light);
}

.knowledge-row,
.document-row {
  display: grid;
  grid-template-columns: 34px minmax(0, 1fr) auto 16px;
  align-items: center;
  gap: 9px;
  width: 100%;
  min-height: 58px;
  padding: 8px 13px;
  border: 0;
  border-top: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-primary);
  background: var(--td-bg-color-container);
  cursor: pointer;
  text-align: left;

  &:hover {
    background: var(--td-bg-color-container-hover);

    .row-chevron {
      color: var(--td-brand-color);
      transform: translateX(2px);
    }
  }
}

.row-folder,
.document-icon {
  width: 32px;
  height: 32px;

  &.shared {
    color: var(--td-warning-color);
    background: var(--td-warning-color-light);
  }
}

.document-icon {
  color: var(--td-text-color-secondary);
  background: var(--td-bg-color-secondarycontainer);
}

.row-copy {
  display: flex;
  flex-direction: column;
  min-width: 0;
  gap: 3px;

  strong,
  small {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  strong {
    font-size: 13px;
    font-weight: 500;
  }

  small {
    color: var(--td-text-color-placeholder);
    font-size: 12px;
  }
}

.permission-tag {
  display: inline-flex;
  align-items: center;
  min-height: 21px;
  padding: 0 7px;
  border-radius: 4px;
  color: var(--td-text-color-secondary);
  background: var(--td-bg-color-secondarycontainer);
  font-size: 11px;
  white-space: nowrap;

  &.is-manage {
    color: var(--td-brand-color);
    background: var(--td-brand-color-light);
  }

  &.is-edit {
    color: var(--td-warning-color);
    background: var(--td-warning-color-light);
  }

  &.compact {
    padding: 0 5px;
  }
}

.row-chevron {
  color: var(--td-text-color-placeholder);
  transition: color 0.2s, transform 0.2s;
}

.group-empty,
.panel-state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  min-height: 76px;
  padding: 16px;
  color: var(--td-text-color-placeholder);
  font-size: 13px;
  text-align: center;
}

.panel-state {
  flex: 1;
  flex-direction: column;

  &.is-error {
    color: var(--td-error-color);
  }
}

.document-guide {
  strong {
    color: var(--td-text-color-primary);
    font-size: 14px;
  }

  span:last-child {
    max-width: 260px;
    line-height: 20px;
  }
}

.guide-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 52px;
  height: 52px;
  margin-bottom: 4px;
  border-radius: 50%;
  color: var(--td-brand-color);
  background: var(--td-brand-color-light);
}

.document-scroll {
  padding-bottom: 10px;
}

.load-more {
  width: calc(100% - 24px);
  margin: 10px 12px 0;
}

:global(.desktop-knowledge-search-dialog) {
  max-width: calc(100vw - 48px);
  border-radius: 12px;
  overflow: hidden;
}

:global(.desktop-knowledge-search-dialog .t-dialog__body) {
  padding: 20px 22px 16px;
}

@media (max-width: 900px) {
  .result-grid {
    grid-template-columns: 1fr;
    max-height: 70vh;
    overflow-y: auto;
  }

  .result-panel {
    min-height: 330px;
  }
}
</style>
