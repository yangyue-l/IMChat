<script setup>
import { nextTick, ref, watch } from 'vue';
import { closeLightbox, ui } from '../ui';
import Icon from './Icon.vue';

const el = ref(null);

watch(
  () => ui.lightbox,
  async (src) => {
    await nextTick();
    const node = el.value;
    if (!node) return;
    if (src && !node.open) node.showModal();
    if (!src && node.open) node.close();
  },
  { flush: 'post' },
);

function onClose() {
  if (ui.lightbox) closeLightbox();
}
</script>

<template>
  <dialog ref="el" class="lightbox" @close="onClose" @click.self="closeLightbox">
    <div class="lightbox-inner" @click.self="closeLightbox">
      <button class="close" aria-label="关闭" @click="closeLightbox"><Icon name="x" /></button>
      <img v-if="ui.lightbox" :src="ui.lightbox" alt="图片预览" />
    </div>
  </dialog>
</template>
