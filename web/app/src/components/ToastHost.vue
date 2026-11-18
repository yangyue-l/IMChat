<script setup>
import { ui } from '../ui';
import Icon from './Icon.vue';
</script>

<template>
  <div class="toasts" role="status" aria-live="polite">
    <div v-for="item in ui.toasts" :key="item.id" class="toast">
      <Icon :name="item.kind === 'error' ? 'info' : 'check'" />
      <span>{{ item.message }}</span>
    </div>
  </div>
</template>
