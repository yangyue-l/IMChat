<script setup>
import { onMounted, ref } from 'vue';
import { pickAvatar, saveProfile, state } from '../../store';
import { closeDialog, toast } from '../../ui';
import Avatar from '../Avatar.vue';

const nickname = ref(state.me.nickname || '');
const signature = ref(state.me.signature || '');
const preview = ref(state.me.avatar || '');
const busy = ref(false);
const nickEl = ref(null);
const fileEl = ref(null);

onMounted(() => {
  state.pendingAvatar = '';
  if (nickEl.value) nickEl.value.focus();
});

async function onFile(event) {
  const file = event.target.files && event.target.files[0];
  event.target.value = '';
  if (!file) return;
  try {
    preview.value = await pickAvatar(file);
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function submit() {
  if (!nickname.value.trim()) { toast('昵称不能为空', 'error'); return; }
  if (busy.value) return;
  busy.value = true;
  const ok = await saveProfile({ nickname: nickname.value.trim(), signature: signature.value.trim() });
  busy.value = false;
  if (ok) closeDialog();
}
</script>

<template>
  <div class="dlg-body">
    <div style="display:flex;align-items:center;gap:14px;margin-bottom:18px">
      <Avatar :name="nickname" :src="preview" :size="56" />
      <div>
        <button class="btn ghost sm" @click="fileEl.click()">更换头像</button>
        <div class="muted" style="margin-top:6px">支持 jpg / png / gif / webp</div>
      </div>
      <input ref="fileEl" type="file" accept="image/*" hidden @change="onFile" />
    </div>

    <label class="field">
      <span>昵称</span>
      <input ref="nickEl" v-model="nickname" maxlength="32" />
    </label>
    <label class="field">
      <span>个性签名</span>
      <input v-model="signature" maxlength="255" placeholder="说点什么…" />
    </label>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">取消</button>
    <button class="btn primary" :disabled="busy" @click="submit">保存</button>
  </footer>
</template>
