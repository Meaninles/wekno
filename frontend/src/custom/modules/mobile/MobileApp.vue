<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { MessagePlugin } from "tdesign-vue-next";
import zhCNConfig from "tdesign-vue-next/esm/locale/zh_CN";
import { getCurrentUser, userInfoFromApi } from "@/api/auth";
import { useAuthStore } from "@/stores/auth";

const router = useRouter();
const authStore = useAuthStore();

const booting = ref(true);
const redirecting = ref(false);
const bootMessage = ref("正在进入企业微信工作台");

const tdGlobalConfig = computed(() => zhCNConfig);

const decodeOIDCResult = (encoded: string) => {
  const normalized = encoded.replace(/-/g, "+").replace(/_/g, "/");
  const padded = normalized + "=".repeat((4 - (normalized.length % 4)) % 4);
  const binary = window.atob(padded);
  const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
  return JSON.parse(new TextDecoder().decode(bytes));
};

const clearOIDCCallbackState = (path = "/chat") => {
  window.history.replaceState({}, document.title, path);
};

const syncOIDCUserContext = async () => {
  const currentUserResponse = await getCurrentUser();
  if (!currentUserResponse.success || !currentUserResponse.data?.user) {
    throw new Error(currentUserResponse.message || "获取用户信息失败");
  }

  const { user, tenant, memberships } = currentUserResponse.data;
  authStore.setUser(userInfoFromApi(user, tenant?.id));
  if (tenant) {
    authStore.setTenant({
      id: String(tenant.id) || "",
      name: tenant.name || "",
      api_key: tenant.api_key || "",
      owner_id: tenant.owner_id || user.id || "",
      description: tenant.description,
      status: tenant.status,
      business: tenant.business,
      storage_quota: tenant.storage_quota,
      storage_used: tenant.storage_used,
      created_at: tenant.created_at || new Date().toISOString(),
      updated_at: tenant.updated_at || new Date().toISOString(),
    });
  }
  if (Array.isArray(memberships)) {
    authStore.setMemberships(memberships);
  }

  const activeIdNum = tenant?.id != null ? Number(tenant.id) : NaN;
  const homeIdNum = user.tenant_id != null ? Number(user.tenant_id) : NaN;
  if (Number.isFinite(activeIdNum) && Number.isFinite(homeIdNum) && activeIdNum !== homeIdNum) {
    authStore.setSelectedTenant(activeIdNum, tenant?.name || null);
  } else {
    authStore.setSelectedTenant(null, null);
  }
};

const handleOIDCCallback = async () => {
  const hash = window.location.hash.startsWith("#") ? window.location.hash.slice(1) : "";
  if (!hash) return false;

  const params = new URLSearchParams(hash);
  const oidcError = params.get("oidc_error");
  const oidcErrorDescription = params.get("oidc_error_description");
  const oidcResult = params.get("oidc_result");

  if (!oidcError && !oidcResult) return false;

  if (oidcError) {
    clearOIDCCallbackState("/chat");
    throw new Error(oidcErrorDescription || oidcError || "统一身份认证失败");
  }

  if (!oidcResult) {
    clearOIDCCallbackState("/chat");
    throw new Error("统一身份认证失败");
  }

  const response = decodeOIDCResult(oidcResult);
  if (!response.success || !response.token) {
    clearOIDCCallbackState("/chat");
    throw new Error(response.message || "统一身份认证失败");
  }

  authStore.setToken(response.token);
  if (response.refresh_token) {
    authStore.setRefreshToken(response.refresh_token);
  }

  await syncOIDCUserContext();
  clearOIDCCallbackState("/chat");
  await nextTick();
  await router.replace("/chat");
  return true;
};

const redirectToEnterpriseSSO = () => {
  if (redirecting.value) return;
  redirecting.value = true;
  bootMessage.value = "正在跳转统一身份认证";
  window.location.replace("/api/v1/custom/iam/sso/entry");
};

const bootstrap = async () => {
  booting.value = true;
  try {
    await handleOIDCCallback();

    if (authStore.isLoggedIn) {
      booting.value = false;
      return;
    }

    if (authStore.token) {
      const ok = await authStore.refreshFromAuthMe();
      if (ok && authStore.isLoggedIn) {
        booting.value = false;
        return;
      }
    }

    redirectToEnterpriseSSO();
  } catch (error: any) {
    console.error("[mobile] SSO bootstrap failed", error);
    MessagePlugin.error(error?.message || "统一身份认证失败");
    window.setTimeout(redirectToEnterpriseSSO, 900);
  }
};

onMounted(bootstrap);
</script>

<template>
  <t-config-provider :globalConfig="tdGlobalConfig">
    <div class="mobile-shell">
      <RouterView v-if="!booting && authStore.isLoggedIn" />
      <div v-else class="mobile-boot">
        <div class="mobile-boot__mark">W</div>
        <div class="mobile-boot__title">WeKnora</div>
        <div class="mobile-boot__status">
          <span>{{ bootMessage }}</span>
          <i />
          <i />
          <i />
        </div>
      </div>
    </div>
  </t-config-provider>
</template>

<style>
:root {
  --mobile-font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "HarmonyOS Sans SC", "MiSans", "Noto Sans SC", "Source Han Sans SC", "Microsoft YaHei UI", "Microsoft YaHei", "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  --mobile-font-family-mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace;
  --mobile-base-font-size: 17px;
  --mobile-reading-font-size: 18.5px;
  --mobile-reading-line-height: 2.22;
}

#mobile-app {
  --app-font-family: var(--mobile-font-family);
  --app-font-family-mono: var(--mobile-font-family-mono);
  --td-font-family: var(--mobile-font-family);
  --td-font-family-medium: var(--mobile-font-family);
}

html,
body,
#mobile-app {
  width: 100%;
  min-width: 0;
  height: 100%;
  margin: 0;
  padding: 0;
  background: #f5f7f8;
  color: #15211d;
  font-family: var(--mobile-font-family);
  font-size: var(--mobile-base-font-size);
  font-weight: 400;
  font-feature-settings: "kern" 1;
  font-kerning: normal;
  text-rendering: optimizeLegibility;
  -webkit-text-size-adjust: 100%;
  -webkit-font-smoothing: antialiased;
  -moz-osx-font-smoothing: grayscale;
  overscroll-behavior: none;
}

* {
  box-sizing: border-box;
}

button,
input,
textarea {
  font-family: var(--mobile-font-family);
  font-size: inherit;
}

pre,
code,
kbd,
samp {
  font-family: var(--mobile-font-family-mono);
}

button {
  -webkit-tap-highlight-color: transparent;
}

.mobile-shell {
  height: 100%;
  min-height: 100dvh;
  background: #f5f7f8;
}

.mobile-boot {
  display: flex;
  min-height: 100dvh;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 12px;
  padding: 24px;
}

.mobile-boot__mark {
  display: grid;
  width: 52px;
  height: 52px;
  place-items: center;
  border-radius: 14px;
  background: #07c160;
  color: white;
  font-size: 28px;
  font-weight: 700;
}

.mobile-boot__title {
  font-size: 19px;
  font-weight: 650;
}

.mobile-boot__status {
  display: flex;
  align-items: center;
  gap: 5px;
  color: #6d7b75;
  font-size: 14px;
}

.mobile-boot__status i {
  width: 4px;
  height: 4px;
  border-radius: 50%;
  background: #07c160;
  animation: mobileBootPulse 1.2s ease-in-out infinite;
}

.mobile-boot__status i:nth-child(3) {
  animation-delay: 0.16s;
}

.mobile-boot__status i:nth-child(4) {
  animation-delay: 0.32s;
}

@keyframes mobileBootPulse {
  0%,
  100% {
    opacity: 0.25;
    transform: translateY(0);
  }
  50% {
    opacity: 1;
    transform: translateY(-2px);
  }
}
</style>
