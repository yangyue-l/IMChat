<script setup>
import { onMounted } from 'vue';
import { boot, state } from './store';
import AuthView from './views/AuthView.vue';
import AppShell from './components/AppShell.vue';
import IconSprite from './components/IconSprite.vue';
import DialogHost from './components/DialogHost.vue';
import MenuHost from './components/MenuHost.vue';
import ToastHost from './components/ToastHost.vue';
import LightboxHost from './components/LightboxHost.vue';

onMounted(boot);
</script>

<template>
  <IconSprite />
  <div v-if="state.booting" class="boot-splash"><div class="boot-mark">IM</div></div>
  <AuthView v-else-if="!state.me" />
  <AppShell v-else />
  <DialogHost />
  <MenuHost />
  <ToastHost />
  <LightboxHost />
</template>
