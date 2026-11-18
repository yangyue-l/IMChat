<script setup>
import { computed } from 'vue';
import { loadTimeline, state, switchTab } from '../store';
import { openDialog } from '../ui';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';
import MomentItem from './MomentItem.vue';

const subtitle = computed(() => (
  state.moments.length ? `最近 ${state.moments.length} 条动态` : '好友的动态会出现在这里'
));

function publish() {
  openDialog({ component: 'publish', title: '发表动态', wide: true });
}

function back() {
  switchTab('convs');
}
</script>

<template>
  <div class="moments">
    <div class="feed">
      <div class="feed-head">
        <button class="icon-btn only-mobile" aria-label="返回" @click="back">
          <Icon name="arrow-left" />
        </button>
        <div>
          <h2>朋友圈</h2>
          <div class="feed-sub">{{ subtitle }}</div>
        </div>
        <div class="spacer" />
        <button class="btn primary sm" @click="publish">
          <Icon name="edit" /> 发表
        </button>
      </div>

      <button class="feed-publish" @click="publish">
        <Avatar :user="state.me" :size="32" />
        <span>分享你的此刻…</span>
        <div class="spacer" />
        <Icon name="image" />
      </button>

      <div v-if="!state.momentsLoaded" class="empty" style="padding-top:60px">
        <span>加载中…</span>
      </div>

      <div v-else-if="!state.moments.length" class="empty" style="padding-top:40px">
        <Icon name="compass" />
        <b>朋友圈还很安静</b>
        <span>发布第一条动态，让朋友看到你的此刻。</span>
      </div>

      <MomentItem v-for="moment in state.moments" :key="moment.id" :moment="moment" />

      <div v-if="state.moments.length && !state.momentsEnded" style="display:flex;justify-content:center;padding:22px 0">
        <button class="btn ghost sm" @click="loadTimeline(true)">加载更多</button>
      </div>
    </div>
  </div>
</template>
