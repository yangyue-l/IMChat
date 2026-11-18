<script setup>
import { computed, ref, watch } from 'vue';
import {
  blockUser, closeInfo, deleteFriend, dissolveGroup, leaveGroup, loadGroupDetail,
  loadUser, muteMember, openFriendChat, openUserInfo, removeMember, sendFriendRequest,
  setAdmin, state, toggleStar,
} from '../store';
import { openDialog, openMenu } from '../ui';
import { sameId } from '../utils';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';

const info = computed(() => state.info);
const group = computed(
  () => (info.value && info.value.type === 'group' ? state.groupDetails[info.value.id] : null),
);
const user = ref(null);

watch(
  info,
  async (value) => {
    if (!value) { user.value = null; return; }
    if (value.type === 'group') {
      user.value = null;
      if (!state.groupDetails[value.id]) loadGroupDetail(value.id);
      return;
    }
    user.value = state.friends.find((f) => sameId(f.id, value.id)) || null;
    if (!user.value) {
      try { user.value = await loadUser(value.id); } catch (_) { user.value = null; }
    }
  },
  { immediate: true },
);

const isFriend = computed(() => !!(user.value && state.friends.some((f) => sameId(f.id, user.value.id))));
const isSelf = computed(() => !!(user.value && sameId(user.value.id, state.me.id)));
const displayName = computed(() => (user.value && (user.value.remark || user.value.nickname)) || '');
const owner = computed(() => {
  if (!group.value) return null;
  return (group.value.members || []).find((m) => m.role === 2) || null;
});

function memberMenu(event, member) {
  const detail = group.value;
  if (!detail) return;
  const gid = detail.group.id;
  const myRole = detail.group.my_role;
  const isMe = sameId(member.id, state.me.id);
  const items = [];

  if (!isMe && myRole === 2) {
    items.push({
      label: member.role === 1 ? '取消管理员' : '设为管理员',
      icon: 'star',
      action: () => setAdmin(gid, member.id, member.role !== 1),
    });
  }
  if (!isMe && myRole >= 1 && member.role !== 2) {
    items.push({
      label: member.muted ? '解除禁言' : '禁言该成员',
      icon: member.muted ? 'bell' : 'bell-off',
      action: () => muteMember(gid, member.id, !member.muted),
    });
  }
  if (!isMe && myRole >= 1 && member.role !== 2 && (myRole === 2 || member.role === 0)) {
    items.push({ sep: true });
    items.push({
      label: '移出群聊',
      icon: 'logout',
      danger: true,
      action: () => removeMember(gid, member.id),
    });
  }
  if (!items.length) {
    items.push({ label: '查看资料', icon: 'info', action: () => openUserInfo(member.id) });
  }
  openMenu(event.currentTarget.getBoundingClientRect(), items);
}
</script>

<template>
  <aside class="info" aria-label="详情">
    <header class="info-head">
      <h3>详情</h3>
      <div class="spacer" />
      <button class="icon-btn" aria-label="关闭" @click="closeInfo"><Icon name="x" /></button>
    </header>

    <div class="info-body">
      <!-- 群聊 -->
      <template v-if="info && info.type === 'group'">
        <div v-if="!group" class="empty" style="padding-top:36px"><span>加载中…</span></div>
        <template v-else>
          <div class="info-profile">
            <Avatar :name="group.group.name" :src="group.group.avatar" :size="72" square />
            <h2>{{ group.group.name }}</h2>
            <div class="handle">
              {{ group.group.member_count }} 位成员<template v-if="owner"> · 群主 {{ owner.group_nickname || owner.nickname }}</template>
            </div>
          </div>

          <div class="info-section">
            <div class="info-label">群公告</div>
            <div class="info-text">{{ group.group.notice || '群主还没有填写公告。' }}</div>
          </div>

          <div class="info-section">
            <div class="info-label">群成员 <span class="count">{{ group.members.length }}</span></div>
            <div
              v-for="member in group.members"
              :key="member.id"
              class="member-row"
              role="button"
              tabindex="0"
              @click="memberMenu($event, member)"
              @keydown.enter.prevent="memberMenu($event, member)"
            >
              <Avatar :user="member" :size="32" />
              <span class="name">
                {{ member.group_nickname || member.nickname }}
                <span v-if="sameId(member.id, state.me.id)" class="me-tag"> · 我</span>
              </span>
              <span v-if="member.role === 2" class="tag owner">群主</span>
              <span v-else-if="member.role === 1" class="tag admin">管理</span>
              <span v-if="member.muted" class="tag">禁言</span>
            </div>
          </div>

          <div class="info-section info-foot">
            <button
              v-if="group.group.my_role >= 1"
              class="btn ghost sm block"
              @click="openDialog({ component: 'group-edit', title: '编辑群资料', props: { groupId: group.group.id } })"
            >
              <Icon name="edit" /> 编辑群资料
            </button>
            <button
              class="btn ghost sm block"
              @click="openDialog({ component: 'invite', title: '邀请好友入群', props: { groupId: group.group.id } })"
            >
              <Icon name="user-plus" /> 邀请好友入群
            </button>
            <button
              v-if="group.group.my_role === 2"
              class="btn danger sm block"
              @click="dissolveGroup(group.group.id)"
            >
              <Icon name="trash" /> 解散群聊
            </button>
            <button v-else class="btn danger sm block" @click="leaveGroup(group.group.id)">
              <Icon name="logout" /> 退出群聊
            </button>
          </div>
        </template>
      </template>

      <!-- 单聊资料 -->
      <template v-else-if="user">
        <div class="info-profile">
          <Avatar :user="user" :size="72" />
          <h2>{{ displayName }}</h2>
          <div class="handle">
            {{ isSelf ? '这是你' : isFriend ? (user.online ? '在线' : '离线') : '还不是好友' }}
          </div>
          <div class="sig">{{ user.signature || '这个人很神秘，什么都没留下' }}</div>
          <div class="info-actions">
            <button
              v-if="isSelf"
              class="btn primary sm"
              @click="openDialog({ component: 'edit-profile', title: '编辑资料' })"
            >编辑我的资料</button>
            <button
              v-else-if="isFriend"
              class="btn primary sm"
              @click="openFriendChat(user.id, displayName, user.avatar)"
            >发消息</button>
            <button
              v-else
              class="btn primary sm"
              @click="sendFriendRequest(user.id)"
            >加为好友</button>
          </div>
        </div>

        <div v-if="isFriend" class="info-section">
          <div
            class="member-row"
            role="button"
            tabindex="0"
            @click="openDialog({ component: 'remark', title: '设置备注', props: { friendId: user.id, name: user.remark || '' } })"
          >
            <Icon name="edit" />
            <span class="name">设置备注</span>
            <span class="muted">{{ user.remark || '未设置' }}</span>
          </div>
          <div
            class="member-row"
            role="button"
            tabindex="0"
            @click="toggleStar(user.id, !user.starred)"
          >
            <Icon name="star" />
            <span class="name">星标好友</span>
            <span class="muted">{{ user.starred ? '已星标' : '未星标' }}</span>
          </div>
          <div class="member-row" role="button" tabindex="0" @click="blockUser(user.id)">
            <Icon name="ban" />
            <span class="name">加入黑名单</span>
          </div>
          <div
            class="member-row"
            role="button"
            tabindex="0"
            @click="deleteFriend(user.id, displayName)"
          >
            <Icon name="trash" />
            <span class="name" style="color:var(--danger)">删除好友</span>
          </div>
        </div>
      </template>

      <div v-else class="empty" style="padding-top:36px"><span>加载中…</span></div>
    </div>
  </aside>
</template>
