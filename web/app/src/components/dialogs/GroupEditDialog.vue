<script setup>
import { onMounted, ref } from 'vue';
import { loadGroupDetail, saveGroup, state } from '../../store';
import { closeDialog, toast } from '../../ui';

const props = defineProps({
  groupId: { type: String, required: true },
});

const name = ref('');
const notice = ref('');
const busy = ref(false);
const nameEl = ref(null);

onMounted(async () => {
  if (!state.groupDetails[props.groupId]) await loadGroupDetail(props.groupId);
  const detail = state.groupDetails[props.groupId];
  if (detail) {
    name.value = detail.group.name || '';
    notice.value = detail.group.notice || '';
  }
  if (nameEl.value) nameEl.value.focus();
});

async function submit() {
  if (!name.value.trim()) { toast('群名称不能为空', 'error'); return; }
  if (busy.value) return;
  busy.value = true;
  const ok = await saveGroup(props.groupId, { name: name.value.trim(), notice: notice.value.trim() });
  busy.value = false;
  if (ok) closeDialog();
}
</script>

<template>
  <div class="dlg-body">
    <label class="field">
      <span>群名称</span>
      <input ref="nameEl" v-model="name" maxlength="32" />
    </label>
    <label class="field">
      <span>群公告</span>
      <textarea v-model="notice" rows="4" maxlength="500" placeholder="写点什么让成员知道…" />
    </label>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">取消</button>
    <button class="btn primary" :disabled="busy" @click="submit">保存</button>
  </footer>
</template>
