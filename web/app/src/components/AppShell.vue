<script setup>
import { computed } from 'vue';
import { logout, pendingCount, state, switchTab, toggleTheme, unreadTotal } from '../store';
import { openDialog, openMenu, ui } from '../ui';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';
import PaneView from './PaneView.vue';
import ChatView from './ChatView.vue';
import MomentsView from './MomentsView.vue';
import InfoPane from './InfoPane.vue';

const showChat = computed(() => state.tab === 'moments' || !!state.current);
const emptyCopy = computed(() => ({
  convs: ['选一个会话开始聊天', '左侧是最近的会话，也可以去通讯录找朋友。'],
  contacts: ['从通讯录挑个人聊聊', '添加好友、发起群聊，或者查看某位朋友的资料。'],
}[state.tab] || ['选一个会话开始聊天', '']));

const openProfile = () => openDialog({ component: 'edit-profile', title: '编辑资料' });
const openAddFriend = () => openDialog({ component: 'add-friend', title: '添加好友' });
const openBlacklist = () => openDialog({ component: 'blacklist', title: '黑名单' });

function meMenu(event) {
  const rect = event.currentTarget.getBoundingClientRect();
  openMenu(rect, [
    { label: '编辑资料', icon: 'edit', action: openProfile },
    { label: '添加好友', icon: 'user-plus', action: openAddFriend },
    { label: '黑名单', icon: 'ban', action: openBlacklist },
    { sep: true },
    { label: '退出登录', icon: 'logout', danger: true, action: logout },
  ]);
}
</script>

<template>
  <div
    class="app"
    :class="{
      'has-info': !!state.info,
      'focus-mode': state.tab === 'moments',
      'show-chat': ui.mobileChat && showChat,
    }"
  >
    <nav class="rail" aria-label="主导航">
      <div class="mark" title="IMChat">IM</div>
      <button
        class="rail-btn"
        :class="{ 'is-active': state.tab === 'convs' }"
        title="消息" aria-label="消息"
        @click="switchTab('convs')"
      >
        <Icon name="chat" />
        <span v-if="unreadTotal" class="badge">{{ unreadTotal > 99 ? '99+' : unreadTotal }}</span>
      </button>
      <button
        class="rail-btn"
        :class="{ 'is-active': state.tab === 'contacts' }"
        title="通讯录" aria-label="通讯录"
        @click="switchTab('contacts')"
      >
        <Icon name="users" />
        <span v-if="pendingCount" class="badge">{{ pendingCount > 99 ? '99+' : pendingCount }}</span>
      </button>
      <button
        class="rail-btn"
        :class="{ 'is-active': state.tab === 'moments' }"
        title="朋友圈" aria-label="朋友圈"
        @click="switchTab('moments')"
      >
        <Icon name="compass" />
      </button>
      <div class="rail-spacer" />
      <button class="rail-btn" title="切换主题" aria-label="切换主题" @click="toggleTheme">
        <Icon :name="state.theme === 'dark' ? 'sun' : 'moon'" />
      </button>
      <div class="rail-sep" />
      <button class="rail-btn" title="我的账号" aria-label="我的账号" @click="meMenu">
        <Avatar :user="state.me" :size="32" />
      </button>
    </nav>

    <PaneView v-if="state.tab !== 'moments'" />

    <main class="chat">
      <MomentsView v-if="state.tab === 'moments'" />
      <ChatView v-else-if="state.current" />
      <div v-else class="chat-empty">
        <div class="chat-empty-inner">
          <div class="art"><Icon :name="state.tab === 'contacts' ? 'users' : 'chat'" /></div>
          <b>{{ emptyCopy[0] }}</b>
          <p>{{ emptyCopy[1] }}</p>
        </div>
      </div>
    </main>

    <InfoPane v-if="state.info" />
  </div>
</template>
