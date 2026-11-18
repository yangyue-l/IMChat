<script setup>
import { onMounted, ref } from 'vue';
import { searchMessages, state } from '../../store';
import { closeDialog, ui } from '../../ui';
import { escapeHtml, fmtListTime } from '../../utils';
import Avatar from '../Avatar.vue';
import Icon from '../Icon.vue';

const keyword = ref('');
const results = ref([]);
const busy = ref(false);
const error = ref('');
const inputEl = ref(null);

onMounted(() => inputEl.value && inputEl.value.focus());

function highlight(content) {
  const raw = String(content || '');
  const kw = keyword.value.trim();
  if (!kw) return escapeHtml(raw);
  const safe = kw.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return escapeHtml(raw).replace(
    new RegExp(safe, 'gi'),
    (match) => `<mark style="background:var(--primary-soft-2);color:inherit;border-radius:3px;padding:0 1px">${match}</mark>`,
  );
}

async function search() {
  const kw = keyword.value.trim();
  if (kw.length < 2) { error.value = '至少输入 2 个字'; return; }
  if (!state.current || busy.value) return;
  busy.value = true;
  error.value = '';
  try {
    const list = await searchMessages(state.current.conv_id, kw);
    results.value = Array.isArray(list) ? list : [];
  } catch (err) {
    error.value = err.message;
  } finally {
    busy.value = false;
  }
}

function jump(message) {
  closeDialog();
  ui.focusMessage = String(message.id);
}
</script>

<template>
  <div class="dlg-body">
    <div class="search">
      <Icon name="search" />
      <input
        ref="inputEl"
        v-model="keyword"
        type="search"
        placeholder="输入关键词，2-32 个字"
        @keydown.enter.prevent="search"
      />
    </div>

    <div style="margin-top:12px;max-height:340px;overflow-y:auto">
      <div v-if="busy" class="empty" style="padding:20px"><span>搜索中…</span></div>
      <div v-else-if="error" class="empty" style="padding:20px"><span>{{ error }}</span></div>
      <div v-else-if="!results.length" class="empty" style="padding:20px">
        <span>{{ keyword ? '没有找到相关消息' : '搜索这个会话里的聊天记录' }}</span>
      </div>
      <template v-else>
        <div
          v-for="message in results"
          :key="message.id"
          class="row"
          role="button"
          tabindex="0"
          @click="jump(message)"
        >
          <Avatar :user="message.from_user" :size="32" />
          <div class="row-main">
            <div class="row-name">
              {{ (message.from_user && message.from_user.nickname) || '' }}
              <span class="muted" style="font-weight:400;margin-left:6px">{{ fmtListTime(message.created_at) }}</span>
            </div>
            <div class="row-sub" v-html="message.msg_type === 2 ? '[图片]' : highlight(message.content)" />
          </div>
        </div>
      </template>
    </div>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">关闭</button>
    <button class="btn primary" :disabled="busy" @click="search">搜索</button>
  </footer>
</template>
