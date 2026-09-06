<script setup lang="ts">
const props = defineProps(['reviewImg', 'reviewUrl'])
const emit = defineEmits(['closePreImg'])
const close = () => {
    emit('closePreImg')
}
</script>
<template>
    <t-image-viewer v-if="reviewImg && reviewUrl" :visible="reviewImg" closeOnOverlay closeOnEscKeydown @close="close"
        :images="[reviewUrl]">
        <template #trigger><span></span></template>
    </t-image-viewer>
</template>
<style scoped lang="less"></style>
