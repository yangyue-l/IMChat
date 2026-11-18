<script setup>
import { computed } from 'vue';
import { assetUrl } from '../config';
import { recallMessage, state } from '../store';
import { copyText, openLightbox, openMenu } from '../ui';
import { clock, imageUrlOf, linkify, previewOf, sameId, toDate } from '../utils';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';

const props = defineProps({
  row: { type: Object, required: true },
  conv: { type: Object, required: true },
});
const emit = defineEmits(['quote']);

const m = computed(() => props.row.message);
const date = computed(() => toDate(m.value.created_at) || new Date());
const clockOf = computed(() => clock(date.value));
const pending = computed(() => m.value.status === -1);
const failed = computed(() => m.value.failed || m.value.status === -2);
const textHtml = computed(() => linkify(m.value.content));
const imageUrl = computed(() => assetUrl(imageUrlOf(m.value)));

const quoteName = computed(() => {
  const q = m.value.quote;
  if (!q) return '';
  if (sameId(q.from_id, state.me.id)) return '我';
  if (m.value.from_user && sameId(q.from_id, m.value.from_id)) return m.value.from_user.nickname;
  if (props.conv.conv_type === 1) return props.conv.name || '对方';
  const detail = state.groupDetails[props.conv.group_id];
  const member = detail && (detail.members || []).find((x) => sameId(x.id, q.from_id));
  return (member && (member.group_nickname || member.nickname)) || '对方';
});

const canRecall = computed(() => {
  if (m.value.status === 1) return false;
  if (props.row.mine) {
    return Date.now() - date.value.getTime() < 2 * 60 * 1000;
  }
  if (props.conv.conv_type === 2) {
    const detail = state.groupDetails[props.conv.group_id];
    return !!(detail && detail.group.my_role >= 1);
  }
  return false;
});

function showMenu(event) {
  const items = [
    { label: '引用', icon: 'quote', action: () => emit('quote', m.value) },
    {
      label: '复制',
      icon: 'copy',
      action: () => copyText(m.value.msg_type === 1 ? m.value.content : previewOf(m.value)),
    },
  ];
  if (canRecall.value) {
    items.push({ sep: true });
    items.push({ label: '撤回', icon: 'trash', danger: true, action: () => recallMessage(m.value) });
  }
  openMenu(event.currentTarget.getBoundingClientRect(), items);
}
</script>

<template>
  <div class="msg" :class="{ mine: row.mine, first: row.first }" :data-id="m.id">
    <span
      v-if="!row.mine"
      :style="{ visibility: row.first ? 'visible' : 'hidden' }"
    >
      <Avatar :user="m.from_user" :size="32" />
    </span>

    <div class="msg-body">
      <div v-if="row.showWho" class="msg-who">
        <b>{{ (m.from_user && m.from_user.nickname) || '' }}</b>
        <span>{{ clockOf }}</span>
      </div>

      <div class="bubble-wrap">
        <div class="bubble" :class="{ 'is-pending': pending, 'is-failed': failed }">
          <div v-if="m.quote" class="quote-block">
            <Icon name="corner" />
            <div style="min-width:0">
              <b>{{ quoteName }}</b>
              <div class="q-text">{{ m.quote.preview || '' }}</div>
            </div>
          </div>

          <img
            v-if="m.msg_type === 2"
            class="msg-img"
            :src="imageUrl"
            alt="图片消息"
            loading="lazy"
            @click="openLightbox(imageUrl)"
          />
          <div v-else-if="m.msg_type === 3" class="msg-file">
            <span class="file-ic"><Icon name="file" /></span>
            <span class="file-name">{{ m.content }}</span>
          </div>
          <span v-else v-html="textHtml" />
        </div>

        <button
          v-if="!pending && !failed"
          class="msg-more"
          title="更多操作"
          aria-label="更多操作"
          @click.stop="showMenu"
        >
          <Icon name="more" />
        </button>
      </div>

      <div
        v-if="row.mine"
        class="msg-meta"
        :class="{ 'is-flow': pending || failed }"
        :style="pending || failed ? null : { opacity: 0 }"
      >
        {{ clockOf }}<span v-if="pending"> · 发送中</span>
        <span v-if="failed" class="fail"> · 发送失败</span>
      </div>
    </div>
  </div>
</template>
