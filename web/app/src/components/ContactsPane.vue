<script setup>
import { computed } from 'vue';
import {
  handleRequest, openFriendChat, openGroupChat, openUserInfo, state,
} from '../store';
import { openDialog } from '../ui';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';

const pending = computed(() => state.requests.filter((r) => r.status === 0));

const friends = computed(() => {
  const q = state.filter.trim().toLowerCase();
  if (!q) return state.friends;
  return state.friends.filter((f) => `${f.nickname || ''} ${f.remark || ''}`.toLowerCase().includes(q));
});

const groups = computed(() => {
  const q = state.filter.trim().toLowerCase();
  if (!q) return state.groups;
  return state.groups.filter((g) => (g.name || '').toLowerCase().includes(q));
});

function openGroup(group, showInfo = false) {
  openGroupChat(group.id, group.name, group.avatar);
  if (showInfo) state.info = { type: 'group', id: String(group.id) };
}
</script>

<template>
  <div class="quick-actions">
    <button class="quick" @click="openDialog({ component: 'add-friend', title: '添加好友' })">
      <Icon name="user-plus" /><span>添加好友</span>
    </button>
    <button class="quick" @click="openDialog({ component: 'create-group', title: '发起群聊' })">
      <Icon name="users" /><span>发起群聊</span>
    </button>
    <button class="quick" @click="openDialog({ component: 'blacklist', title: '黑名单' })">
      <Icon name="ban" /><span>黑名单</span>
    </button>
  </div>

  <template v-if="!state.filter && pending.length">
    <div class="pane-section">新的朋友 <span class="count">{{ pending.length }}</span></div>
    <div v-for="req in pending" :key="req.id" class="row">
      <Avatar :user="req.from_user" :size="40" />
      <div class="row-main">
        <div class="row-name">{{ (req.from_user && req.from_user.nickname) || '未知用户' }}</div>
        <div class="row-sub">{{ req.message || '请求添加你为好友' }}</div>
      </div>
      <div class="row-actions">
        <button class="btn sm" @click="handleRequest(req.id, true)">同意</button>
        <button class="btn sm ghost" @click="handleRequest(req.id, false)">拒绝</button>
      </div>
    </div>
  </template>

  <div class="pane-section">群聊 <span class="count">{{ groups.length }}</span></div>
  <div
    v-for="group in groups"
    :key="group.id"
    class="row"
    role="button"
    tabindex="0"
    @click="openGroup(group)"
    @keydown.enter.prevent="openGroup(group)"
  >
    <Avatar :name="group.name" :src="group.avatar" :size="40" square />
    <div class="row-main">
      <div class="row-name">
        {{ group.name }}
        <span v-if="group.my_role === 2" class="tag owner">群主</span>
        <span v-else-if="group.my_role === 1" class="tag admin">管理员</span>
      </div>
      <div class="row-sub">{{ group.member_count || 0 }} 位成员{{ group.muted ? ' · 已静音' : '' }}</div>
    </div>
    <div class="row-actions">
      <button class="icon-btn" title="群信息" @click.stop="openGroup(group, true)">
        <Icon name="info" />
      </button>
    </div>
  </div>
  <div v-if="!groups.length" class="empty" style="padding:22px">
    <span>还没有加入任何群聊</span>
  </div>

  <div class="pane-section">好友 <span class="count">{{ friends.length }}</span></div>
  <div
    v-for="friend in friends"
    :key="friend.id"
    class="row"
    role="button"
    tabindex="0"
    @click="openFriendChat(friend.id, friend.remark || friend.nickname, friend.avatar)"
    @keydown.enter.prevent="openFriendChat(friend.id, friend.remark || friend.nickname, friend.avatar)"
  >
    <Avatar :user="friend" :size="40" />
    <div class="row-main">
      <div class="row-name">
        {{ friend.remark || friend.nickname }}
        <span v-if="friend.starred" class="pin-flag" title="星标好友"><Icon name="star" /></span>
      </div>
      <div class="row-sub">{{ friend.signature || '这个人很神秘，什么都没留下' }}</div>
    </div>
    <div class="row-actions">
      <span class="status-dot" :class="{ on: friend.online }" :title="friend.online ? '在线' : '离线'" />
      <button class="icon-btn" title="资料" @click.stop="openUserInfo(friend.id)">
        <Icon name="info" />
      </button>
    </div>
  </div>
  <div v-if="!friends.length" class="empty" style="padding:22px">
    <span>还没有好友，先添加一位吧</span>
  </div>
</template>
