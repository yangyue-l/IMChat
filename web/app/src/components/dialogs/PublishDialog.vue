<script setup>
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { publishMoment } from '../../store';
import { closeDialog, toast } from '../../ui';
import Icon from '../Icon.vue';

const text = ref('');
const files = ref([]);
const visibility = ref(2);
const busy = ref(false);
const fileEl = ref(null);
const textEl = ref(null);

const options = [
  { value: 1, label: '公开' },
  { value: 2, label: '仅好友' },
  { value: 3, label: '仅自己' },
];

onMounted(() => textEl.value && textEl.value.focus());
onBeforeUnmount(() => files.value.forEach((item) => URL.revokeObjectURL(item.url)));

function addFiles(event) {
  const list = Array.from(event.target.files || []);
  event.target.value = '';
  for (const file of list) {
    if (files.value.length >= 9) break;
    if (!file.type.startsWith('image/')) continue;
    files.value.push({ file, url: URL.createObjectURL(file) });
  }
}

function remove(index) {
  URL.revokeObjectURL(files.value[index].url);
  files.value.splice(index, 1);
}

async function submit() {
  if (!text.value.trim() && !files.value.length) {
    toast('写点什么或选张图片', 'error');
    return;
  }
  if (busy.value) return;
  busy.value = true;
  try {
    await publishMoment({
      content: text.value.trim(),
      files: files.value.map((item) => item.file),
      visibility: visibility.value,
    });
    closeDialog();
  } catch (err) {
    toast(err.message, 'error');
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="dlg-body">
    <label class="field">
      <span>内容</span>
      <textarea ref="textEl" v-model="text" rows="5" maxlength="2000" placeholder="分享你的此刻…" />
    </label>

    <div class="field">
      <span>图片（最多 9 张）</span>
      <div class="upload-grid">
        <div v-for="(item, index) in files" :key="item.url" class="thumb">
          <img :src="item.url" alt="" />
          <button type="button" aria-label="移除" @click="remove(index)"><Icon name="x" /></button>
        </div>
        <button
          v-if="files.length < 9"
          type="button"
          class="upload-add"
          @click="fileEl.click()"
        >
          <Icon name="plus" />
          <span style="font-size:11px">添加</span>
        </button>
        <input ref="fileEl" type="file" accept="image/*" multiple hidden @change="addFiles" />
      </div>
    </div>

    <div class="field">
      <span>可见范围</span>
      <div class="seg">
        <button
          v-for="option in options"
          :key="option.value"
          type="button"
          :class="{ 'is-on': visibility === option.value }"
          @click="visibility = option.value"
        >{{ option.label }}</button>
      </div>
    </div>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">取消</button>
    <button class="btn primary" :disabled="busy" @click="submit">{{ busy ? '发布中…' : '发布' }}</button>
  </footer>
</template>
