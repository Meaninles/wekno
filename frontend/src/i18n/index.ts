import { createI18n } from 'vue-i18n'
import zhCN from './locales/zh-CN.ts'

const messages = {
  'zh-CN': zhCN
}

// 二开版本仅提供简体中文；覆盖历史语言偏好，避免旧值导致空白文案。
localStorage.setItem('locale', 'zh-CN')

const i18n = createI18n({
  legacy: false,
  locale: 'zh-CN',
  fallbackLocale: 'zh-CN',
  globalInjection: true,
  // Some translations intentionally embed `<strong>` markup (e.g. agent step summaries).
  // We render them via v-html with our own sanitization, so silence vue-i18n's HTML warning
  // to avoid flooding the console and slowing renders during history loads.
  warnHtmlMessage: false,
  messages
})

export default i18n
