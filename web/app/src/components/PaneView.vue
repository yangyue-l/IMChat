<script setup>
import { computed } from 'vue';
import { state } from '../store';
import { openDialog, openMenu } from '../ui';
import Icon from './Icon.vue';
import ConvsPane from './ConvsPane.vue';
import ContactsPane from './ContactsPane.vue';

const title = computed(() => (state.tab === 'contacts' ? '通讯录' : '消息'));
const count = computed(() => (state.tab === 'convs' && state.convs.length ? state.convs.length : 0));
const placeholder = computed(() => (state.tab === 'contacts' ? '搜索联系人与群聊' : '搜索会话'));

function actionMenu(event) {
  const rect = event.currentTarget.getBoundingClientRect();
  openMenu(rect, [
    { label: '添加好友', icon: 'user-plus', action: () => openDialog({ component: 'add-friend', title: '添加好友' }) },
    { label: '发起群聊', icon: 'users', action: () => openDialog({ component: 'create-group', title: '发起群聊' }) },
    { label: '黑名单', icon: 'ban', action: () => openDialog({ component: 'blacklist', title: '黑名单' }) },
  ]);
}
</script>

<template>
  <section class="pane" aria-label="列表">
    <div class="pane-head">
      <div class="pane-title">
        <h2>{{ title }}</h2>
        <span v-if="count" class="count">{{ count }}</span>
        <div class="spacer" />
        <button class="icon-btn" title="新建" aria-label="新建" @click="actionMenu">
          <Icon name="plus" />
        </button>
      </div>
      <div class="search pane-search">
        <Icon name="search" />
        <input v-model="state.filter" type="search" :placeholder="placeholder" aria-label="搜索" />
      </div>
    </div>
    <div class="pane-body">
      <ConvsPane v-if="state.tab === 'convs'" />
      <ContactsPane v-else />
    </div>
  </section>
</template>
