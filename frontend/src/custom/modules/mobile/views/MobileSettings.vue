<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { useAuthStore } from "@/stores/auth";

type PermissionStateText = "已授权" | "未授权" | "询问时授权" | "不支持检测";

const router = useRouter();
const route = useRoute();
const authStore = useAuthStore();

const cameraState = ref<PermissionStateText>("不支持检测");
const storageState = ref<PermissionStateText>("询问时授权");

const normalizePermission = (state?: PermissionState): PermissionStateText => {
  if (state === "granted") return "已授权";
  if (state === "denied") return "未授权";
  if (state === "prompt") return "询问时授权";
  return "不支持检测";
};

const returnTo = computed(() => {
  const raw = Array.isArray(route.query.returnTo) ? route.query.returnTo[0] : route.query.returnTo;
  if (typeof raw === "string" && (raw === "/chat" || raw.startsWith("/chat/"))) return raw;
  return "/chat";
});

const backToChat = () => {
  router.replace(returnTo.value);
};

const accountDisplayName = computed(() => authStore.user?.display_name || authStore.user?.username || "企业用户");

const openKnowledgeManager = () => {
  router.push({
    path: "/settings/knowledge",
    query: { returnTo: returnTo.value },
  });
};

const refreshPermissions = async () => {
  storageState.value = "询问时授权";
  try {
    const permissions = navigator.permissions;
    if (!permissions?.query) return;
    const camera = await permissions.query({ name: "camera" as PermissionName });
    cameraState.value = normalizePermission(camera.state);
  } catch {
    cameraState.value = "不支持检测";
  }
};

onMounted(refreshPermissions);
</script>

<template>
  <main class="mobile-settings">
    <header class="settings-topbar">
      <button type="button" class="icon-button" aria-label="返回聊天" @click="backToChat">
        <MobileIcon name="chevron-left" />
      </button>
      <strong>设置</strong>
      <span />
    </header>

    <section class="account-card">
      <div class="account-avatar">W</div>
      <div class="account-info">
        <strong>{{ accountDisplayName }}</strong>
      </div>
      <em>统一身份认证</em>
    </section>

    <section class="settings-section">
      <button type="button" class="settings-row primary" @click="openKnowledgeManager">
        <span class="row-icon"><MobileIcon name="folder" /></span>
        <span class="row-text">
          <strong>知识库管理</strong>
          <small>上传、下载、删除文档</small>
        </span>
        <MobileIcon name="chevron-right" />
      </button>
    </section>

    <section class="settings-section">
      <div class="section-title">手机权限</div>
      <div class="settings-row">
        <span class="row-icon"><MobileIcon name="image" /></span>
        <span class="row-text">
          <strong>相册与拍照</strong>
          <small>用于聊天图片上传</small>
        </span>
        <em class="status-chip">{{ cameraState }}</em>
      </div>
      <div class="settings-row">
        <span class="row-icon"><MobileIcon name="file" /></span>
        <span class="row-text">
          <strong>文件选择</strong>
          <small>用于聊天附件和知识库文档上传</small>
        </span>
        <em class="status-chip">{{ storageState }}</em>
      </div>
      <button type="button" class="refresh-button" @click="refreshPermissions">刷新权限状态</button>
    </section>
  </main>
</template>

<style scoped>
.mobile-settings {
  min-height: 100dvh;
  background: #f5f7f8;
  padding-bottom: calc(env(safe-area-inset-bottom) + 18px);
}

.settings-topbar {
  display: grid;
  grid-template-columns: 42px 1fr 42px;
  align-items: center;
  padding: calc(env(safe-area-inset-top) + 8px) 12px 8px;
}

.settings-topbar strong {
  font-size: 17px;
  text-align: center;
}

.icon-button {
  display: grid;
  width: 38px;
  height: 38px;
  place-items: center;
  border: 1px solid #dce6e1;
  border-radius: 10px;
  background: #fff;
  color: #24372f;
  padding: 0;
}

.account-card {
  display: grid;
  grid-template-columns: 46px minmax(0, 1fr) auto;
  align-items: center;
  gap: 10px;
  margin: 8px 12px 14px;
  border: 1px solid #dfe9e4;
  border-radius: 8px;
  background: #fff;
  padding: 12px;
}

.account-avatar {
  display: grid;
  width: 44px;
  height: 44px;
  place-items: center;
  border-radius: 12px;
  background: #07c160;
  color: #fff;
  font-size: 24px;
  font-weight: 700;
}

.account-info {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 3px;
}

.account-info strong,
.account-info span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.account-info strong {
  color: #15211d;
  font-size: 16px;
}

.account-info span {
  color: #788982;
  font-size: 13px;
}

.account-card em,
.status-chip {
  border-radius: 999px;
  background: #eef9f3;
  color: #078f49;
  font-size: 12px;
  font-style: normal;
  padding: 5px 8px;
  white-space: nowrap;
}

.settings-section {
  margin: 0 12px 14px;
  border: 1px solid #dfe9e4;
  border-radius: 8px;
  background: #fff;
  padding: 6px;
}

.section-title {
  color: #73847c;
  font-size: 13px;
  font-weight: 650;
  padding: 8px 8px 4px;
}

.settings-row {
  display: grid;
  width: 100%;
  min-height: 58px;
  grid-template-columns: 34px minmax(0, 1fr) auto;
  align-items: center;
  gap: 10px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #1c2d25;
  padding: 8px;
  text-align: left;
}

.settings-row.primary {
  background: #f2fbf6;
}

.row-icon {
  display: grid;
  width: 32px;
  height: 32px;
  place-items: center;
  border-radius: 8px;
  background: #eef9f3;
  color: #07a557;
}

.row-text {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 3px;
}

.row-text strong,
.row-text small {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.row-text strong {
  font-size: 15px;
}

.row-text small {
  color: #788982;
  font-size: 13px;
}

.refresh-button {
  width: calc(100% - 16px);
  height: 38px;
  margin: 6px 8px 8px;
  border: 1px solid #bfe8cf;
  border-radius: 8px;
  background: #f6fcf8;
  color: #078f49;
  font-weight: 650;
}
</style>
