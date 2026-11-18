<script setup>
import { onMounted, ref } from 'vue';
import { assetUrl } from '../../config';
import { loadUserMoments } from '../../store';
import { closeDialog, openLightbox } from '../../ui';
import { fmtRelative, linkify } from '../../utils';
import Icon from '../Icon.vue';

const props = defineProps({
  userId: { type: String, required: true },
  nickname: { type: String, default: '' },
});

const loading = ref(true);
const error = ref('');
const list = ref([]);

onMounted(async () => {
  try {
    const data = await loadUserMoments(props.userId);
    list.value = Array.isArray(data) ? data : [];
  } catch (err) {
    error.value = err.message;
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <div class="dlg-body">
    <div v-if="loading" class="empty" style="padding:24px"><span>加载中…</span></div>
    <div v-else-if="error" class="empty" style="padding:24px"><span>{{ error }}</span></div>
    <div v-else-if="!list.length" class="empty" style="padding:28px">
      <Icon name="compass" />
      <span>TA 还没有发布动态</span>
    </div>
    <template v-else>
      <article
        v-for="moment in list"
        :key="moment.id"
        style="padding:14px 0;border-top:1px solid var(--line-soft)"
      >
        <div class="muted" style="margin-bottom:6px">{{ fmtRelative(moment.created_at) }}</div>
        <div class="moment-text" v-html="linkify(moment.content)" />
        <div
          v-if="moment.images && moment.images.length"
          class="moment-imgs"
          :class="`n${Math.min(moment.images.length, 9)}`"
          style="margin-top:10px"
        >
          <img
            v-for="(url, index) in moment.images"
            :key="index"
            :src="assetUrl(url)"
            alt=""
            loading="lazy"
            @click="openLightbox(assetUrl(url))"
          />
        </div>
      </article>
    </template>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">关闭</button>
  </footer>
</template>
