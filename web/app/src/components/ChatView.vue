<script setup>
import { computed, nextTick, ref, watch } from 'vue';
import {
  loadMore, markRead, notifyTyping, openInfo, sendImage, sendText, setConvFlag, state,
} from '../store';
import { openDialog, openMenu, toast, ui } from '../ui';
import { dayKey, fmtDay, previewOf, sameId, toDate } from '../utils';
import Avatar from './Avatar.vue';
import Icon from './Icon.vue';
import MessageRow from './MessageRow.vue';

const threadEl = ref(null);
const inputEl = ref(null);
const fileEl = ref(null);
const draft = ref('');
const stick = ref(true);
const loadingOlder = ref(false);

const conv = computed(() => state.current);
const list = computed(() => (conv.value ? state.messages[conv.value.conv_id] || [] : []));
const canLoadMore = computed(
  () => !!conv.value && list.value.length > 0 && !state.historyEnded[conv.value.conv_id],
);
const rows = computed(() => buildRows(list.value, conv.value));

const typing = computed(() => (conv.value ? state.typing[conv.value.conv_id] : null));
const online = computed(() => {
  const c = conv.value;
  if (!c || c.conv_type === 2) return false;
  const friend = state.friends.find((f) => sameId(f.id, c.target_id));
  return !!(friend && friend.online);
});
const meta = computed(() => {
  const c = conv.value;
  if (!c) return '';
  const t = state.typing[c.conv_id];
  if (t && t.until > Date.now()) return `${t.name} 正在输入…`;
  if (c.conv_type === 2) {
    const detail = state.groupDetails[c.group_id];
    return `${detail ? detail.group.member_count : c.member_count || 0} 位成员`;
  }
  return online.value ? '在线' : '离线';
});

const quoteName = computed(() => {
  const q = state.quote;
  if (!q) return '';
  return sameId(q.from_id, state.me.id)
    ? '我'
    : (q.from_user && q.from_user.nickname) || (conv.value && conv.value.name) || '对方';
});

function buildRows(messages, c) {
  if (!c) return [];
  const out = [];
  let lastDay = '';
  let prevFrom = null;

  for (const m of messages) {
    const d = toDate(m.created_at) || new Date();
    const key = dayKey(d);
    if (key !== lastDay) {
      out.push({ type: 'day', id: `day-${key}`, label: fmtDay(d) });
      lastDay = key;
      prevFrom = null;
    }

    if (m.msg_type === 4) {
      out.push({ type: 'sys', id: `sys-${m.id}`, text: m.content });
      prevFrom = null;
      continue;
    }
    if (m.status === 1) {
      const who = m.from_user && m.from_user.nickname ? `${m.from_user.nickname} ` : '';
      out.push({ type: 'sys', id: `recall-${m.id}`, text: `${who}撤回了一条消息` });
      prevFrom = null;
      continue;
    }

    const mine = sameId(m.from_id, state.me.id);
    const first = !sameId(prevFrom, m.from_id);
    prevFrom = m.from_id;
    out.push({
      type: 'msg',
      id: `msg-${m.id}`,
      message: m,
      mine,
      first,
      showWho: c.conv_type === 2 && !mine && first,
    });
  }
  return out;
}

/* ------------------------------ 滚动 ------------------------------ */
function nearBottom() {
  const el = threadEl.value;
  if (!el) return true;
  return el.scrollHeight - el.scrollTop - el.clientHeight < 140;
}

function scrollBottom(smooth = false) {
  const el = threadEl.value;
  if (!el) return;
  el.scrollTo({ top: el.scrollHeight, behavior: smooth ? 'smooth' : 'auto' });
}

function onScroll() {
  stick.value = nearBottom();
  if (threadEl.value && threadEl.value.scrollTop < 40) loadOlder();
}

async function loadOlder() {
  if (loadingOlder.value || !canLoadMore.value) return;
  loadingOlder.value = true;
  const el = threadEl.value;
  const prevHeight = el.scrollHeight;
  const prevTop = el.scrollTop;
  try {
    await loadMore();
    await nextTick();
    el.scrollTop = el.scrollHeight - prevHeight + prevTop;
  } finally {
    loadingOlder.value = false;
  }
}

watch(
  () => conv.value && conv.value.conv_id,
  async () => {
    stick.value = true;
    draft.value = '';
    await nextTick();
    scrollBottom();
    markRead();
    if (window.matchMedia('(pointer: fine)').matches && inputEl.value) inputEl.value.focus();
  },
  { immediate: true },
);

watch(
  () => list.value.length,
  async () => {
    await nextTick();
    if (stick.value) scrollBottom();
    markRead();
  },
);

watch(
  () => ui.focusMessage,
  async (id) => {
    if (!id) return;
    ui.focusMessage = '';
    await nextTick();
    const node = threadEl.value && threadEl.value.querySelector(`.msg[data-id="${CSS.escape(String(id))}"]`);
    if (!node) { toast('这条消息不在当前已加载的记录里'); return; }
    node.scrollIntoView({ block: 'center', behavior: 'smooth' });
    node.classList.add('flash');
    setTimeout(() => node.classList.remove('flash'), 1400);
  },
);

/* ------------------------------ 输入 ------------------------------ */
function autosize() {
  const el = inputEl.value;
  if (!el) return;
  el.style.height = 'auto';
  el.style.height = `${Math.min(el.scrollHeight, 160)}px`;
}

function onInput() {
  autosize();
  notifyTyping();
}

async function submit() {
  const text = draft.value;
  if (!text.trim()) return;
  draft.value = '';
  stick.value = true;
  await nextTick();
  autosize();
  await sendText(text);
}

function onKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    submit();
  }
}

async function onFile(e) {
  const file = e.target.files && e.target.files[0];
  e.target.value = '';
  if (!file) return;
  stick.value = true;
  await sendImage(file);
}

function onPaste(e) {
  const item = Array.from((e.clipboardData && e.clipboardData.items) || [])
    .find((x) => x.type.startsWith('image/'));
  if (!item) return;
  const file = item.getAsFile();
  if (!file) return;
  e.preventDefault();
  sendImage(file);
}

function clearQuote() {
  state.quote = null;
  if (inputEl.value) inputEl.value.focus();
}

function quoteMessage(message) {
  state.quote = message;
  if (inputEl.value) inputEl.value.focus();
}

/* ------------------------------ 头部 ------------------------------ */
function convMenu(event) {
  const c = conv.value;
  if (!c) return;
  const known = state.convs.find((x) => x.conv_id === c.conv_id) || c;
  const items = [
    {
      label: known.pinned ? '取消置顶' : '置顶会话',
      icon: 'pin',
      action: () => setConvFlag(c.conv_id, { pinned: !known.pinned }),
    },
    {
      label: known.muted ? '关闭免打扰' : '开启免打扰',
      icon: known.muted ? 'bell' : 'bell-off',
      action: () => setConvFlag(c.conv_id, { muted: !known.muted }),
    },
    { sep: true },
    { label: '查看详情', icon: 'info', action: openInfo },
  ];
  openMenu(event.currentTarget.getBoundingClientRect(), items);
}
function back() {
  ui.mobileChat = false;
}
</script>

<template>
  <div v-if="conv" class="chat-inner">
    <header class="chat-head">
      <button class="icon-btn only-mobile" title="返回" aria-label="返回" @click="back">
        <Icon name="arrow-left" />
      </button>
      <button class="chat-head-id" title="查看详情" @click="openInfo">
        <Avatar :name="conv.name" :src="conv.avatar" :size="36" :square="conv.conv_type === 2" />
        <span style="min-width:0;display:block">
          <h3>{{ conv.name || '会话' }}</h3>
          <div class="chat-head-meta">
            <span v-if="online" class="status-dot on" />
            {{ meta }}
          </div>
        </span>
      </button>
      <div class="spacer" />
      <button class="icon-btn" title="搜索聊天记录" aria-label="搜索聊天记录"
              @click="openDialog({ component: 'search-message', title: '搜索聊天记录' })">
        <Icon name="search" />
      </button>
      <button class="icon-btn" title="更多" aria-label="更多" @click="convMenu">
        <Icon name="more" />
      </button>
    </header>

    <div ref="threadEl" class="thread" @scroll.passive="onScroll">
      <div class="thread-inner">
        <div v-if="canLoadMore" class="thread-loadmore">
          <button class="btn sm ghost" :disabled="loadingOlder" @click="loadOlder">
            {{ loadingOlder ? '加载中…' : '加载更早的消息' }}
          </button>
        </div>

        <div v-if="!list.length" class="empty" style="padding-top:64px">
          <Icon name="chat" />
          <b>还没有消息</b>
          <span>发个招呼，开始这段对话。</span>
        </div>

        <template v-for="row in rows" :key="row.id">
          <div v-if="row.type === 'day'" class="daysep"><span>{{ row.label }}</span></div>
          <div v-else-if="row.type === 'sys'" class="sys"><span>{{ row.text }}</span></div>
          <MessageRow v-else :row="row" :conv="conv" @quote="quoteMessage" />
        </template>
      </div>
    </div>

    <div class="composer">
      <div class="composer-inner">
        <div class="composer-box">
          <div v-if="state.quote" class="composer-quote">
            <Icon name="quote" />
            <div class="q-body">
              <b>回复 {{ quoteName }}</b>
              <div class="q-text">{{ previewOf(state.quote) }}</div>
            </div>
            <button class="icon-btn" aria-label="取消引用" @click="clearQuote"><Icon name="x" /></button>
          </div>

          <div class="composer-row">
            <div class="composer-tools">
              <button class="icon-btn" title="发送图片" aria-label="发送图片" @click="fileEl.click()">
                <Icon name="image" />
              </button>
            </div>
            <textarea
              ref="inputEl"
              v-model="draft"
              rows="1"
              maxlength="2000"
              placeholder="输入消息…"
              aria-label="消息内容"
              @input="onInput"
              @keydown="onKeydown"
              @paste="onPaste"
            />
            <button class="send-btn" title="发送" aria-label="发送" :disabled="!draft.trim()" @click="submit">
              <Icon name="send" />
            </button>
          </div>
        </div>
        <div class="composer-hint">
          <span><kbd>Enter</kbd> 发送 · <kbd>Shift</kbd> + <kbd>Enter</kbd> 换行</span>
          <span>{{ draft.length > 1600 ? `${draft.length} / 2000` : '' }}</span>
        </div>
      </div>
    </div>

    <input ref="fileEl" type="file" accept="image/*" hidden @change="onFile" />
  </div>
</template>
