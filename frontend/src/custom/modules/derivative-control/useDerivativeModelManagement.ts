import { computed, ref } from 'vue'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import {
  getDerivativeAdminConfig,
  publishDerivativeModel,
  setDefaultDerivativeModel,
  testDerivativeModel,
  unpublishDerivativeModel,
  updateDerivativeTPM,
  type DerivativeAdminConfig,
} from '@/api/derivative-control'

export interface DerivativeCardModel {
  id: string
  tenantID?: number
  name: string
  displayName?: string
}

export function useDerivativeModelManagement() {
  const config = ref<DerivativeAdminConfig | null>(null)
  const savingTPM = ref(false)
  const publishedIDs = computed(() =>
    new Set((config.value?.models || []).map(model => model.id)),
  )

  const displayName = (model: DerivativeCardModel) =>
    model.displayName?.trim() || model.name

  async function load() {
    try {
      config.value = await getDerivativeAdminConfig()
    } catch (error: any) {
      console.error('加载衍生任务配置失败:', error)
      MessagePlugin.error(error?.message || '加载衍生任务配置失败')
    }
  }

  const isPublished = (model: DerivativeCardModel) => publishedIDs.value.has(model.id)
  const isDefault = (model: DerivativeCardModel) => config.value?.default_model_id === model.id
  const tagLabel = (model: DerivativeCardModel) => {
    if (isDefault(model)) return '默认'
    if (isPublished(model)) return '已下发'
    return '未下发'
  }
  const tagTheme = (model: DerivativeCardModel): 'primary' | 'success' | 'default' => {
    if (isDefault(model)) return 'primary'
    if (isPublished(model)) return 'success'
    return 'default'
  }

  async function saveTPM(tpm: number) {
    savingTPM.value = true
    try {
      const value = await updateDerivativeTPM(tpm)
      if (config.value) config.value.tpm = value
      MessagePlugin.success('TPM 已保存')
    } catch (error: any) {
      MessagePlugin.error(error?.message || '保存 TPM 失败')
    } finally {
      savingTPM.value = false
    }
  }

  async function publish(model: DerivativeCardModel) {
    if (!model.tenantID) {
      MessagePlugin.error('模型租户信息缺失，无法下发')
      return
    }
    try {
      config.value = await publishDerivativeModel(model.id, model.tenantID)
      MessagePlugin.success('模型已下发')
    } catch (error: any) {
      MessagePlugin.error(error?.message || '下发模型失败')
    }
  }

  async function setDefault(model: DerivativeCardModel) {
    try {
      config.value = await setDefaultDerivativeModel(model.id)
      MessagePlugin.success('默认模型已更新')
    } catch (error: any) {
      MessagePlugin.error(error?.message || '设置默认模型失败')
    }
  }

  async function test(model: DerivativeCardModel) {
    try {
      const result = await testDerivativeModel(model.id)
      MessagePlugin.success(`测试成功（${result.elapsed_ms}ms）`)
    } catch (error: any) {
      MessagePlugin.error(error?.message || '模型测试失败')
    }
  }

  function confirmUnpublish(model: DerivativeCardModel) {
    const dialog = DialogPlugin.confirm({
      header: '撤回衍生任务模型',
      body: `确定撤回“${displayName(model)}”吗？`,
      confirmBtn: '撤回',
      cancelBtn: '取消',
      theme: 'warning',
      onConfirm: async () => {
        try {
          config.value = await unpublishDerivativeModel(model.id)
          MessagePlugin.success('模型已撤回')
        } catch (error: any) {
          MessagePlugin.error(error?.message || '撤回模型失败')
        }
        dialog.destroy()
      },
    })
  }

  return {
    config,
    savingTPM,
    load,
    isPublished,
    isDefault,
    tagLabel,
    tagTheme,
    saveTPM,
    publish,
    setDefault,
    test,
    confirmUnpublish,
  }
}
