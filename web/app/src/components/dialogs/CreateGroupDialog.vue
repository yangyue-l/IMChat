<script setup>
import { onMounted, ref } from 'vue';
import { createGroup, state } from '../../store';
import { closeDialog, toast } from '../../ui';
import Avatar from '../Avatar.vue';

const name = ref('');
const selected = ref([]);
const busy = ref(false);
const nameEl = ref(null);

onMounted(() => nameEl.value && nameEl.value.focus());

function toggle(id) {
  const i = selected.value.indexOf(id);
  if (i >= 0) selected.value.splice(i, 1);
  else selected.value.push(id);
}

async function submit() {
  if (!name.value.trim()) { toast('请输入群名称', 'error'); return; }
  if (busy.value) return;
  busy.value = true;
  try {
    await createGroup(name.value.trim(), selected.value);
    closeDialog();
  } catch (err) {
    toast(err.message, 'error');
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="dlg-body">
    <label class="field">
      <span>群名称</span>
      <input ref="nameEl" v-model="name" maxlength="32" placeholder="给群起个名字" @keydown.enter.prevent="submit" />
    </label>

    <div class="field">
      <span>选择成员（{{ selected.length }}）</span>
      <div class="pick-list">
        <label v-for="friend in state.friends" :key="friend.id" class="pick-row">
          <input
            type="checkbox"
            :checked="selected.includes(String(friend.id))"
            @change="toggle(String(friend.id))"
          />
          <Avatar :user="friend" :size="28" />
          <span class="name">{{ friend.remark || friend.nickname }}</span>
        </label>
        <div v-if="!state.friends.length" class="empty" style="padding:16px">
          <span>先添加好友再建群</span>
        </div>
      </div>
    </div>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">取消</button>
    <button class="btn primary" :disabled="busy" @click="submit">创建</button>
  </footer>
</template>
