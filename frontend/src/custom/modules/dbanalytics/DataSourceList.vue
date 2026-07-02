<template>
  <div class="db-source-shell">
    <ListSpaceSidebar
      v-if="!authStore.isLiteMode"
      v-model="sourceScope"
      collapsed-key="data-source-list-sidebar"
      :count-all="allSourcesCount"
      :count-mine="mineSourcesCount"
      :count-by-org="effectiveSourceCountByOrg"
      :count-favorites="sourceFavoritesCount"
      :count-recents="sourceRecentsCount"
    />
    <div class="db-source-page">
      <InformationSourceTabs class="source-tabs" />
      <header class="page-header">
        <div>
          <h1>数据源</h1>
          <p>添加 MySQL / PostgreSQL 数据库，绑定到数据分析智能体后可统一用 SQL 分析。</p>
        </div>
        <t-button theme="primary" @click="openCreateDialog">
          <template #icon><t-icon name="add" /></template>
          新建数据源
        </t-button>
      </header>

      <div class="page-body">
        <aside class="source-list">
        <div v-if="loadingCurrentSources" class="loading-wrap"><t-loading size="small" /></div>
        <div
          v-for="source in sources"
          :key="source.id"
          class="source-row"
          :class="{ active: source.id === activeSourceId }"
        >
          <button type="button" class="source-row-button" @click="selectSource(source.id)">
            <span class="source-main">
              <span class="source-name">{{ source.name }}</span>
              <span class="source-meta">{{ source.type }} · {{ source.config?.database || source.query_mode }}</span>
            </span>
            <span class="source-tags">
              <t-tag v-if="source.source_from_agent" size="small" theme="primary" variant="light">智能体</t-tag>
              <t-tag v-else-if="source.shared" size="small" theme="primary" variant="light">共享</t-tag>
              <t-tag size="small" :theme="source.status === 'active' ? 'success' : 'danger'" variant="light">
                {{ source.status === 'active' ? '正常' : '异常' }}
              </t-tag>
            </span>
          </button>
          <button
            type="button"
            class="source-favorite-star"
            :class="{ 'is-favorited': isSourceFavorited(source.id) }"
            :aria-label="isSourceFavorited(source.id) ? '取消收藏数据源' : '收藏数据源'"
            @click.stop="toggleFavoriteSource(source.id, $event)"
          >
            <t-icon :name="isSourceFavorited(source.id) ? 'star-filled' : 'star'" size="14px" />
          </button>
        </div>
        <div v-if="!loadingCurrentSources && sources.length === 0" class="empty">{{ sourceEmptyText }}</div>
        </aside>

      <main class="source-detail">
        <template v-if="activeSource">
          <section class="detail-head">
            <div>
              <h2>{{ activeSource.name }}</h2>
              <p>{{ activeSource.description || '未填写描述' }}</p>
              <p v-if="activeSource.source_from_agent" class="shared-text">来自共享智能体：{{ activeSource.source_from_agent.agent_name }}</p>
              <p v-else-if="activeSource.shared" class="shared-text">来自共享空间：{{ activeSource.org_name }}</p>
              <p v-if="activeSource.error_message" class="error-text">{{ activeSource.error_message }}</p>
            </div>
            <div class="head-actions">
              <t-button v-if="canManageActiveSource" variant="outline" @click="openShareDialog">共享</t-button>
              <t-button v-if="canEditActiveSource" variant="outline" :loading="testing" @click="handleTest">测试连接</t-button>
              <t-button v-if="canEditActiveSource" variant="outline" :loading="refreshing" @click="handleRefresh">刷新元数据</t-button>
              <t-button v-if="canEditActiveSource" variant="outline" @click="openEditDialog">编辑</t-button>
              <t-popconfirm v-if="canManageActiveSource" content="确定删除这个数据源吗？" @confirm="handleDelete">
                <t-button theme="danger" variant="outline">删除</t-button>
              </t-popconfirm>
            </div>
          </section>

          <section v-if="canManageActiveSource" class="share-section">
            <div class="section-title">
              <div>
                <h3>共享空间</h3>
                <p>数据源共享到空间后，空间成员可以在数据分析智能体中使用。</p>
              </div>
              <t-button size="small" variant="outline" @click="openShareDialog">共享到空间</t-button>
            </div>
            <div v-if="loadingShares" class="loading-wrap"><t-loading size="small" /></div>
            <div v-else-if="sourceShares.length === 0" class="empty share-empty">尚未共享到任何空间。</div>
            <div v-else class="share-list">
              <div v-for="share in sourceShares" :key="share.id" class="share-row">
                <span>
                  <strong>{{ share.organization_name || share.organization_id }}</strong>
                  <small>权限：{{ formatSharePermission(share.permission) }}</small>
                </span>
                <span class="share-actions">
                  <t-select v-model="share.permission" size="small" class="permission-select" @change="onSharePermissionChange(share, $event)">
                    <t-option value="viewer" label="只读" />
                    <t-option value="editor" label="可编辑" />
                  </t-select>
                  <t-popconfirm content="确定取消共享吗？" @confirm="removeShare(share)">
                    <t-button size="small" variant="text" theme="danger">移除</t-button>
                  </t-popconfirm>
                </span>
              </div>
            </div>
          </section>

          <section class="scope-section">
            <div class="section-title">
              <div>
                <h3>可分析表范围</h3>
                <p>只启用业务分析需要的表，避免智能体看到无关或敏感表。</p>
              </div>
              <t-button size="small" :loading="savingScope" :disabled="!canEditActiveSource" @click="saveTableScope">保存表范围</t-button>
            </div>
            <div class="table-grid">
              <button
                v-for="table in tables"
                :key="table.id"
                type="button"
                class="table-row"
                :class="{ selected: selectedTable?.id === table.id }"
                @click="selectedTableId = table.id"
              >
                <t-checkbox :checked="enabledTableIds.has(table.id)" :disabled="!canEditActiveSource" @change="handleTableChecked(table.id, $event)" />
                <span class="table-main">
                  <span class="table-name">{{ table.virtual_name }}</span>
                  <span class="table-meta">{{ table.schema_name }}.{{ table.table_name }} · {{ table.object_type }} · {{ table.row_estimate || 0 }} rows</span>
                </span>
              </button>
              <div v-if="tables.length === 0" class="empty">请先刷新元数据。</div>
            </div>
          </section>

          <section v-if="selectedTable" class="columns-section">
            <div class="section-title">
              <div>
                <h3>字段业务说明</h3>
                <p>{{ selectedTable.virtual_name }}。字段说明会参与 SQL 生成前的业务含义推断。</p>
              </div>
            </div>
            <div class="column-table-wrap">
              <table class="column-table">
                <thead>
                  <tr>
                    <th>字段</th>
                    <th>类型</th>
                    <th>语义</th>
                    <th>敏感级别</th>
                    <th>业务说明</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="column in selectedTable.columns || []" :key="column.id">
                    <td>
                      <div class="column-name">{{ column.column_name }}</div>
                      <div class="sample-values">{{ formatSamples(column.sample_values) }}</div>
                    </td>
                    <td>{{ column.data_type }}</td>
                    <td>
                      <t-select v-model="columnDrafts[column.id].semantic_type" size="small" class="small-select" :disabled="!canEditActiveSource">
                        <t-option value="dimension" label="维度" />
                        <t-option value="metric" label="指标" />
                        <t-option value="time" label="时间" />
                      </t-select>
                    </td>
                    <td>
                      <t-select v-model="columnDrafts[column.id].sensitive_level" size="small" class="small-select" :disabled="!canEditActiveSource">
                        <t-option value="none" label="无" />
                        <t-option value="masked" label="脱敏" />
                        <t-option value="hidden" label="隐藏" />
                      </t-select>
                    </td>
                    <td>
                      <t-input v-model="columnDrafts[column.id].description" size="small" placeholder="例如：订单实付金额，单位元" :disabled="!canEditActiveSource" />
                    </td>
                    <td class="column-action">
                      <t-button size="small" variant="text" :disabled="!canEditActiveSource" :loading="savingColumnId === column.id" @click="saveColumn(column.id)">保存</t-button>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </section>
        </template>

        <div v-else class="empty-detail">
          <t-icon name="server" />
          <span>选择或新建一个数据源</span>
        </div>
      </main>
      </div>

    <t-dialog v-model:visible="dialogVisible" :header="editingSource ? '编辑数据源' : '新建数据源'" width="640px">
      <t-form label-align="top">
        <div class="form-grid">
          <t-form-item label="名称">
            <t-input v-model="sourceForm.name" placeholder="例如：电商交易库" />
          </t-form-item>
          <t-form-item label="类型">
            <t-select v-model="sourceForm.type" @change="handleTypeChange">
              <t-option value="mysql" label="MySQL" />
              <t-option value="postgres" label="PostgreSQL" />
            </t-select>
          </t-form-item>
          <t-form-item label="Host">
            <t-input v-model="sourceForm.config.host" placeholder="127.0.0.1" />
          </t-form-item>
          <t-form-item label="Port">
            <t-input-number v-model="sourceForm.config.port" :min="1" :max="65535" />
          </t-form-item>
          <t-form-item label="Database">
            <t-input v-model="sourceForm.config.database" />
          </t-form-item>
          <t-form-item label="Username">
            <t-input v-model="sourceForm.config.username" />
          </t-form-item>
          <t-form-item label="Password">
            <t-input v-model="sourceForm.config.password" type="password" placeholder="编辑时留空表示不修改" />
          </t-form-item>
          <t-form-item label="SSL Mode">
            <t-select v-model="sourceForm.config.ssl_mode" clearable>
              <t-option value="disable" label="disable" />
              <t-option value="require" label="require" />
              <t-option value="verify-ca" label="verify-ca" />
              <t-option value="verify-full" label="verify-full" />
            </t-select>
          </t-form-item>
        </div>
        <t-form-item label="描述">
          <t-textarea v-model="sourceForm.description" :autosize="{ minRows: 2, maxRows: 4 }" placeholder="说明这个库的业务范围，帮助智能体选择数据源" />
        </t-form-item>
        <div class="form-grid">
          <t-form-item label="最大返回行数">
            <t-input-number v-model="sourceForm.max_rows" :min="1" :max="10000" />
          </t-form-item>
          <t-form-item label="单表最大扫描行数">
            <t-input-number v-model="sourceForm.max_scan_rows" :min="100" :max="500000" />
          </t-form-item>
          <t-form-item label="查询超时（秒）">
            <t-input-number v-model="sourceForm.timeout_seconds" :min="3" :max="120" />
          </t-form-item>
        </div>
      </t-form>
      <template #footer>
        <div class="source-dialog-footer">
          <div class="source-dialog-footer__left">
            <t-button
              v-if="!editingSource"
              theme="primary"
              :loading="testingSourceConfig"
              :disabled="savingSource"
              @click="testSourceConfig"
            >
              测试连接
            </t-button>
          </div>
          <div class="source-dialog-footer__right">
            <t-button
              class="source-dialog-cancel"
              theme="default"
              :disabled="savingSource || testingSourceConfig"
              @click="dialogVisible = false"
            >
              取消
            </t-button>
            <t-button theme="primary" :loading="savingSource" :disabled="testingSourceConfig" @click="saveSource">确认</t-button>
          </div>
        </div>
      </template>
    </t-dialog>

    <t-dialog v-model:visible="shareDialogVisible" header="共享数据源" width="520px" :confirm-loading="sharing" @confirm="saveShare">
      <t-form label-align="top">
        <t-form-item label="共享空间">
          <t-select v-model="shareForm.organization_id" placeholder="选择共享空间">
            <t-option v-for="org in writableOrganizations" :key="org.id" :value="org.id" :label="org.name" />
          </t-select>
        </t-form-item>
        <t-form-item label="权限">
          <t-select v-model="shareForm.permission">
            <t-option value="viewer" label="只读" />
            <t-option value="editor" label="可编辑" />
          </t-select>
        </t-form-item>
      </t-form>
    </t-dialog>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue';
import { MessagePlugin } from 'tdesign-vue-next';
import ListSpaceSidebar from '@/components/ListSpaceSidebar.vue';
import { useAuthStore } from '@/stores/auth';
import { useOrganizationStore } from '@/stores/organization';
import { useResourcePins } from '@/composables/useResourcePins';
import InformationSourceTabs from '@/custom/modules/information-source/InformationSourceTabs.vue';
import {
  createDatabaseSource,
  deleteDatabaseSource,
  getDatabaseSource,
  listDatabaseSourceShares,
  listDatabaseSources,
  listOrganizationSharedDatabaseSources,
  refreshDatabaseMetadata,
  removeDatabaseSourceShare,
  setDatabaseTableScope,
  shareDatabaseSource,
  testDatabaseSource,
  testDatabaseSourceConfig,
  updateDatabaseColumn,
  updateDatabaseSource,
  updateDatabaseSourceSharePermission,
  type CreateDatabaseSourceRequest,
  type DatabaseColumn,
  type DatabaseSource,
  type DatabaseSourceSharePermission,
  type DatabaseSourceShare,
  type DatabaseSourceType,
  type DatabaseTable,
  type OrganizationSharedDatabaseSourceItem,
} from '@/api/dbanalytics';

const orgStore = useOrganizationStore();
const authStore = useAuthStore();
const pins = useResourcePins();
const allSources = ref<DatabaseSource[]>([]);
const spaceSources = ref<DatabaseSource[]>([]);
const activeSourceId = ref('');
const activeSource = ref<DatabaseSource | null>(null);
const tables = ref<DatabaseTable[]>([]);
const selectedTableId = ref('');
const loadingSources = ref(false);
const spaceSourcesLoading = ref(false);
const testing = ref(false);
const refreshing = ref(false);
const savingScope = ref(false);
const savingSource = ref(false);
const testingSourceConfig = ref(false);
const savingColumnId = ref('');
const dialogVisible = ref(false);
const editingSource = ref<DatabaseSource | null>(null);
const sourceScope = ref('all');
const sourceCountByOrg = ref<Record<string, number>>({});
const sourceShares = ref<DatabaseSourceShare[]>([]);
const loadingShares = ref(false);
const shareDialogVisible = ref(false);
const sharing = ref(false);
const shareForm = reactive<{ organization_id: string; permission: DatabaseSourceSharePermission }>({
  organization_id: '',
  permission: 'viewer',
});
const enabledTableIds = ref(new Set<string>());
const columnDrafts = reactive<Record<string, { description: string; semantic_type: string; sensitive_level: string }>>({});

const sourceForm = reactive<CreateDatabaseSourceRequest>({
  name: '',
  description: '',
  type: 'postgres',
  config: {
    host: '',
    port: 5432,
    database: '',
    username: '',
    password: '',
    ssl_mode: 'disable',
  },
  query_mode: 'live',
  max_rows: 1000,
  max_scan_rows: 50000,
  timeout_seconds: 30,
});

const selectedTable = computed(() => tables.value.find(table => table.id === selectedTableId.value) || null);
const canManageActiveSource = computed(() => !!activeSource.value && activeSource.value.shared !== true && !activeSource.value.source_from_agent);
const canEditActiveSource = computed(() => {
  if (!activeSource.value || activeSource.value.source_from_agent) return false;
  if (canManageActiveSource.value) return true;
  return activeSource.value.permission === 'editor';
});
const writableOrganizations = computed(() =>
  orgStore.organizations.filter(org => ['admin', 'editor'].includes(org.my_role || '')),
);
const RESERVED_SOURCE_SCOPES = new Set(['all', 'mine', 'favorites', 'recents']);
const sourceScopeOrgId = computed(() => {
  const value = sourceScope.value;
  return value && !RESERVED_SOURCE_SCOPES.has(value) ? value : '';
});
const sourceFavoritesCount = computed(
  () => pins.favorites.value.filter(entry => entry.type === 'data_source').length,
);
const sourceRecentsCount = computed(
  () => pins.recents.value.filter(entry => entry.type === 'data_source').length,
);
const allSourcesCount = computed(() => allSources.value.length);
const mineSourcesCount = computed(() => allSources.value.filter(isSourceMine).length);
const baseSourceCountByOrg = computed<Record<string, number>>(() => {
  const countByOrg: Record<string, number> = {};
  orgStore.organizations.forEach(org => {
    countByOrg[org.id] = 0;
  });
  allSources.value.forEach(source => {
    if (!source.organization_id) return;
    countByOrg[source.organization_id] = (countByOrg[source.organization_id] || 0) + 1;
  });
  return countByOrg;
});
const effectiveSourceCountByOrg = computed<Record<string, number>>(() => ({
  ...baseSourceCountByOrg.value,
  ...sourceCountByOrg.value,
}));
const sourceResourceIndex = computed(() => {
  const map = new Map<string, DatabaseSource>();
  allSources.value.forEach(source => map.set(source.id, source));
  spaceSources.value.forEach(source => {
    if (!map.has(source.id)) map.set(source.id, source);
  });
  if (activeSource.value && !map.has(activeSource.value.id)) {
    map.set(activeSource.value.id, activeSource.value);
  }
  return map;
});
const favoriteSources = computed<DatabaseSource[]>(() =>
  pins.favorites.value
    .filter(entry => entry.type === 'data_source')
    .map(entry => sourceResourceIndex.value.get(entry.id))
    .filter((source): source is DatabaseSource => !!source),
);
const recentSources = computed<DatabaseSource[]>(() =>
  pins.recents.value
    .filter(entry => entry.type === 'data_source')
    .map(entry => sourceResourceIndex.value.get(entry.id))
    .filter((source): source is DatabaseSource => !!source),
);
const sources = computed<DatabaseSource[]>(() => {
  if (sourceScope.value === 'favorites') return favoriteSources.value;
  if (sourceScope.value === 'recents') return recentSources.value;
  if (sourceScope.value === 'mine') return allSources.value.filter(isSourceMine);
  if (sourceScopeOrgId.value) return spaceSources.value;
  return allSources.value;
});
const loadingCurrentSources = computed(() => loadingSources.value || spaceSourcesLoading.value);
const sourceEmptyText = computed(() => {
  if (sourceScope.value === 'favorites') return '暂无收藏数据源';
  if (sourceScope.value === 'recents') return '暂无最近使用数据源';
  if (sourceScope.value === 'mine') return '本空间暂无数据源';
  if (sourceScopeOrgId.value) return '该空间暂无数据源';
  return '暂无数据源';
});

const sharePermissionLabels: Record<DatabaseSourceSharePermission, string> = {
  viewer: '只读',
  editor: '可编辑',
};

function normalizeSharePermission(value: unknown): DatabaseSourceSharePermission {
  return value === 'editor' || value === 'admin' ? 'editor' : 'viewer';
}

function normalizeOptionalSharePermission(value: unknown): DatabaseSourceSharePermission | undefined {
  if (value === undefined || value === null || value === '') return undefined;
  return normalizeSharePermission(value);
}

function formatSharePermission(value: unknown) {
  return sharePermissionLabels[normalizeSharePermission(value)];
}

function normalizeSourceListItem(source: DatabaseSource): DatabaseSource {
  return {
    ...source,
    permission: normalizeOptionalSharePermission(source.permission),
  };
}

function normalizeOrganizationSourceItem(item: OrganizationSharedDatabaseSourceItem): DatabaseSource {
  return {
    ...item.source,
    is_mine: item.is_mine,
    source_from_agent: item.source_from_agent || item.source.source_from_agent,
    organization_id: item.organization_id,
    org_name: item.org_name,
    permission: normalizeOptionalSharePermission(item.permission),
    source_tenant_id: item.source_tenant_id,
    shared: !item.is_mine,
  };
}

function isSourceMine(source: DatabaseSource): boolean {
  return source.is_mine === true || (!source.shared && !source.source_from_agent);
}

function clearActiveSource() {
  activeSourceId.value = '';
  activeSource.value = null;
  tables.value = [];
  selectedTableId.value = '';
  enabledTableIds.value = new Set<string>();
  sourceShares.value = [];
}

function resetForm(type: DatabaseSourceType = 'postgres') {
  sourceForm.name = '';
  sourceForm.description = '';
  sourceForm.type = type;
  sourceForm.config = {
    host: '',
    port: type === 'mysql' ? 3306 : 5432,
    database: '',
    username: '',
    password: '',
    ssl_mode: type === 'mysql' ? '' : 'disable',
  };
  sourceForm.query_mode = 'live';
  sourceForm.max_rows = 1000;
  sourceForm.max_scan_rows = 50000;
  sourceForm.timeout_seconds = 30;
}

function handleTypeChange(value: DatabaseSourceType) {
  sourceForm.config.port = value === 'mysql' ? 3306 : 5432;
  sourceForm.config.ssl_mode = value === 'mysql' ? '' : 'disable';
}

function buildSourcePayload() {
  const payload = JSON.parse(JSON.stringify(sourceForm)) as CreateDatabaseSourceRequest;
  if (editingSource.value && !payload.config.password) {
    delete payload.config.password;
  }
  return payload;
}

function validateSourceForm(requireName: boolean, requirePassword: boolean) {
  if (requireName && !sourceForm.name.trim()) {
    MessagePlugin.warning('请填写数据源名称');
    return false;
  }
  if (!sourceForm.config.host.trim()) {
    MessagePlugin.warning('请填写 Host');
    return false;
  }
  if (!sourceForm.config.port) {
    MessagePlugin.warning('请填写 Port');
    return false;
  }
  if (!sourceForm.config.database.trim()) {
    MessagePlugin.warning('请填写 Database');
    return false;
  }
  if (!sourceForm.config.username.trim()) {
    MessagePlugin.warning('请填写 Username');
    return false;
  }
  if (requirePassword && !sourceForm.config.password?.trim()) {
    MessagePlugin.warning('请填写 Password');
    return false;
  }
  return true;
}

function handleTableChecked(tableId: string, checked: unknown) {
  if (!canEditActiveSource.value) return;
  toggleTable(tableId, checked === true);
}

function openCreateDialog() {
  editingSource.value = null;
  resetForm();
  dialogVisible.value = true;
}

function openEditDialog() {
  if (!activeSource.value || !canEditActiveSource.value) return;
  editingSource.value = activeSource.value;
  resetForm(activeSource.value.type);
  sourceForm.name = activeSource.value.name;
  sourceForm.description = activeSource.value.description || '';
  sourceForm.type = activeSource.value.type;
  sourceForm.config = {
    host: activeSource.value.config?.host || '',
    port: activeSource.value.config?.port || (activeSource.value.type === 'mysql' ? 3306 : 5432),
    database: activeSource.value.config?.database || '',
    username: activeSource.value.config?.username || '',
    password: '',
    ssl_mode: activeSource.value.config?.ssl_mode || (activeSource.value.type === 'postgres' ? 'disable' : ''),
  };
  sourceForm.max_rows = activeSource.value.max_rows;
  sourceForm.max_scan_rows = activeSource.value.max_scan_rows;
  sourceForm.timeout_seconds = activeSource.value.timeout_seconds;
  dialogVisible.value = true;
}

async function loadSources() {
  loadingSources.value = true;
  try {
    const res = await listDatabaseSources();
    allSources.value = (res.data || []).map(normalizeSourceListItem);
  } finally {
    loadingSources.value = false;
  }
}

async function loadSpaceSources(orgId: string) {
  if (!orgId) {
    spaceSources.value = [];
    return;
  }
  spaceSources.value = [];
  spaceSourcesLoading.value = true;
  try {
    const res = await listOrganizationSharedDatabaseSources(orgId);
    const nextSources = (res.data || []).map(normalizeOrganizationSourceItem);
    spaceSources.value = nextSources;
    sourceCountByOrg.value = {
      ...sourceCountByOrg.value,
      [orgId]: nextSources.length,
    };
  } finally {
    spaceSourcesLoading.value = false;
  }
}

async function refreshSourceCounts() {
  const organizations = orgStore.organizations || [];
  if (organizations.length === 0) {
    sourceCountByOrg.value = {};
    return;
  }
  const entries = await Promise.all(
    organizations.map(async org => {
      try {
        const res = await listOrganizationSharedDatabaseSources(org.id);
        return [org.id, (res.data || []).length] as const;
      } catch {
        return [org.id, 0] as const;
      }
    }),
  );
  sourceCountByOrg.value = Object.fromEntries(entries);
}

let activeReconcileVersion = 0;
async function reconcileActiveSource() {
  const version = ++activeReconcileVersion;
  if (activeSourceId.value && sources.value.some(source => source.id === activeSourceId.value)) {
    return;
  }
  clearActiveSource();
  const firstSource = sources.value[0];
  if (firstSource && version === activeReconcileVersion) {
    await selectSource(firstSource.id, false);
  }
}

async function selectSource(id: string, markRecent = true) {
  const listItem = sourceResourceIndex.value.get(id) || sources.value.find(source => source.id === id);
  if (markRecent) pins.touchRecent('data_source', id);
  activeSourceId.value = id;
  const res = await getDatabaseSource(id);
  activeSource.value = {
    ...(res.data || {}),
    ...(listItem || {}),
  };
  tables.value = res.tables || [];
  enabledTableIds.value = new Set(tables.value.filter(table => table.enabled).map(table => table.id));
  selectedTableId.value = tables.value[0]?.id || '';
  rebuildColumnDrafts();
  await loadSourceShares();
}

function rebuildColumnDrafts() {
  for (const table of tables.value) {
    for (const column of table.columns || []) {
      columnDrafts[column.id] = {
        description: column.description || '',
        semantic_type: column.semantic_type || 'dimension',
        sensitive_level: column.sensitive_level || 'none',
      };
    }
  }
}

function toggleTable(tableId: string, checked: boolean) {
  const next = new Set(enabledTableIds.value);
  if (checked) {
    next.add(tableId);
  } else {
    next.delete(tableId);
  }
  enabledTableIds.value = next;
}

async function saveSource() {
  if (!validateSourceForm(true, !editingSource.value)) {
    return;
  }
  savingSource.value = true;
  try {
    const payload = buildSourcePayload();
    if (editingSource.value) {
      const updatedSourceId = editingSource.value.id;
      await updateDatabaseSource(editingSource.value.id, payload);
      MessagePlugin.success('数据源已更新');
      await loadSources();
      await selectSource(updatedSourceId, false);
    } else {
      const res = await createDatabaseSource(payload);
      MessagePlugin.success('数据源已创建');
      await loadSources();
      if (res.data?.id) await selectSource(res.data.id, false);
    }
    dialogVisible.value = false;
  } catch (err: any) {
    MessagePlugin.error(err?.message || '保存失败');
  } finally {
    savingSource.value = false;
  }
}

async function testSourceConfig() {
  if (editingSource.value) return;
  if (!validateSourceForm(false, true)) {
    return;
  }
  testingSourceConfig.value = true;
  try {
    await testDatabaseSourceConfig(buildSourcePayload());
    MessagePlugin.success('连接正常，账号权限为只读');
  } catch (err: any) {
    MessagePlugin.error(err?.message || '连接测试失败');
  } finally {
    testingSourceConfig.value = false;
  }
}

async function handleTest() {
  if (!activeSource.value || !canEditActiveSource.value) return;
  testing.value = true;
  try {
    await testDatabaseSource(activeSource.value.id);
    MessagePlugin.success('连接正常');
    await selectSource(activeSource.value.id);
  } catch (err: any) {
    MessagePlugin.error(err?.message || '连接失败');
  } finally {
    testing.value = false;
  }
}

async function handleRefresh() {
  if (!activeSource.value || !canEditActiveSource.value) return;
  refreshing.value = true;
  try {
    await refreshDatabaseMetadata(activeSource.value.id);
    MessagePlugin.success('元数据已刷新');
    await selectSource(activeSource.value.id);
  } catch (err: any) {
    MessagePlugin.error(err?.message || '刷新失败');
  } finally {
    refreshing.value = false;
  }
}

async function saveTableScope() {
  if (!activeSource.value || !canEditActiveSource.value) return;
  savingScope.value = true;
  try {
    await setDatabaseTableScope(activeSource.value.id, Array.from(enabledTableIds.value));
    MessagePlugin.success('表范围已保存');
    await selectSource(activeSource.value.id);
  } catch (err: any) {
    MessagePlugin.error(err?.message || '保存失败');
  } finally {
    savingScope.value = false;
  }
}

async function saveColumn(columnId: string) {
  if (!canEditActiveSource.value) return;
  savingColumnId.value = columnId;
  try {
    await updateDatabaseColumn(columnId, columnDrafts[columnId]);
    MessagePlugin.success('字段说明已保存');
    if (activeSource.value) await selectSource(activeSource.value.id);
  } catch (err: any) {
    MessagePlugin.error(err?.message || '保存失败');
  } finally {
    savingColumnId.value = '';
  }
}

async function handleDelete() {
  if (!activeSource.value || !canManageActiveSource.value) return;
  const id = activeSource.value.id;
  try {
    await deleteDatabaseSource(id);
    MessagePlugin.success('数据源已删除');
    if (pins.isFavorite('data_source', id)) {
      await pins.toggleFavorite('data_source', id);
    }
    pins.removeRecent('data_source', id);
    clearActiveSource();
    await loadSources();
    await refreshSourceCounts();
    if (sourceScopeOrgId.value) {
      await loadSpaceSources(sourceScopeOrgId.value);
    }
    await reconcileActiveSource();
  } catch (err: any) {
    MessagePlugin.error(err?.message || '删除失败');
  }
}

async function loadSourceShares() {
  sourceShares.value = [];
  if (!activeSource.value || !canManageActiveSource.value) return;
  loadingShares.value = true;
  try {
    const res = await listDatabaseSourceShares(activeSource.value.id);
    sourceShares.value = (res.data?.shares || []).map(share => ({
      ...share,
      permission: normalizeSharePermission(share.permission),
      my_permission: normalizeOptionalSharePermission(share.my_permission),
    }));
  } catch {
    sourceShares.value = [];
  } finally {
    loadingShares.value = false;
  }
}

function openShareDialog() {
  if (!activeSource.value || !canManageActiveSource.value) return;
  shareForm.organization_id = writableOrganizations.value[0]?.id || '';
  shareForm.permission = 'viewer';
  shareDialogVisible.value = true;
}

async function saveShare() {
  if (!activeSource.value || !shareForm.organization_id) {
    MessagePlugin.warning('请选择共享空间');
    return;
  }
  sharing.value = true;
  try {
    await shareDatabaseSource(activeSource.value.id, {
      organization_id: shareForm.organization_id,
      permission: shareForm.permission,
    });
    MessagePlugin.success('数据源已共享');
    shareDialogVisible.value = false;
    await loadSourceShares();
    await orgStore.fetchOrganizations({ force: true });
    await refreshSourceCounts();
    if (sourceScopeOrgId.value === shareForm.organization_id) {
      await loadSpaceSources(shareForm.organization_id);
    }
  } catch (err: any) {
    MessagePlugin.error(err?.message || '共享失败');
  } finally {
    sharing.value = false;
  }
}

async function changeSharePermission(share: DatabaseSourceShare, permission: DatabaseSourceSharePermission) {
  if (!activeSource.value) return;
  try {
    await updateDatabaseSourceSharePermission(activeSource.value.id, share.id, { permission });
    MessagePlugin.success('共享权限已更新');
    await loadSourceShares();
  } catch (err: any) {
    MessagePlugin.error(err?.message || '更新失败');
    await loadSourceShares();
  }
}

function onSharePermissionChange(share: DatabaseSourceShare, value: unknown) {
  if (value === 'editor' || value === 'viewer') {
    changeSharePermission(share, value);
  }
}

async function removeShare(share: DatabaseSourceShare) {
  if (!activeSource.value) return;
  try {
    const removedOrgId = share.organization_id;
    await removeDatabaseSourceShare(activeSource.value.id, share.id);
    MessagePlugin.success('已取消共享');
    await loadSourceShares();
    await orgStore.fetchOrganizations({ force: true });
    await refreshSourceCounts();
    if (sourceScopeOrgId.value === removedOrgId) {
      await loadSpaceSources(removedOrgId);
      await reconcileActiveSource();
    }
  } catch (err: any) {
    MessagePlugin.error(err?.message || '移除失败');
  }
}

async function toggleFavoriteSource(sourceId: string, evt?: Event) {
  evt?.stopPropagation();
  const favorited = await pins.toggleFavorite('data_source', sourceId);
  if (!favorited && sourceScope.value === 'favorites') {
    await reconcileActiveSource();
  }
}

function isSourceFavorited(sourceId: string) {
  return pins.isFavorite('data_source', sourceId);
}

function formatSamples(values?: string[]) {
  if (!values || values.length === 0) return '';
  return values.slice(0, 3).join(' / ');
}

watch(sourceScope, async () => {
  clearActiveSource();
  if (sourceScopeOrgId.value) {
    await loadSpaceSources(sourceScopeOrgId.value);
  }
  await reconcileActiveSource();
});

watch(
  () => sources.value.map(source => source.id).join('|'),
  () => {
    void reconcileActiveSource();
  },
);

watch(
  () => orgStore.organizations.map(org => org.id).join('|'),
  () => {
    void refreshSourceCounts();
  },
);

onMounted(async () => {
  await orgStore.fetchOrganizations();
  await Promise.all([loadSources(), refreshSourceCounts()]);
  if (sourceScopeOrgId.value) {
    await loadSpaceSources(sourceScopeOrgId.value);
  }
  await reconcileActiveSource();
});
</script>

<style scoped lang="less">
.db-source-shell {
  flex: 1;
  display: flex;
  height: 100%;
  margin: 0;
  box-sizing: border-box;
  position: relative;
  min-height: 0;
  background: var(--td-bg-color-page);
}

.db-source-page {
  flex: 1;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
  padding: 20px 0 0 28px;
  color: var(--td-text-color-primary);
  background: var(--td-bg-color-page);
  box-sizing: border-box;
  overflow: hidden;
}

.source-tabs {
  margin-bottom: 6px;
}

.page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 18px;
  padding-right: 28px;

  h1 {
    margin: 0;
    font-size: 24px;
    line-height: 32px;
  }

  p {
    margin: 6px 0 0;
    color: var(--td-text-color-secondary);
  }
}

.page-body {
  flex: 1;
  display: grid;
  grid-template-columns: 300px minmax(0, 1fr);
  gap: 16px;
  min-height: 0;
  padding: 0 24px 24px 0;
  box-sizing: border-box;
}

.source-list,
.source-detail {
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
}

.source-list {
  min-height: 0;
  padding: 8px;
  overflow: auto;
}

.source-row {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  border-radius: 6px;
  background: transparent;
  color: inherit;
  position: relative;

  &:hover,
  &.active {
    background: var(--td-bg-color-container-hover);
  }

  &:hover .source-favorite-star {
    opacity: 1;
  }
}

.source-row-button {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  border: 0;
  border-radius: 6px;
  padding: 10px 4px 10px 10px;
  background: transparent;
  color: inherit;
  text-align: left;
  cursor: pointer;
}

.source-favorite-star {
  width: 28px;
  height: 28px;
  flex: 0 0 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  margin-right: 4px;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--td-text-color-secondary);
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.15s ease, background 0.15s ease, color 0.15s ease;

  &:hover {
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-warning-color, #e37318);
  }

  &.is-favorited {
    opacity: 1;
    color: var(--td-warning-color, #e37318);
  }
}

.source-tags {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}

.source-main,
.table-main {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.source-name,
.table-name,
.column-name {
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.source-meta,
.table-meta,
.sample-values {
  margin-top: 3px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.source-detail {
  min-height: 0;
  padding: 18px;
  overflow: auto;
}

.detail-head,
.section-title {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;

  h2,
  h3 {
    margin: 0;
  }

  p {
    margin: 6px 0 0;
    color: var(--td-text-color-secondary);
  }
}

.head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  justify-content: flex-end;
}

.scope-section,
.columns-section,
.share-section {
  margin-top: 24px;
}

.shared-text {
  color: var(--td-brand-color) !important;
}

.share-list {
  display: grid;
  gap: 8px;
  margin-top: 12px;
}

.share-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  padding: 10px 12px;

  small {
    display: block;
    margin-top: 3px;
    color: var(--td-text-color-secondary);
  }
}

.share-actions {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.permission-select {
  width: 112px;
}

.share-empty {
  min-height: 64px;
}

.table-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 8px;
  margin-top: 12px;
}

.table-row {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  padding: 10px;
  color: inherit;
  text-align: left;
  cursor: pointer;

  &:hover,
  &.selected {
    border-color: var(--td-brand-color);
    background: var(--td-brand-color-light);
  }
}

.column-table-wrap {
  margin-top: 12px;
  overflow-x: auto;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
}

.column-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;

  th,
  td {
    padding: 10px;
    border-bottom: 1px solid var(--td-component-stroke);
    vertical-align: top;
  }

  th {
    text-align: left;
    background: var(--td-bg-color-secondarycontainer);
    font-weight: 600;
  }
}

.small-select {
  width: 104px;
}

.column-action {
  width: 72px;
  text-align: right;
}

.form-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}

.source-dialog-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  width: 100%;
}

.source-dialog-footer__left,
.source-dialog-footer__right {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.source-dialog-footer__left {
  min-width: 0;
}

.source-dialog-footer__right {
  flex-shrink: 0;
}

.source-dialog-cancel {
  background-color: var(--td-bg-color-component);
  border-color: var(--td-component-border);
  color: var(--td-text-color-primary);
}

.source-dialog-cancel:hover,
.source-dialog-cancel:focus-visible {
  background-color: var(--td-bg-color-component-hover);
  border-color: var(--td-component-border);
  color: var(--td-text-color-primary);
}

.empty,
.empty-detail,
.loading-wrap {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 120px;
  color: var(--td-text-color-placeholder);
}

.empty-detail {
  min-height: 520px;
  flex-direction: column;
  gap: 10px;

  .t-icon {
    font-size: 28px;
  }
}

.error-text {
  color: var(--td-error-color) !important;
}

@media (max-width: 900px) {
  .page-body {
    grid-template-columns: 1fr;
  }

  .form-grid {
    grid-template-columns: 1fr;
  }
}
</style>
