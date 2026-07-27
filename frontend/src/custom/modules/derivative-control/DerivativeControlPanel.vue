<template>
  <section class="derivative-control" data-testid="derivative-control-panel">
    <div class="derivative-control__label">
      <span>全局 TPM</span>
      <small>所有衍生任务共享</small>
    </div>
    <t-input-number
      id="derivative-tpm-input"
      v-model="draftTPM"
      :min="100"
      :max="2000000"
      :step="1000"
      theme="normal"
      data-testid="derivative-tpm-input"
    />
    <t-button
      variant="outline"
      :loading="saving"
      data-testid="derivative-tpm-save"
      @click="save"
    >
      保存
    </t-button>
  </section>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'

const props = withDefaults(defineProps<{
  tpm?: number
  saving?: boolean
}>(), {
  tpm: 20_000,
  saving: false,
})

const emit = defineEmits<{
  save: [tpm: number]
}>()

const draftTPM = ref(props.tpm)

watch(() => props.tpm, value => {
  draftTPM.value = value
})

function save() {
  if (!Number.isInteger(draftTPM.value) || draftTPM.value < 100 || draftTPM.value > 2_000_000) {
    MessagePlugin.warning('TPM 请输入 100–2,000,000 的整数')
    return
  }
  emit('save', draftTPM.value)
}
</script>

<style scoped lang="less">
.derivative-control {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 42px;
  margin: -4px 0 16px;
  padding: 8px 12px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-secondarycontainer);
}

.derivative-control__label {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-right: auto;

  span {
    color: var(--td-text-color-primary);
    font-size: 13px;
    font-weight: 600;
  }

  small {
    color: var(--td-text-color-placeholder);
    font-size: 12px;
  }
}

:deep(.t-input-number) {
  width: 150px;
}

@media (max-width: 720px) {
  .derivative-control {
    align-items: stretch;
    flex-wrap: wrap;
  }

  .derivative-control__label {
    flex-basis: 100%;
  }
}
</style>
