<script setup>
defineProps({
  text: { type: String, default: '' },
  ok: { type: String, default: '确定' },
  danger: { type: Boolean, default: true },
  resolve: { type: Function, required: true },
});
</script>

<template>
  <div class="dlg-body">
    <p class="dlg-note" style="margin-bottom:6px">{{ text }}</p>
  </div>
  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="resolve(false)">取消</button>
    <button class="btn" :class="danger ? 'danger' : 'primary'" @click="resolve(true)">{{ ok }}</button>
  </footer>
</template>
