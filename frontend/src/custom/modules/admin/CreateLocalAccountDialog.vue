<template>
  <t-dialog
    :visible="visible"
    width="520px"
    :footer="false"
    :close-btn="!submitting && !result"
    :close-on-overlay-click="false"
    :close-on-esc-keydown="!submitting && !result"
    @update:visible="onVisibleUpdate"
    @close="handleClose"
  >
    <template #header>
      <span class="create-account__header">
        <t-icon name="user-add" size="20px" />
        <span>新建本地账号</span>
      </span>
    </template>

    <template v-if="!result">
      <p class="create-account__description">
        系统将自动生成符合当前复杂度要求的随机密码。创建成功后，临时密码只在本弹窗中显示一次。
      </p>

      <t-form
        ref="formRef"
        :data="form"
        :rules="formRules"
        layout="vertical"
        @submit.prevent
      >
        <t-form-item label="用户名" name="username">
          <t-input
            v-model="form.username"
            autofocus
            :maxlength="20"
            placeholder="2-20 个字母、数字、中文或下划线"
            :disabled="submitting"
            @enter="handleSubmit"
          />
        </t-form-item>
        <t-form-item label="姓名（可选）" name="displayName">
          <t-input
            v-model="form.displayName"
            :maxlength="255"
            placeholder="用于系统内展示"
            :disabled="submitting"
            @enter="handleSubmit"
          />
        </t-form-item>
      </t-form>

      <div class="create-account__actions">
        <t-button variant="outline" :disabled="submitting" @click="handleClose">取消</t-button>
        <t-button theme="primary" :loading="submitting" @click="handleSubmit">创建账号</t-button>
      </div>
    </template>

    <template v-else>
      <t-alert
        theme="warning"
        title="请立即保存临时密码"
        message="关闭弹窗后将无法再次查看该密码，请通过安全渠道发送给用户。"
      />

      <dl class="create-account__result">
        <div>
          <dt>账号</dt>
          <dd>{{ result.user.username }}</dd>
        </div>
        <div v-if="result.user.display_name">
          <dt>姓名</dt>
          <dd>{{ result.user.display_name }}</dd>
        </div>
        <div>
          <dt>临时密码</dt>
          <dd class="create-account__password-row">
            <code>{{ result.temporary_password }}</code>
            <t-button size="small" variant="outline" @click="copyPassword">
              <template #icon><t-icon name="file-copy" /></template>
              复制密码
            </t-button>
          </dd>
        </div>
      </dl>

      <t-alert
        v-for="warning in result.warnings || []"
        :key="warning"
        class="create-account__warning"
        theme="warning"
        :message="warning"
      />

      <div class="create-account__actions">
        <t-button theme="primary" @click="acknowledgeAndClose">我已保存，关闭</t-button>
      </div>
    </template>
  </t-dialog>
</template>

<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import {
  MessagePlugin,
  type FormInstanceFunctions,
  type FormRule,
} from 'tdesign-vue-next'

import {
  createAdminLocalAccount,
  type AdminCreatedLocalAccount,
} from '@/api/custom-admin'

const props = defineProps<{
  visible: boolean
}>()

const emit = defineEmits<{
  (event: 'update:visible', value: boolean): void
  (event: 'created', result: AdminCreatedLocalAccount): void
}>()

const formRef = ref<FormInstanceFunctions | null>(null)
const submitting = ref(false)
const result = ref<AdminCreatedLocalAccount | null>(null)
const allowResultClose = ref(false)
const form = reactive({
  username: '',
  displayName: '',
})

const formRules: Record<string, FormRule[]> = {
  username: [
    {
      validator: (value: string) => {
        const username = String(value || '').trim()
        return username.length >= 2 &&
          username.length <= 20 &&
          /^[a-zA-Z0-9_\u4e00-\u9fa5]+$/u.test(username)
      },
      message: '用户名须为 2-20 个字母、数字、中文或下划线',
      trigger: 'blur',
    },
  ],
}

watch(
  () => props.visible,
  (open) => {
    if (!open) return
    form.username = ''
    form.displayName = ''
    result.value = null
    allowResultClose.value = false
    requestAnimationFrame(() => formRef.value?.clearValidate?.())
  },
)

function onVisibleUpdate(next: boolean) {
  if (!next && (submitting.value || (result.value && !allowResultClose.value))) return
  emit('update:visible', next)
}

function handleClose() {
  if (submitting.value || result.value) return
  emit('update:visible', false)
}

async function handleSubmit() {
  if (submitting.value) return
  const validation = await formRef.value?.validate?.()
  if (validation !== true) return

  submitting.value = true
  try {
    const response = await createAdminLocalAccount({
      username: form.username.trim(),
      display_name: form.displayName.trim() || undefined,
    })
    if (!response.success || !response.data) {
      throw new Error(response.message || '创建账号失败')
    }
    result.value = response.data
    emit('created', response.data)
    MessagePlugin.success('本地账号已创建')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '创建账号失败')
  } finally {
    submitting.value = false
  }
}

async function copyPassword() {
  const password = result.value?.temporary_password
  if (!password) return
  try {
    await navigator.clipboard.writeText(password)
    MessagePlugin.success('临时密码已复制')
  } catch {
    MessagePlugin.error('复制失败，请手动选择密码')
  }
}

function acknowledgeAndClose() {
  allowResultClose.value = true
  result.value = null
  emit('update:visible', false)
}
</script>

<style scoped lang="less">
.create-account__header {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.create-account__description {
  margin: 0 0 18px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 1.6;
}

.create-account__actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 22px;
}

.create-account__result {
  margin: 18px 0 0;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  overflow: hidden;

  > div {
    display: grid;
    grid-template-columns: 92px minmax(0, 1fr);
    align-items: center;
    min-height: 48px;
    border-bottom: 1px solid var(--td-component-stroke);

    &:last-child {
      border-bottom: 0;
    }
  }

  dt,
  dd {
    margin: 0;
    padding: 10px 14px;
  }

  dt {
    height: 100%;
    display: flex;
    align-items: center;
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-text-color-secondary);
  }

  dd {
    min-width: 0;
    overflow-wrap: anywhere;
    color: var(--td-text-color-primary);
  }
}

.create-account__password-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;

  code {
    font-size: 15px;
    font-weight: 600;
    letter-spacing: 0.04em;
  }
}

.create-account__warning {
  margin-top: 10px;
}

@media (max-width: 560px) {
  .create-account__password-row {
    align-items: flex-start;
    flex-direction: column;
  }
}
</style>
