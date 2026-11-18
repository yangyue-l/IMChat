<script setup>
import { onMounted, ref } from 'vue';
import { loadBlacklist, state, unblockUser } from '../../store';
import { closeDialog } from '../../ui';
import Avatar from '../Avatar.vue';

const loading = ref(true);

onMounted(async () => {
  await loadBlacklist();
  loading.value = false;
});
</script>

<template>
  <div class="dlg-body">
    <div v-if="loading" class="empty" style="padding:24px"><span>加载中…</span></div>
    <div v-else-if="!state.blocked.length" class="empty" style="padding:28px">
      <span>黑名单是空的</span>
    </div>
    <template v-else>
      <div v-for="user in state.blocked" :key="user.id" class="row">
        <Avatar :user="user" :size="40" />
        <div class="row-main">
          <div class="row-name">{{ user.nickname }}</div>
          <div class="row-sub">{{ user.signature || '已被你拉黑' }}</div>
        </div>
        <button class="btn sm ghost" @click="unblockUser(user.id)">解除</button>
      </div>
    </template>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">关闭</button>
  </footer>
</template>
