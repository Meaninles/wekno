<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { BRAND_NAME } from "@/custom/modules/branding";
import {
  artifactShareContentURL,
  getArtifactShare,
  getArtifactSharePreview,
  getArtifactSharePreviewContent,
  verifyArtifactSharePassword,
  type ArtifactShareView as ArtifactShareViewData,
} from "../api";

const route = useRoute();
const share = ref<ArtifactShareViewData | null>(null);
const loading = ref(true);
const errorMessage = ref("");
const password = ref("");
const passwordSubmitting = ref(false);
const accessToken = ref("");
const passwordError = ref("");
const previewBlobURL = ref("");
const passwordAttemptsRemaining = ref<number | null>(null);
const passwordAttemptsMax = ref(0);
const passwordRetryAfterSeconds = ref(0);
const passwordLocked = ref(false);

const previewContentSecurityPolicy =
  "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data: blob:; connect-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'";

const token = computed(() => String(route.params.token || "").trim());
const previewToken = computed(() => String(route.query.preview || "").trim());
const isCreatorPreview = computed(() => Boolean(previewToken.value));
const requiresPassword = computed(() =>
  !isCreatorPreview.value && Boolean(share.value?.requires_password) && !accessToken.value,
);
const contentURL = computed(() => {
  if (!token.value) return "";
  if (isCreatorPreview.value) return previewBlobURL.value;
  return artifactShareContentURL(token.value, share.value?.content_url, {
    accessToken: accessToken.value,
  });
});

const passwordAttemptHint = computed(() => {
  if (passwordLocked.value) {
    if (passwordRetryAfterSeconds.value > 0) {
      const minutes = Math.ceil(passwordRetryAfterSeconds.value / 60);
      return `尝试次数已用完，请 ${minutes} 分钟后再试`;
    }
    return "尝试次数已用完，请稍后再试";
  }
  if (passwordAttemptsRemaining.value !== null && passwordAttemptsMax.value > 0) {
    return `还可尝试 ${passwordAttemptsRemaining.value} 次`;
  }
  return "";
});

function clearPreviewBlobURL() {
  if (previewBlobURL.value) {
    URL.revokeObjectURL(previewBlobURL.value);
    previewBlobURL.value = "";
  }
}

function updatePasswordAttemptStatus(source: any) {
  const remaining = Number(source?.password_attempts_remaining ?? source?.remaining_attempts);
  if (Number.isFinite(remaining)) passwordAttemptsRemaining.value = Math.max(0, remaining);

  const maxAttempts = Number(source?.password_attempts_max ?? source?.max_attempts);
  if (Number.isFinite(maxAttempts) && maxAttempts > 0) passwordAttemptsMax.value = maxAttempts;

  const retryAfter = Number(source?.retry_after_seconds);
  passwordRetryAfterSeconds.value = Number.isFinite(retryAfter) ? Math.max(0, retryAfter) : 0;
  const lockedUntil = String(source?.password_locked_until ?? source?.locked_until ?? "").trim();
  passwordLocked.value = passwordRetryAfterSeconds.value > 0 ||
    (Boolean(lockedUntil) && Date.parse(lockedUntil) > Date.now());
}

async function createPreviewBlobURL(blob: Blob) {
  const html = await blob.text();
  const cspMeta = `<meta http-equiv="Content-Security-Policy" content="${previewContentSecurityPolicy}">`;
  const headPattern = /<head\b[^>]*>/i;
  const htmlPattern = /<html\b[^>]*>/i;
  let securedHTML = html;
  if (headPattern.test(securedHTML)) {
    securedHTML = securedHTML.replace(headPattern, (head) => `${head}${cspMeta}`);
  } else if (htmlPattern.test(securedHTML)) {
    securedHTML = securedHTML.replace(htmlPattern, (htmlTag) => `${htmlTag}<head>${cspMeta}</head>`);
  } else {
    securedHTML = `<head>${cspMeta}</head>${securedHTML}`;
  }
  return URL.createObjectURL(new Blob([securedHTML], { type: "text/html" }));
}

async function loadShare() {
  clearPreviewBlobURL();
  share.value = null;
  errorMessage.value = "";
  password.value = "";
  passwordError.value = "";
  accessToken.value = "";
  passwordAttemptsRemaining.value = null;
  passwordAttemptsMax.value = 0;
  passwordRetryAfterSeconds.value = 0;
  passwordLocked.value = false;
  if (!token.value) {
    errorMessage.value = "分享链接无效";
    loading.value = false;
    return;
  }

  loading.value = true;
  try {
    const response: any = isCreatorPreview.value
      ? await getArtifactSharePreview(token.value, previewToken.value)
      : await getArtifactShare(token.value);
    if (!response?.success || !response?.data) {
      throw new Error("分享内容不存在或已失效");
    }
    share.value = response.data;
    updatePasswordAttemptStatus(response.data);
    if (isCreatorPreview.value) {
      const blob = await getArtifactSharePreviewContent(token.value, previewToken.value);
      previewBlobURL.value = await createPreviewBlobURL(blob);
    }
    document.title = `${share.value?.filename || "HTML 预览"} - ${BRAND_NAME}`;
  } catch (error: any) {
    if (isCreatorPreview.value && error?.status === 403) {
      errorMessage.value = "只有创建者可以预览此内容";
    } else if (isCreatorPreview.value && error?.status === 401) {
      errorMessage.value = "请先登录后再预览";
    } else {
      errorMessage.value = "分享内容不存在或已失效";
    }
  } finally {
    loading.value = false;
  }
}

async function unlockShare() {
  const value = password.value;
  if (!value.trim() || passwordSubmitting.value) {
    passwordError.value = "请输入分享密码";
    return;
  }
  if ([...value].length < 6) {
    passwordError.value = "分享密码至少需要 6 个字符";
    return;
  }
  if (new TextEncoder().encode(value).length > 72) {
    passwordError.value = "分享密码长度过长";
    return;
  }
  passwordSubmitting.value = true;
  passwordError.value = "";
  try {
    const response: any = await verifyArtifactSharePassword(token.value, value);
    if (!response?.success || !response?.data?.access_token) {
      throw new Error("分享密码不正确");
    }
    accessToken.value = response.data.access_token;
    password.value = "";
    passwordAttemptsRemaining.value = null;
    passwordRetryAfterSeconds.value = 0;
    passwordLocked.value = false;
    document.title = `${share.value?.filename || "HTML 预览"} - ${BRAND_NAME}`;
  } catch (error: any) {
    const details = error?.error?.details || error?.details || {};
    updatePasswordAttemptStatus(details);
    if (error?.status === 429 || passwordLocked.value) {
      passwordError.value = passwordAttemptHint.value;
    } else if (passwordAttemptsRemaining.value !== null) {
      passwordError.value = `分享密码不正确，还可尝试 ${passwordAttemptsRemaining.value} 次`;
    } else {
      passwordError.value = "分享密码不正确";
    }
  } finally {
    passwordSubmitting.value = false;
  }
}

onBeforeUnmount(() => {
  clearPreviewBlobURL();
});

onMounted(() => {
  void loadShare();
});

watch([token, previewToken], ([currentToken, currentPreview], [previousToken, previousPreview]) => {
  if (currentToken && (currentToken !== previousToken || currentPreview !== previousPreview)) void loadShare();
});
</script>

<template>
  <main class="artifact-share">
    <div v-if="loading" class="artifact-share__state">
      <span>正在加载预览…</span>
    </div>

    <div v-else-if="errorMessage" class="artifact-share__state artifact-share__state--error">
      <strong>无法打开预览</strong>
      <span>{{ errorMessage }}</span>
    </div>

    <div v-else-if="requiresPassword" class="artifact-share__state artifact-share__state--password">
      <strong>请输入分享密码</strong>
      <span>输入创建人提供的密码后查看文件</span>
      <span v-if="passwordAttemptHint" class="artifact-share__password-hint">{{ passwordAttemptHint }}</span>
      <form class="artifact-share__password-form" @submit.prevent="unlockShare">
        <input
          v-model="password"
          type="password"
          autocomplete="current-password"
          placeholder="分享密码"
          minlength="6"
          maxlength="72"
          :disabled="passwordSubmitting || passwordLocked"
          autofocus
        />
        <button type="submit" :disabled="passwordSubmitting || passwordLocked || !password.trim()">
          {{ passwordSubmitting ? "验证中…" : "查看" }}
        </button>
      </form>
      <span v-if="passwordError" class="artifact-share__password-error">{{ passwordError }}</span>
    </div>

    <iframe
      v-else-if="contentURL && !requiresPassword"
      class="artifact-share__frame"
      :src="contentURL"
      :title="share?.filename || 'HTML 预览'"
      sandbox="allow-scripts"
      referrerpolicy="no-referrer"
    />
  </main>
</template>

<style scoped>
.artifact-share {
  width: 100vw;
  min-height: 100vh;
  overflow: hidden;
  background: #fff;
}

.artifact-share__frame {
  display: block;
  width: 100%;
  height: 100vh;
  border: 0;
  background: #fff;
}

.artifact-share__state {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #53615a;
  font-size: 14px;
}

.artifact-share__state--error {
  flex-direction: column;
  gap: 8px;
}

.artifact-share__state--error strong {
  color: #17211c;
  font-size: 18px;
}

.artifact-share__state--password {
  flex-direction: column;
  gap: 10px;
  padding: 24px;
}

.artifact-share__state--password strong {
  color: #17211c;
  font-size: 18px;
}

.artifact-share__password-form {
  display: flex;
  width: min(360px, 100%);
  gap: 8px;
  margin-top: 6px;
}

.artifact-share__password-form input {
  min-width: 0;
  flex: 1;
  height: 36px;
  padding: 0 10px;
  border: 1px solid #c8d0cb;
  border-radius: 4px;
  outline: none;
  font: inherit;
}

.artifact-share__password-form input:focus {
  border-color: #1f9d55;
  box-shadow: 0 0 0 2px rgb(31 157 85 / 12%);
}

.artifact-share__password-form button {
  height: 36px;
  padding: 0 16px;
  border: 0;
  border-radius: 4px;
  color: #fff;
  background: #1f9d55;
  cursor: pointer;
  font: inherit;
}

.artifact-share__password-form button:disabled {
  cursor: default;
  opacity: 0.55;
}

.artifact-share__password-hint {
  color: #53615a;
  font-size: 13px;
}

.artifact-share__password-error {
  color: #d54941;
}
</style>
