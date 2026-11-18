<script setup>
import { computed, nextTick, ref } from 'vue';
import { assetUrl } from '../config';
import {
  addComment, deleteComment, deleteMoment, state, toggleLike,
} from '../store';
import { openDialog, openLightbox, openMenu } from '../ui';
import { fmtRelative, linkify, sameId } from '../utils';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';

const props = defineProps({
  moment: { type: Object, required: true },
});

const commentDraft = ref('');
const commentInput = ref(null);

const images = computed(() => props.moment.images || []);
const likes = computed(() => props.moment.likes || []);
const comments = computed(() => props.moment.comments || []);
const visibilityLabel = computed(() => {
  if (!props.moment.is_mine) return '';
  if (props.moment.visibility === 3) return ' · 仅自己可见';
  if (props.moment.visibility === 1) return ' · 公开';
  return '';
});

function canDeleteComment(comment) {
  return sameId(comment.user && comment.user.id, state.me.id) || props.moment.is_mine;
}

async function submitComment() {
  const content = commentDraft.value;
  if (!content.trim()) return;
  commentDraft.value = '';
  const ok = await addComment(props.moment.id, content);
  await nextTick();
  if (ok && commentInput.value) commentInput.value.focus();
}

function focusComment() {
  if (commentInput.value) {
    commentInput.value.focus();
    commentInput.value.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }
}

function menu(event) {
  const items = [];
  if (props.moment.is_mine) {
    items.push({ label: '删除动态', icon: 'trash', danger: true, action: () => deleteMoment(props.moment.id) });
  }
  if (props.moment.user && props.moment.user.id) {
    items.push({
      label: '查看 TA 的动态',
      icon: 'user',
      action: () => openDialog({
        component: 'user-moments',
        title: `${props.moment.user.nickname || 'TA'} 的动态`,
        wide: true,
        props: { userId: props.moment.user.id, nickname: props.moment.user.nickname },
      }),
    });
  }
  if (items.length) openMenu(event.currentTarget.getBoundingClientRect(), items);
}
</script>

<template>
  <article class="moment" :data-id="moment.id">
    <header class="moment-head">
      <Avatar :user="moment.user" :size="40" />
      <div class="moment-by">
        <b>{{ (moment.user && moment.user.nickname) || '' }}</b>
        <span>{{ fmtRelative(moment.created_at) }}{{ moment.is_mine ? ' · 我' : '' }}{{ visibilityLabel }}</span>
      </div>
      <div class="spacer" />
      <button class="icon-btn" title="更多" aria-label="更多" @click="menu">
        <Icon name="more" />
      </button>
    </header>

    <div v-if="moment.content" class="moment-text" v-html="linkify(moment.content)" />

    <div v-if="images.length" class="moment-imgs" :class="`n${Math.min(images.length, 9)}`">
      <img
        v-for="(url, i) in images"
        :key="`${moment.id}-${i}`"
        :src="assetUrl(url)"
        alt=""
        loading="lazy"
        @click="openLightbox(assetUrl(url))"
      />
    </div>

    <div class="moment-actions">
      <button class="link-btn" :class="{ 'is-liked': moment.liked }" @click="toggleLike(moment.id)">
        <Icon name="heart" /><span>{{ moment.like_count || 0 }}</span>
      </button>
      <button class="link-btn" @click="focusComment">
        <Icon name="message" /><span>{{ moment.comment_count || 0 }}</span>
      </button>
    </div>

    <div v-if="likes.length || comments.length" class="moment-social">
      <div v-if="likes.length" class="moment-likes">
        <Icon name="heart" />
        <div><b v-for="(user, i) in likes" :key="user.id">{{ user.nickname }}<span v-if="i < likes.length - 1">、</span></b></div>
      </div>
      <div v-if="comments.length" class="moment-comments">
        <div v-for="comment in comments" :key="comment.id" class="comment">
          <div class="c-body">
            <b>{{ (comment.user && comment.user.nickname) || '匿名' }}</b>
            <span v-if="comment.reply_to" class="c-reply"> 回复 </span>
            <b v-if="comment.reply_to">{{ comment.reply_to.nickname }}</b>：
            {{ comment.content }}
          </div>
          <button
            v-if="canDeleteComment(comment)"
            class="c-del"
            title="删除评论"
            @click="deleteComment(moment.id, comment.id)"
          >
            <Icon name="x" />
          </button>
        </div>
      </div>
    </div>

    <div class="comment-form">
      <input
        ref="commentInput"
        v-model="commentDraft"
        placeholder="写评论…"
        maxlength="200"
        @keydown.enter.prevent="submitComment"
      />
      <button class="btn sm" @click="submitComment">发送</button>
    </div>
  </article>
</template>
