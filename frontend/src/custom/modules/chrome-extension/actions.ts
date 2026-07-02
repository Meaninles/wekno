import type { Router } from 'vue-router'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import { getCurrentUser } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'
import { getApiBaseUrl } from '@/utils/api-base'
import {
  WEKNORA_CHROME_EXTENSION_GUIDE_PATH,
  WEKNORA_CHROME_EXTENSION_PACKAGE_URL,
  configureChromeExtension,
  detectChromeExtension,
  type ChromeExtensionStatus,
} from './bridge'

function confirmDialog(options: {
  header: string
  body: string
  confirmBtn?: string
  cancelBtn?: string
  theme?: 'default' | 'warning' | 'danger' | 'success'
}) {
  return new Promise<boolean>((resolve) => {
    let settled = false
    const finish = (value: boolean) => {
      if (settled) return
      settled = true
      resolve(value)
    }
    const dialog = DialogPlugin.confirm({
      header: options.header,
      body: options.body,
      confirmBtn: options.confirmBtn || '确定',
      cancelBtn: options.cancelBtn || '取消',
      theme: options.theme || 'default',
      onConfirm: () => {
        dialog.hide()
        finish(true)
      },
      onCancel: () => {
        dialog.hide()
        finish(false)
      },
      onClose: () => finish(false),
    })
  })
}

function apiBaseUrlForBrowserExtension() {
  const configured = getApiBaseUrl().trim().replace(/\/+$/, '')
  const apiPath = configured ? `${configured}/api/v1` : '/api/v1'
  return new URL(apiPath, window.location.origin)
    .toString()
    .replace(/\/+$/, '')
}

async function resolveCurrentTenantSnapshot() {
  const authStore = useAuthStore()
  const response = await getCurrentUser()
  const activeTenant = response.data?.tenant
  const activeTenantId = String(
    authStore.effectiveTenantId || activeTenant?.id || authStore.tenant?.id || '',
  )
  const activeTenantName =
    authStore.currentTenantName ||
    activeTenant?.name ||
    authStore.selectedTenantName ||
    authStore.tenant?.name ||
    ''
  const apiKey =
    activeTenant?.api_key ||
    (String(authStore.tenant?.id || '') === activeTenantId ? authStore.tenant?.api_key : '') ||
    ''

  if (!activeTenantId) {
    throw new Error('未识别到当前空间')
  }

  return {
    baseUrl: apiBaseUrlForBrowserExtension(),
    apiKey,
    tenantId: activeTenantId,
    tenantName: activeTenantName || `空间 ${activeTenantId}`,
  }
}

function requireTenantConfig(snapshot: Awaited<ReturnType<typeof resolveCurrentTenantSnapshot>>) {
  if (!snapshot.apiKey) {
    throw new Error('当前账号没有可用于插件配置的 API Key，请使用当前空间 Owner 账号操作')
  }
  return {
    baseUrl: snapshot.baseUrl,
    apiKey: snapshot.apiKey,
    tenantId: snapshot.tenantId,
    tenantName: snapshot.tenantName,
  }
}

function isSameTenantConfigured(status: ChromeExtensionStatus, tenantId: string) {
  return status.configured && status.tenantId && String(status.tenantId) === String(tenantId)
}

export function downloadChromeExtensionPackage() {
  const link = document.createElement('a')
  link.href = WEKNORA_CHROME_EXTENSION_PACKAGE_URL
  link.download = 'weknora-chrome-extension.zip'
  document.body.appendChild(link)
  link.click()
  link.remove()
}

export function openChromeExtensionGuide(router: Router) {
  router.push(WEKNORA_CHROME_EXTENSION_GUIDE_PATH)
}

export async function oneClickConfigureChromeExtension(router: Router) {
  const status = await detectChromeExtension()

  if (!status.installed) {
    const shouldDownload = await confirmDialog({
      header: '未检测到 Chrome 插件',
      body: '请先下载离线插件包并按安装指南安装。是否现在下载并打开安装指南？',
      confirmBtn: '下载并查看指南',
      cancelBtn: '取消',
      theme: 'warning',
    })
    if (shouldDownload) {
      downloadChromeExtensionPackage()
      openChromeExtensionGuide(router)
    }
    return
  }

  const tenantSnapshot = await resolveCurrentTenantSnapshot()

  if (isSameTenantConfigured(status, tenantSnapshot.tenantId)) {
    const shouldOverwrite = await confirmDialog({
      header: '插件已安装并已配置当前空间',
      body: `当前插件已配置到「${tenantSnapshot.tenantName}」。继续操作会重新写入当前空间配置。`,
      confirmBtn: '重新配置',
      cancelBtn: '不用了',
    })
    if (!shouldOverwrite) return
  } else if (status.configured) {
    const configuredName = status.tenantName || status.tenantId || '其他空间'
    const shouldOverwrite = await confirmDialog({
      header: '插件已有配置',
      body: `当前插件已配置到「${configuredName}」。继续操作会覆盖为「${tenantSnapshot.tenantName}」。`,
      confirmBtn: '覆盖配置',
      cancelBtn: '取消',
      theme: 'warning',
    })
    if (!shouldOverwrite) return
  }

  try {
    const tenantConfig = requireTenantConfig(tenantSnapshot)
    await configureChromeExtension(tenantConfig)
    MessagePlugin.success(`Chrome 插件已配置到「${tenantConfig.tenantName}」`)
  } catch (error: any) {
    MessagePlugin.error(error?.message || 'Chrome 插件配置失败')
  }
}
