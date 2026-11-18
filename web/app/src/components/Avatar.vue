<script setup>
import { computed } from 'vue';
import { assetUrl } from '../config';
import { hueOf, initialOf } from '../utils';

const props = defineProps({
  user: { type: Object, default: null },
  name: { type: String, default: '' },
  src: { type: String, default: '' },
  size: { type: Number, default: 40 },
  square: { type: Boolean, default: false },
});

const displayName = computed(
  () => props.name || (props.user && (props.user.nickname || props.user.name)) || '?',
);
const url = computed(() => assetUrl(props.src || (props.user && props.user.avatar) || ''));
const style = computed(() => (url.value ? undefined : { '--av-bg': `oklch(0.52 0.085 ${hueOf(displayName.value)})` }));
</script>

<template>
  <span class="av" :class="[`s${size}`, { sq: square }]" :style="style" aria-hidden="true">
    <img v-if="url" :src="url" alt="" loading="lazy" />
    <template v-else>{{ initialOf(displayName) }}</template>
  </span>
</template>
