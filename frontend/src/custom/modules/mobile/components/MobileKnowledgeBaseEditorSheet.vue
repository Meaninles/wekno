<script setup lang="ts">
import { ref, watch } from "vue";

const props = defineProps<{
  visible: boolean;
  mode: "create" | "edit";
  initialName?: string;
  initialDescription?: string;
  submitting?: boolean;
  canDelete?: boolean;
}>();

const emit = defineEmits<{
  (event: "close"): void;
  (event: "submit", value: { name: string; description: string }): void;
  (event: "delete"): void;
}>();

const name = ref("");
const description = ref("");
const confirmingDelete = ref(false);

watch(
  () => props.visible,
  (visible) => {
    if (!visible) return;
    name.value = props.initialName || "";
    description.value = props.initialDescription || "";
    confirmingDelete.value = false;
  },
  { immediate: true },
);
</script>

<template>
  <div v-if="visible" class="mobile-sheet-backdrop" @click.self="emit('close')">
    <section class="kb-editor-sheet" role="dialog" aria-modal="true">
      <header>
        <strong>{{ mode === 'create' ? '新建知识库' : '编辑库信息' }}</strong>
        <button type="button" aria-label="关闭" @click="emit('close')"><MobileIcon name="close" /></button>
      </header>

      <label>
        <span>知识库名称</span>
        <input v-model.trim="name" maxlength="50" placeholder="请输入名称" autofocus>
      </label>
      <label>
        <span>描述</span>
        <textarea v-model="description" maxlength="200" rows="3" placeholder="选填，说明知识库用途"></textarea>
      </label>

      <div v-if="mode === 'create'" class="default-summary">
        <MobileIcon name="setting" />
        <div>
          <strong>使用平台默认配置</strong>
          <span>文档型知识库 · 默认模型和存储 · 自动分块 · 向量与关键词索引</span>
        </div>
      </div>

      <div v-if="mode === 'edit' && canDelete" class="danger-zone">
        <button v-if="!confirmingDelete" type="button" class="danger-link" @click="confirmingDelete = true">
          <MobileIcon name="delete" />删除知识库
        </button>
        <div v-else class="delete-confirm">
          <span>将删除知识库及全部文档、索引和衍生数据，无法恢复。</span>
          <button type="button" class="secondary" @click="confirmingDelete = false">取消</button>
          <button type="button" class="danger" :disabled="submitting" @click="emit('delete')">确认删除</button>
        </div>
      </div>

      <footer>
        <button type="button" class="secondary" @click="emit('close')">取消</button>
        <button type="button" class="primary" :disabled="!name.trim() || submitting"
          @click="emit('submit', { name, description })">
          {{ submitting ? '正在保存' : (mode === 'create' ? '创建' : '保存') }}
        </button>
      </footer>
    </section>
  </div>
</template>

<style scoped>
.mobile-sheet-backdrop {
  position: fixed;
  z-index: 1100;
  inset: 0;
  display: flex;
  align-items: flex-end;
  background: rgb(16 30 23 / 38%);
}

.kb-editor-sheet {
  width: 100%;
  max-height: 88dvh;
  overflow-y: auto;
  border-radius: 18px 18px 0 0;
  background: #fff;
  padding: 12px 16px calc(env(safe-area-inset-bottom) + 18px);
  box-shadow: 0 -12px 42px rgb(24 48 36 / 18%);
}

header {
  display: flex;
  height: 44px;
  align-items: center;
  justify-content: space-between;
}

header strong { color: #17261f; font-size: 18px; }
header button {
  display: grid;
  width: 34px;
  height: 34px;
  place-items: center;
  border: 0;
  border-radius: 17px;
  background: #eef3f0;
  color: #64766d;
}

label {
  display: flex;
  flex-direction: column;
  gap: 7px;
  margin-top: 14px;
  color: #40534a;
  font-size: 13px;
  font-weight: 650;
}

input,
textarea {
  width: 100%;
  border: 1px solid #d7e3dd;
  border-radius: 9px;
  outline: 0;
  background: #fff;
  color: #17261f;
  padding: 10px 11px;
  font: inherit;
  font-weight: 400;
}

input:focus,
textarea:focus { border-color: #58c88b; box-shadow: 0 0 0 3px rgb(7 193 96 / 8%); }

.default-summary {
  display: grid;
  grid-template-columns: 30px minmax(0, 1fr);
  gap: 9px;
  margin-top: 16px;
  border-radius: 10px;
  background: #f2faf5;
  color: #078f49;
  padding: 12px;
}

.default-summary > :first-child { font-size: 22px; }
.default-summary div { display: flex; flex-direction: column; gap: 4px; }
.default-summary strong { color: #1b382a; font-size: 14px; }
.default-summary span { color: #657a70; font-size: 12px; line-height: 1.5; }

.danger-zone { margin-top: 18px; border-top: 1px solid #edf1ef; padding-top: 14px; }
.danger-link {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  border: 0;
  background: transparent;
  color: #c73737;
  padding: 6px 0;
}

.delete-confirm {
  display: grid;
  grid-template-columns: 1fr auto auto;
  align-items: center;
  gap: 8px;
  border-radius: 10px;
  background: #fff3f3;
  padding: 10px;
}
.delete-confirm span { grid-column: 1 / 4; color: #9f3030; font-size: 12px; line-height: 1.5; }
.delete-confirm button { height: 34px; border-radius: 17px; padding: 0 13px; }
.delete-confirm .danger { border: 1px solid #dc4c4c; background: #dc4c4c; color: #fff; }

footer { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-top: 20px; }
footer button { height: 42px; border-radius: 21px; font-size: 15px; font-weight: 650; }
.secondary { border: 1px solid #d7e3dd; background: #fff; color: #53665d; }
.primary { border: 1px solid #07c160; background: #07c160; color: #fff; }
button:disabled { opacity: 0.6; }
</style>

