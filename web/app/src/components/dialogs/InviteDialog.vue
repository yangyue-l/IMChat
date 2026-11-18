<script setup>
import { computed, ref } from 'vue';
import { inviteMembers, state } from '../../store';
import { closeDialog, toast } from '../../ui';
import Avatar from '../Avatar.vue';

const props = defineProps({
  groupId: { type: String, required: true },
});

const selected = ref([]);
const busy = ref(false);

const candidates = computed(() => {
  const detail = state.groupDetails[props.groupId];
  const existing = new Set((detail ? detail.members : []).map((m) => String(m.id)));
  return state.friends.filter((f) => !existing.has(String(f.id)));
});

function toggle(id) {
  const i = selected.value.indexOf(id);
  if (i >= 0) selected.value.splice(i, 1);
  else selected.value.push(id);
}

async function submit() {
  if (!selected.value.length) { toast('请先选择成员', 'error'); return; }
  if (busy.value) return;
  busy.value = true;
  const ok = await inviteMembers(props.groupId, selected.value);
  busy.value = false;
  if (ok) closeDialog();
}
</script>

<template>
  <div class="dlg-body">
    <div v-if="!candidates.length" class="empty" style="padding:24px">
      <span>没有可邀请的好友</span>
    </div>
    <div v-else class="pick-list">
      <label v-for="friend in candidates" :key="friend.id" class="pick-row">
        <input
          type="checkbox"
          :checked="selected.includes(String(friend.id))"
          @change="toggle(String(friend.id))"
        />
        <Avatar :user="friend" :size="28" />
        <span class="name">{{ friend.remark || friend.nickname }}</span>
      </label>
    </div>
  </div>

  <footer class="dlg-foot">
    <div class="spacer" />
    <button class="btn ghost" @click="closeDialog">取消</button>
    <button class="btn primary" :disabled="busy || !candidates.length" @click="submit">邀请</button>
  </footer>
</template>
