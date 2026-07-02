export const WEKNORA_CHROME_EXTENSION_ID = 'jpemjbopikggjlmikmclgbmkhhopjdgd'
export const WEKNORA_CHROME_EXTENSION_PACKAGE_URL = '/downloads/weknora-chrome-extension.zip'
export const WEKNORA_CHROME_EXTENSION_GUIDE_PATH = '/platform/chrome-extension-guide'

export interface ChromeExtensionStatus {
  success: boolean
  installed: boolean
  configured: boolean
  tenantId?: string
  tenantName?: string
  baseUrl?: string
  authType?: string
  loginType?: string
  error?: string
}

export interface ChromeExtensionConfigurePayload {
  baseUrl: string
  apiKey: string
  tenantId: string
  tenantName: string
}

type ChromeRuntime = {
  sendMessage: (
    extensionId: string,
    message: unknown,
    callback: (response: any) => void
  ) => void
  lastError?: { message?: string }
}

function getChromeRuntime(): ChromeRuntime | null {
  const runtime = (window as any).chrome?.runtime
  if (!runtime || typeof runtime.sendMessage !== 'function') return null
  return runtime as ChromeRuntime
}

function sendExtensionMessage<T>(message: unknown, timeoutMs = 1400): Promise<T> {
  const runtime = getChromeRuntime()
  if (!runtime) {
    return Promise.reject(new Error('当前浏览器不支持插件通信'))
  }

  return new Promise<T>((resolve, reject) => {
    let settled = false
    const timer = window.setTimeout(() => {
      if (settled) return
      settled = true
      reject(new Error('未检测到已安装的 Chrome 插件'))
    }, timeoutMs)

    try {
      runtime.sendMessage(WEKNORA_CHROME_EXTENSION_ID, message, (response: any) => {
        if (settled) return
        settled = true
        window.clearTimeout(timer)
        const err = runtime.lastError?.message
        if (err) {
          reject(new Error(err))
          return
        }
        resolve(response as T)
      })
    } catch (error: any) {
      if (settled) return
      settled = true
      window.clearTimeout(timer)
      reject(error)
    }
  })
}

export async function detectChromeExtension(): Promise<ChromeExtensionStatus> {
  try {
    const response = await sendExtensionMessage<ChromeExtensionStatus>({
      type: 'WEKNORA_EXTENSION_PING',
    })
    if (!response?.success) {
      return {
        success: false,
        installed: true,
        configured: false,
        error: response?.error || '插件状态检测失败',
      }
    }
    return {
      ...response,
      installed: true,
      configured: !!response.configured,
    }
  } catch (error: any) {
    return {
      success: false,
      installed: false,
      configured: false,
      error: error?.message || '未检测到已安装的 Chrome 插件',
    }
  }
}

export async function configureChromeExtension(
  payload: ChromeExtensionConfigurePayload,
): Promise<ChromeExtensionStatus> {
  const response = await sendExtensionMessage<ChromeExtensionStatus>(
    {
      type: 'WEKNORA_EXTENSION_CONFIGURE',
      payload,
    },
    8000,
  )
  if (!response?.success) {
    throw new Error(response?.error || '插件配置失败')
  }
  return {
    ...response,
    installed: true,
    configured: true,
  }
}

