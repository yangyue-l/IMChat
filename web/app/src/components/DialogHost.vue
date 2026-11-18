<script setup>
import { computed, nextTick, ref, watch } from 'vue';
import { closeDialog, ui } from '../ui';
import Icon from './Icon.vue';
import ConfirmDialog from './dialogs/ConfirmDialog.vue';
import AddFriendDialog from './dialogs/AddFriendDialog.vue';
import CreateGroupDialog from './dialogs/CreateGroupDialog.vue';
import BlacklistDialog from './dialogs/BlacklistDialog.vue';
import EditProfileDialog from './dialogs/EditProfileDialog.vue';
import GroupEditDialog from './dialogs/GroupEditDialog.vue';
import InviteDialog from './dialogs/InviteDialog.vue';
import PublishDialog from './dialogs/PublishDialog.vue';
import SearchMessageDialog from './dialogs/SearchMessageDialog.vue';
import RemarkDialog from './dialogs/RemarkDialog.vue';
import UserMomentsDialog from './dialogs/UserMomentsDialog.vue';

const registry = {
  confirm: ConfirmDialog,
  'add-friend': AddFriendDialog,
  'create-group': CreateGroupDialog,
  blacklist: BlacklistDialog,
  'edit-profile': EditProfileDialog,
  'group-edit': GroupEditDialog,
  invite: InviteDialog,
  publish: PublishDialog,
  'search-message': SearchMessageDialog,
  remark: RemarkDialog,
  'user-moments': UserMomentsDialog,
};

const el = ref(null);
const component = computed(() => (ui.dialog ? registry[ui.dialog.component] || null : null));

watch(
  () => ui.dialog,
  async (dialog) => {
    await nextTick();
    const node = el.value;
    if (!node) return;
    if (dialog && !node.open) node.showModal();
    if (!dialog && node.open) node.close();
  },
  { flush: 'post' },
);

function onClose() {
  if (ui.dialog) closeDialog();
}

function onBackdrop(event) {
  if (event.target === el.value) closeDialog();
}
</script>

<template>
  <dialog ref="el" :class="{ wide: ui.dialog && ui.dialog.wide }" @close="onClose" @click="onBackdrop">
    <div v-if="ui.dialog" class="dlg">
      <header class="dlg-head">
        <h3>{{ ui.dialog.title }}</h3>
        <button class="icon-btn" aria-label="关闭" @click="closeDialog"><Icon name="x" /></button>
      </header>
      <component :is="component" v-bind="ui.dialog.props || {}" />
    </div>
  </dialog>
</template>
