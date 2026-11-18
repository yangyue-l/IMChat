<script setup>
import { computed } from 'vue';
import { openConv, state } from '../store';
import { fmtListTime } from '../utils';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';

const list = computed(() => {
  const q = state.filter.trim().toLowerCase();
  const sorted = state.convs.slice().sort((a, b) => {
    const pinned = (b.pinned ? 1 : 0) - (a.pinned ? 1 : 0);
    if (pinned) return pinned;
    return new Date(b.last_msg_at || 0) - new Date(a.last_msg_at || 0);
  });
  if (!q) return sorted;
  return sorted.filter((c) => `${c.name || ''} ${c.last_msg_preview || ''}`.toLowerCase().includes(q));
});

function preview(conv) {
  if (!conv.last_msg_preview && !conv.last_msg_type) return '还没有消息';
  if (conv.last_msg_type === 2) return '[图片]';
  if (conv.last_msg_type === 3) return '[文件]';
  return conv.last_msg_preview || '';
}

function open(conv) {
  openConv({
    conv_id: conv.conv_id,
    conv_type: conv.conv_type,
    target_id: String(conv.target_id),
    name: conv.name || '',
    avatar: conv.avatar || '',
    group_id: conv.conv_type === 2 ? String(conv.target_id) : '0',
    member_count: conv.member_count || 0,
  });
}
</script>

<template>
  <div v-if="!state.convsLoaded" aria-busy="true">
    <div v-for="i in 6" :key="i" class="conv" style="pointer-events:none">
      <div class="skeleton" style="width:44px;height:44px;border-radius:50%" />
      <div class="conv-main" style="gap:8px">
        <div class="skeleton" style="width:52%;height:11px" />
        <div class="skeleton" style="width:78%;height:10px" />
      </div>
    </div>
  </div>

  <div v-else-if="!list.length" class="empty">
    <Icon name="inbox" />
    <b>{{ state.filter ? '没有匹配的会话' : '还没有会话' }}</b>
    <span>{{ state.filter ? '换个关键词试试。' : '去通讯录找个人聊聊，或发起一个群聊。' }}</span>
  </div>

  <template v-else>
    <div
      v-for="conv in list"
      :key="conv.conv_id"
      class="conv"
      :class="{ 'is-active': state.current && state.current.conv_id === conv.conv_id }"
      role="button"
      tabindex="0"
      @click="open(conv)"
      @keydown.enter.prevent="open(conv)"
      @keydown.space.prevent="open(conv)"
    >
      <Avatar
        :name="conv.name"
        :src="conv.avatar"
        :size="44"
        :square="conv.conv_type === 2"
      />
      <div class="conv-main">
        <div class="conv-top">
          <span class="conv-name">{{ conv.name || '未命名会话' }}</span>
          <span v-if="conv.pinned" class="pin-flag" title="已置顶"><Icon name="pin" /></span>
          <span class="conv-time">{{ fmtListTime(conv.last_msg_at) }}</span>
        </div>
        <div class="conv-bottom">
          <span class="conv-preview">{{ preview(conv) }}</span>
          <span v-if="conv.muted" class="pin-flag" title="免打扰"><Icon name="bell-off" /></span>
          <span v-if="conv.unread" class="badge">{{ conv.unread > 99 ? '99+' : conv.unread }}</span>
        </div>
      </div>
    </div>
  </template>
</template>
