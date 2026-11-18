<script setup>
import { nextTick, onMounted, onUnmounted, ref, watch } from 'vue';
import { closeMenu, ui } from '../ui';
import Icon from './Icon.vue';

const el = ref(null);
const left = ref(0);
const top = ref(0);
const ready = ref(false);

watch(
  () => ui.menu,
  async (menu) => {
    if (!menu) { ready.value = false; return; }
    ready.value = false;
    await nextTick();
    const node = el.value;
    if (!node) return;
    const w = node.offsetWidth;
    const h = node.offsetHeight;
    const rect = menu.rect;
    const nextLeft = Math.min(Math.max(8, rect.right - w), window.innerWidth - w - 8);
    let nextTop = rect.bottom + 6;
    if (nextTop + h > window.innerHeight - 8) nextTop = Math.max(8, rect.top - h - 6);
    left.value = nextLeft;
    top.value = nextTop;
    ready.value = true;
  },
  { flush: 'post' },
);

function run(item) {
  closeMenu();
  if (item.action) item.action();
}

function onDocDown(e) {
  if (!ui.menu) return;
  if (el.value && el.value.contains(e.target)) return;
  closeMenu();
}

function onKey(e) {
  if (ui.menu && e.key === 'Escape') closeMenu();
}

function onScroll() {
  if (ui.menu) closeMenu();
}

onMounted(() => {
  document.addEventListener('mousedown', onDocDown, true);
  document.addEventListener('keydown', onKey, true);
  window.addEventListener('scroll', onScroll, true);
  window.addEventListener('resize', onScroll);
});

onUnmounted(() => {
  document.removeEventListener('mousedown', onDocDown, true);
  document.removeEventListener('keydown', onKey, true);
  window.removeEventListener('scroll', onScroll, true);
  window.removeEventListener('resize', onScroll);
});
</script>

<template>
  <div
    v-if="ui.menu"
    ref="el"
    class="menu"
    role="menu"
    :style="{ left: `${left}px`, top: `${top}px`, visibility: ready ? 'visible' : 'hidden' }"
  >
    <template v-for="(item, index) in ui.menu.items" :key="index">
      <div v-if="item.sep" class="menu-sep" />
      <button
        v-else
        class="menu-item"
        :class="{ danger: item.danger }"
        role="menuitem"
        @click="run(item)"
      >
        <Icon v-if="item.icon" :name="item.icon" />
        <span>{{ item.label }}</span>
      </button>
    </template>
  </div>
</template>
