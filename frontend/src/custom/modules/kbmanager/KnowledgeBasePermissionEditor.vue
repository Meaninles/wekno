<template>
  <div class="kb-manager-permissions" data-testid="kb-manager-permissions">
    <div class="permission-note">
      <t-icon name="info-circle" size="16px" />
      <div>
        <strong>文档级管理权限</strong>
        <p>“修改”采用先增后删：确认新文档已写入知识库后，由智能体立即调用删除工具删除旧文档，不等待解析完成，因此同时需要新增和删除权限。</p>
      </div>
    </div>

    <section class="permission-section">
      <div class="permission-heading">
        <div>
          <strong>其它知识库统一权限</strong>
          <p>默认应用到所有未单独设置的已选知识库。</p>
        </div>
        <PermissionChecks
          :value="config.default_permissions"
          :disabled="disabled"
          test-prefix="kb-manager-default"
          @update="updateDefault"
        />
      </div>
    </section>

    <section class="permission-section">
      <div class="section-title">
        <div>
          <strong>单个知识库覆盖</strong>
          <p>只为例外知识库开启“单独设置”，其余继续使用上面的统一权限。</p>
        </div>
      </div>

      <div v-if="selectedKnowledgeBases.length === 0" class="empty-state">
        请先选择至少一个知识库。
      </div>
      <div v-else class="kb-permission-list">
        <div
          v-for="kb in selectedKnowledgeBases"
          :key="kb.value"
          class="kb-permission-row"
          :data-testid="`kb-manager-permission-${kb.value}`"
        >
          <div class="kb-meta">
            <div class="kb-name-row">
              <t-icon name="folder" />
              <span class="kb-name">{{ kb.label }}</span>
              <t-tag v-if="kb.shared" size="small" variant="light">共享</t-tag>
            </div>
            <span v-if="kb.orgName" class="kb-org">{{ kb.orgName }}</span>
          </div>

          <div class="override-control">
            <div class="override-switch">
              <span>{{ hasOverride(kb.value) ? '单独设置' : '使用统一权限' }}</span>
              <t-switch
                :value="hasOverride(kb.value)"
                :disabled="disabled"
                :data-testid="`kb-manager-override-${kb.value}`"
                @change="(value: boolean) => toggleOverride(kb.value, value)"
              />
            </div>
            <PermissionChecks
              :value="permissionsFor(kb.value)"
              :disabled="disabled || !hasOverride(kb.value)"
              :test-prefix="`kb-manager-${kb.value}`"
              @update="(value) => updateOverride(kb.value, value)"
            />
          </div>
        </div>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, watch, type PropType } from 'vue';
import type {
  KnowledgeManagementConfig,
  KnowledgeManagementPermissionSet,
} from '@/api/agent';
import {
  normalizeKnowledgeManagementConfig,
  normalizeKnowledgeManagementPermission,
} from './config';

export interface KnowledgeBasePermissionOption {
  label: string;
  value: string;
  shared?: boolean;
  orgName?: string;
}

const props = withDefaults(defineProps<{
  modelValue?: KnowledgeManagementConfig;
  knowledgeBaseIds: string[];
  knowledgeBases: KnowledgeBasePermissionOption[];
  disabled?: boolean;
}>(), {
  modelValue: undefined,
  disabled: false,
});

const emit = defineEmits<{
  (event: 'update:modelValue', value: KnowledgeManagementConfig): void;
}>();

const normalizedConfig = (): KnowledgeManagementConfig => normalizeKnowledgeManagementConfig(
  props.modelValue,
  props.knowledgeBaseIds,
);

const config = computed(normalizedConfig);
const selectedKnowledgeBases = computed(() => {
  const selected = new Set(props.knowledgeBaseIds);
  return props.knowledgeBases.filter((kb) => selected.has(kb.value));
});

const commit = (next: KnowledgeManagementConfig) => {
  emit('update:modelValue', {
    default_permissions: normalizeKnowledgeManagementPermission(next.default_permissions),
    knowledge_base_overrides: Object.fromEntries(
      Object.entries(next.knowledge_base_overrides || {}).map(([kbId, permission]) => [
        kbId,
        normalizeKnowledgeManagementPermission(permission),
      ]),
    ),
  });
};

const hasOverride = (kbId: string) => Object.prototype.hasOwnProperty.call(
  config.value.knowledge_base_overrides,
  kbId,
);

const permissionsFor = (kbId: string) => (
  config.value.knowledge_base_overrides[kbId] || config.value.default_permissions
);

const updateDefault = (permission: KnowledgeManagementPermissionSet) => {
  commit({
    ...config.value,
    default_permissions: permission,
  });
};

const toggleOverride = (kbId: string, enabled: boolean) => {
  const overrides = { ...config.value.knowledge_base_overrides };
  if (enabled) {
    overrides[kbId] = { ...config.value.default_permissions };
  } else {
    delete overrides[kbId];
  }
  commit({ ...config.value, knowledge_base_overrides: overrides });
};

const updateOverride = (kbId: string, permission: KnowledgeManagementPermissionSet) => {
  if (!hasOverride(kbId)) return;
  commit({
    ...config.value,
    knowledge_base_overrides: {
      ...config.value.knowledge_base_overrides,
      [kbId]: permission,
    },
  });
};

watch(
  () => props.knowledgeBaseIds.slice(),
  () => {
    const normalized = normalizedConfig();
    const currentKeys = Object.keys(props.modelValue?.knowledge_base_overrides || {}).sort().join(',');
    const nextKeys = Object.keys(normalized.knowledge_base_overrides).sort().join(',');
    if (currentKeys !== nextKeys) commit(normalized);
  },
);

const PermissionChecks = defineComponent({
  name: 'PermissionChecks',
  props: {
    value: {
      type: Object as PropType<KnowledgeManagementPermissionSet>,
      required: true,
    },
    disabled: Boolean,
    testPrefix: {
      type: String,
      required: true,
    },
  },
  emits: ['update'],
  setup(componentProps, { emit: componentEmit }) {
    const setPermission = (key: keyof KnowledgeManagementPermissionSet, checked: boolean) => {
      const next = { ...componentProps.value, [key]: checked };
      if (key === 'modify' && checked) {
        next.add = true;
        next.delete = true;
      }
      next.modify = next.add && next.delete;
      componentEmit('update', next);
    };
    return () => h('div', { class: 'permission-checks' }, [
      h('label', { class: 'permission-check' }, [
        h('input', {
          type: 'checkbox',
          checked: componentProps.value.add,
          disabled: componentProps.disabled,
          'data-testid': `${componentProps.testPrefix}-add`,
          onChange: (event: Event) => setPermission('add', (event.target as HTMLInputElement).checked),
        }),
        h('span', '新增'),
      ]),
      h('label', { class: 'permission-check' }, [
        h('input', {
          type: 'checkbox',
          checked: componentProps.value.modify,
          disabled: componentProps.disabled,
          'data-testid': `${componentProps.testPrefix}-modify`,
          onChange: (event: Event) => setPermission('modify', (event.target as HTMLInputElement).checked),
        }),
        h('span', '修改（新增 + 删除）'),
      ]),
      h('label', { class: 'permission-check' }, [
        h('input', {
          type: 'checkbox',
          checked: componentProps.value.delete,
          disabled: componentProps.disabled,
          'data-testid': `${componentProps.testPrefix}-delete`,
          onChange: (event: Event) => setPermission('delete', (event.target as HTMLInputElement).checked),
        }),
        h('span', '删除'),
      ]),
    ]);
  },
});
</script>

<style scoped lang="less">
.kb-manager-permissions {
  display: flex;
  flex-direction: column;
  gap: 12px;
  width: 100%;
}

.permission-note {
  display: flex;
  gap: 10px;
  padding: 12px 14px;
  color: var(--td-text-color-primary);
  background: var(--td-brand-color-light);
  border: 1px solid var(--td-brand-color-3);
  border-radius: 8px;

  p {
    margin: 4px 0 0;
    color: var(--td-text-color-secondary);
    font-size: 12px;
    line-height: 1.6;
  }
}

.permission-section {
  padding: 14px;
  background: var(--td-bg-color-container-hover);
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
}

.permission-heading,
.section-title,
.kb-permission-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}

.permission-heading p,
.section-title p {
  margin: 4px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

.kb-permission-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 12px;
}

.kb-permission-row {
  padding: 12px;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
}

.kb-meta {
  min-width: 180px;
}

.kb-name-row {
  display: flex;
  align-items: center;
  gap: 6px;
}

.kb-name {
  font-weight: 500;
}

.kb-org {
  display: block;
  margin-top: 3px;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
}

.override-control {
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 8px;
}

.override-switch {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

:deep(.permission-checks) {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: 12px;
}

:deep(.permission-check) {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--td-text-color-primary);
  cursor: pointer;
  font-size: 13px;

  input {
    accent-color: var(--td-brand-color);
  }

  &:has(input:disabled) {
    color: var(--td-text-color-disabled);
    cursor: not-allowed;
  }
}

.empty-state {
  margin-top: 12px;
  padding: 18px;
  color: var(--td-text-color-placeholder);
  text-align: center;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 6px;
}

@media (max-width: 900px) {
  .permission-heading,
  .kb-permission-row {
    align-items: flex-start;
    flex-direction: column;
  }

  .override-control {
    align-items: flex-start;
    width: 100%;
  }

  :deep(.permission-checks) {
    justify-content: flex-start;
  }
}
</style>
