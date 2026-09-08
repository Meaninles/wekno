<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { BRAND_NAME } from "@/custom/modules/branding";
import {
  artifactShareContentURL,
  getArtifactShare,
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

const token = computed(() => String(route.params.token || "").trim());
const previewToken = computed(() => String(route.query.preview || "").trim());
// The server decides whether this request carries a valid preview capability.
// Do not trust the mere presence of a query parameter to bypass the password
// gate, otherwise a forged/expired preview URL would render a blank iframe.
const requiresPassword = computed(() => Boolean(share.value?.requires_password) && !accessToken.value);
const contentURL = computed(() => {
  if (!token.value) return "";
  return artifactShareContentURL(token.value, share.value?.content_url, {
    previewToken: previewToken.value,
    accessToken: accessToken.value,
  });
});

async function loadShare() {
  share.value = null;
  errorMessage.value = "";
  password.value = "";
  passwordError.value = "";
  accessToken.value = "";
  if (!token.value) {
    errorMessage.value = "分享链接无效";
    loading.value = false;
    return;
  }

  loading.value = true;
  try {
    const response: any = await getArtifactShare(token.value, {
      previewToken: previewToken.value,
    });
    if (!response?.success || !response?.data) {
      throw new Error("分享内容不存在或已失效");
    }
    share.value = response.data;
    document.title = `${share.value?.filename || "HTML 预览"} - ${BRAND_NAME}`;
  } catch {
    errorMessage.value = "分享内容不存在或已失效";
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
  passwordSubmitting.value = true;
  passwordError.value = "";
  try {
    const response: any = await verifyArtifactSharePassword(token.value, value);
    if (!response?.success || !response?.data?.access_token) {
      throw new Error("分享密码不正确");
    }
    accessToken.value = response.data.access_token;
    password.value = "";
    document.title = `${share.value?.filename || "HTML 预览"} - ${BRAND_NAME}`;
  } catch (error: any) {
    passwordError.value = error?.status === 429
      ? "尝试次数过多，请稍后再试"
      : "分享密码不正确";
  } finally {
    passwordSubmitting.value = false;
  }
}

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
      <form class="artifact-share__password-form" @submit.prevent="unlockShare">
        <input
          v-model="password"
          type="password"
          autocomplete="current-password"
          placeholder="分享密码"
          :disabled="passwordSubmitting"
          autofocus
        />
        <button type="submit" :disabled="passwordSubmitting || !password.trim()">
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

.artifact-share__password-error {
  color: #d54941;
}
</style>
