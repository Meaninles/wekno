import { createI18n } from 'vue-i18n'
import zhCN from './locales/zh-CN'

const SUPPORTED_LOCALES = ['zh-CN'] as const
export type EmbedLocale = (typeof SUPPORTED_LOCALES)[number]

/** 嵌入页沿用独立存储键，但二开版本固定使用简体中文。 */
export const EMBED_LOCALE_STORAGE_KEY = 'weknora-embed-locale'

export function normalizeEmbedLocale(_raw: string): EmbedLocale {
  return 'zh-CN'
}

export function readEmbedLocaleFromUrl(): string {
  if (typeof window === 'undefined') return ''
  return new URLSearchParams(window.location.search).get('locale')?.trim() || ''
}

const i18n = createI18n({
  legacy: false,
  locale: 'zh-CN',
  fallbackLocale: 'zh-CN',
  globalInjection: true,
  warnHtmlMessage: false,
  messages: {
    'zh-CN': zhCN,
  },
})

type LocaleRef = { value: string }

/** 保留宿主调用接口，但所有输入均归一为简体中文。 */
export function applyEmbedLocale(_raw: string, localeRef?: LocaleRef) {
  const next: EmbedLocale = 'zh-CN'
  try {
    localStorage.setItem(EMBED_LOCALE_STORAGE_KEY, next)
  } catch {
    // localStorage 在隐私模式下可能不可用。
  }
  if (localeRef) {
    localeRef.value = next
  } else {
    i18n.global.locale.value = next
  }
}

/** URL 中即使携带其他 locale，也固定应用简体中文。 */
export function syncEmbedLocaleFromUrl(localeRef: LocaleRef): boolean {
  const fromUrl = readEmbedLocaleFromUrl()
  if (!fromUrl) return false
  applyEmbedLocale(fromUrl, localeRef)
  return true
}

export default i18n
