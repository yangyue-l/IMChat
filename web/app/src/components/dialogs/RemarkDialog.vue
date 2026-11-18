<script setup>
import { onMounted, ref } from 'vue';
import { saveRemark } from '../../store';
import { closeDialog } from '../../ui';

const props = defineProps({
  friendId: { type: String, required: true },
  name: { type: String, default: '' },
});

const value = ref(props.name || '');
const busy = ref(false);
const inputEl = ref(null);

onMounted(() => inputEl.value && inputEl.value.focus());

async function submit() {
  if (busy.value) return;
  busy.value = true;
  const ok = await saveRemark(props.friendId, value.value.trim());
  busy.value = false;
  if (ok) closeDialog();
}
</script>

<template>
  <div class="dlg-body">
    <label class="field">
      <span>备注名</span>
      <input ref="inputEl" v-model="value" maxlength="32" placeholder="留空则使用昵称" @keydown.enter.prevent="submit" />
    </label>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">取消</button>
    <button class="btn primary" :disabled="busy" @click="submit">保存</button>
  </footer>
</template>
