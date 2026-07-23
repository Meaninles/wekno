<template>
  <section class="password-settings">
    <div class="password-settings__heading">
      <h3>账号安全</h3>
      <p>管理当前账号的登录密码。</p>
    </div>

    <div class="password-settings__card">
      <div v-if="loading" class="password-settings__state">
        <t-loading size="small" />
        <span>正在读取账号类型…</span>
      </div>

      <t-alert
        v-else-if="loadError"
        theme="error"
        :message="loadError"
      >
        <template #operation>
          <t-button size="small" variant="text" @click="loadCapability">重试</t-button>
        </template>
      </t-alert>

      <template v-else-if="capability">
        <div class="password-settings__info">
          <div>
            <div class="password-settings__label">
              登录密码
              <t-tag
                size="small"
                :theme="capability.can_change_password ? 'primary' : 'default'"
                variant="light"
              >
                {{ capability.account_source === 'iam' ? 'IAM 统一身份认证' : '本地账号' }}
              </t-tag>
            </div>
            <p>
              {{
                capability.can_change_password
                  ? '修改后当前账号的所有登录会话都会失效，需要重新登录。'
                  : (capability.reason || '该账号的密码由统一身份认证系统管理。')
              }}
            </p>
          </div>
          <t-button
            theme="primary"
            variant="outline"
            :disabled="!capability.can_change_password"
            @click="openDialog"
          >
            {{ capability.can_change_password ? '修改密码' : '不可更改' }}
          </t-button>
        </div>
      </template>
    </div>

    <t-dialog
      v-model:visible="dialogVisible"
      header="修改登录密码"
      width="520px"
      attach="body"
      :confirm-loading="submitting"
      :confirm-btn="{ content: '确认修改', disabled: challengeLoading }"
      :cancel-btn="{ content: '取消', disabled: submitting }"
      :close-btn="!submitting"
      :close-on-overlay-click="false"
      @confirm="submitPasswordChange"
      @close="resetDialog"
    >
      <t-alert
        class="password-dialog__notice"
        theme="warning"
        message="密码修改成功后，所有设备上的登录状态都会失效。"
      />

      <t-form :data="form" layout="vertical" @submit.prevent>
        <t-form-item label="当前密码" name="oldPassword">
          <t-input
            v-model="form.oldPassword"
            type="password"
            autocomplete="current-password"
            placeholder="请输入当前密码"
            :disabled="submitting"
          />
        </t-form-item>
        <t-form-item label="新密码" name="newPassword">
          <t-input
            v-model="form.newPassword"
            type="password"
            autocomplete="new-password"
            placeholder="请输入新密码"
            :disabled="submitting"
          />
        </t-form-item>
        <p class="password-dialog__hint">8-32 个字符，不能包含空格，且至少包含字母、数字、符号中的两种。</p>
        <t-form-item label="确认新密码" name="confirmPassword">
          <t-input
            v-model="form.confirmPassword"
            type="password"
            autocomplete="new-password"
            placeholder="请再次输入新密码"
            :disabled="submitting"
          />
        </t-form-item>
        <t-form-item label="验证码" name="captcha">
          <div class="password-dialog__captcha">
            <t-input
              v-model="form.captcha"
              autocomplete="off"
              placeholder="请输入图中字符"
              :disabled="submitting || challengeLoading"
              @enter="submitPasswordChange"
            />
            <button
              type="button"
              class="password-dialog__captcha-button"
              :disabled="submitting || challengeLoading"
              title="点击刷新验证码"
              @click="loadChallenge"
            >
              <img
                v-if="challenge?.captcha_image"
                :src="challenge.captcha_image"
                alt="验证码"
              />
              <span v-else>{{ challengeLoading ? '加载中…' : '点击刷新' }}</span>
            </button>
          </div>
        </t-form-item>
      </t-form>
    </t-dialog>
  </section>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'

import { useAuthStore } from '@/stores/auth'
import {
  changeLocalPassword,
  encryptAuthPassword,
  getAuthChallenge,
  getPasswordCapability,
  type AuthChallenge,
  type PasswordCapability,
} from './api'
import { passwordComplexityError } from './passwordPolicy'

const router = useRouter()
const authStore = useAuthStore()

const loading = ref(true)
const loadError = ref('')
const capability = ref<PasswordCapability | null>(null)
const dialogVisible = ref(false)
const submitting = ref(false)
const challengeLoading = ref(false)
const challenge = ref<AuthChallenge | null>(null)
const form = reactive({
  oldPassword: '',
  newPassword: '',
  confirmPassword: '',
  captcha: '',
})

async function loadCapability() {
  loading.value = true
  loadError.value = ''
  try {
    const response = await getPasswordCapability()
    if (!response.success || !response.data) {
      throw new Error(response.message || '读取账号密码设置失败')
    }
    capability.value = response.data
  } catch (error: any) {
    capability.value = null
    loadError.value = error?.message || '读取账号密码设置失败'
  } finally {
    loading.value = false
  }
}

async function openDialog() {
  if (!capability.value?.can_change_password) return
  resetForm()
  dialogVisible.value = true
  await loadChallenge()
}

async function loadChallenge() {
  challengeLoading.value = true
  challenge.value = null
  form.captcha = ''
  try {
    const response = await getAuthChallenge()
    if (!response.success || !response.data) {
      throw new Error(response.message || '获取验证码失败')
    }
    challenge.value = response.data
  } catch (error: any) {
    MessagePlugin.error(error?.message || '获取验证码失败')
  } finally {
    challengeLoading.value = false
  }
}

function validateForm(): string {
  if (!form.oldPassword) return '请输入当前密码'
  const complexityError = passwordComplexityError(form.newPassword)
  if (complexityError) return complexityError
  if (form.oldPassword === form.newPassword) return '新密码不能与当前密码相同'
  if (form.newPassword !== form.confirmPassword) return '两次输入的新密码不一致'
  if (!form.captcha.trim()) return '请输入验证码'
  if (!challenge.value) return '请刷新验证码后重试'
  return ''
}

async function submitPasswordChange() {
  if (submitting.value) return
  const validationError = validateForm()
  if (validationError) {
    MessagePlugin.warning(validationError)
    return
  }

  const currentChallenge = challenge.value
  if (!currentChallenge) return

  submitting.value = true
  try {
    const [
      encryptedOldPassword,
      encryptedNewPassword,
      encryptedConfirmPassword,
    ] = await Promise.all([
      encryptAuthPassword(form.oldPassword, currentChallenge.public_key),
      encryptAuthPassword(form.newPassword, currentChallenge.public_key),
      encryptAuthPassword(form.confirmPassword, currentChallenge.public_key),
    ])
    const response = await changeLocalPassword({
      encrypted_old_password: encryptedOldPassword,
      encrypted_new_password: encryptedNewPassword,
      encrypted_confirm_password: encryptedConfirmPassword,
      challenge_id: currentChallenge.challenge_id,
      captcha_answer: form.captcha.trim(),
    })
    if (!response.success) {
      throw new Error(response.message || '密码修改失败')
    }

    dialogVisible.value = false
    resetForm()
    authStore.logout()
    MessagePlugin.success('密码已修改，请使用新密码重新登录')
    await router.replace('/login')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '密码修改失败')
    await loadChallenge()
  } finally {
    submitting.value = false
  }
}

function resetForm() {
  form.oldPassword = ''
  form.newPassword = ''
  form.confirmPassword = ''
  form.captcha = ''
  challenge.value = null
}

function resetDialog() {
  if (submitting.value) return
  resetForm()
}

onMounted(loadCapability)
</script>

<style scoped lang="less">
.password-settings {
  margin-top: 28px;
}

.password-settings__heading {
  margin-bottom: 14px;

  h3 {
    margin: 0 0 4px;
    color: var(--td-text-color-primary);
    font-size: 16px;
  }

  p {
    margin: 0;
    color: var(--td-text-color-secondary);
    font-size: 13px;
  }
}

.password-settings__card {
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  padding: 18px 20px;
  background: var(--td-bg-color-container);
}

.password-settings__state,
.password-settings__info {
  display: flex;
  align-items: center;
  gap: 12px;
}

.password-settings__state {
  color: var(--td-text-color-secondary);
}

.password-settings__info {
  justify-content: space-between;

  p {
    margin: 6px 0 0;
    color: var(--td-text-color-secondary);
    font-size: 13px;
  }
}

.password-settings__label {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--td-text-color-primary);
  font-weight: 500;
}

.password-dialog__notice {
  margin-bottom: 18px;
}

.password-dialog__hint {
  margin: -12px 0 18px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

.password-dialog__captcha {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 132px;
  gap: 10px;
  width: 100%;
}

.password-dialog__captcha-button {
  height: 32px;
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  border-radius: 4px;
  background: var(--td-bg-color-container);
  color: var(--td-text-color-secondary);
  cursor: pointer;

  &:disabled {
    cursor: not-allowed;
    opacity: 0.6;
  }

  img {
    width: 100%;
    height: 100%;
    object-fit: contain;
  }
}

@media (max-width: 640px) {
  .password-settings__info {
    align-items: flex-start;
    flex-direction: column;
  }
}
</style>
