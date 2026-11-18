<script setup>
import { onMounted, ref } from 'vue';
import { api } from '../../api';
import { sendFriendRequest } from '../../store';
import { closeDialog } from '../../ui';
import Avatar from '../Avatar.vue';
import Icon from '../Icon.vue';

const keyword = ref('');
const busy = ref(false);
const searched = ref(false);
const results = ref([]);
const error = ref('');
const inputEl = ref(null);

onMounted(() => inputEl.value && inputEl.value.focus());

async function search() {
  const kw = keyword.value.trim();
  if (!kw || busy.value) return;
  busy.value = true;
  error.value = '';
  try {
    const list = await api('GET', `/users/search?keyword=${encodeURIComponent(kw)}`);
    results.value = Array.isArray(list) ? list : [];
    searched.value = true;
  } catch (err) {
    error.value = err.message;
  } finally {
    busy.value = false;
  }
}

async function add(user) {
  const ok = await sendFriendRequest(user.id);
  if (ok) closeDialog();
}
</script>

<template>
  <div class="dlg-body">
    <div class="search">
      <Icon name="search" />
      <input ref="inputEl" v-model="keyword" type="search" placeholder="输入用户名或昵称" @keydown.enter.prevent="search" />
    </div>

    <div style="margin-top:12px;max-height:340px;overflow-y:auto">
      <div v-if="busy" class="empty" style="padding:20px"><span>搜索中…</span></div>
      <div v-else-if="error" class="empty" style="padding:20px"><span>{{ error }}</span></div>
      <div v-else-if="searched && !results.length" class="empty" style="padding:20px">
        <span>没有找到相关用户</span>
      </div>
      <div v-for="user in results" :key="user.id" class="row">
        <Avatar :user="user" :size="40" />
        <div class="row-main">
          <div class="row-name">{{ user.nickname }}</div>
          <div class="row-sub">@{{ user.username }}{{ user.signature ? ` · ${user.signature}` : '' }}</div>
        </div>
        <span v-if="user.is_friend" class="tag">已是好友</span>
        <button v-else class="btn sm" @click="add(user)">加好友</button>
      </div>
    </div>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">关闭</button>
    <button class="btn primary" :disabled="busy" @click="search">搜索</button>
  </footer>
</template>
