<script setup>
defineProps({
  name: { type: String, required: true },
  size: { type: String, default: '' },
});
</script>

<template>
  <svg class="ic" :class="size" aria-hidden="true"><use :href="`#i-${name}`" /></svg>
</template>
