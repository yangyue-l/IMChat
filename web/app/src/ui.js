import { reactive } from 'vue';

/**
 * 纯 UI 状态：提示、弹窗、右键菜单、灯箱。
 * 与业务状态（store）分开，方便组件按需订阅。
 */
export const ui = reactive({
  toasts: [],
  dialog: null, // { component, title, props, wide }
  menu: null, // { rect, items }
  lightbox: '',
  focusMessage: '', // 搜索结果定位到某条消息
  mobileChat: false, // 窄屏下是否显示聊天主区域
});

let toastSeq = 0;

export function toast(message, kind = 'ok') {
  const id = ++toastSeq;
  ui.toasts.push({ id, message, kind });
  while (ui.toasts.length > 4) ui.toasts.shift();
  setTimeout(() => {
    const i = ui.toasts.findIndex((t) => t.id === id);
    if (i >= 0) ui.toasts.splice(i, 1);
  }, 2600);
}

export function openDialog(dialog) {
  ui.dialog = dialog;
}

export function closeDialog() {
  ui.dialog = null;
}

export function openMenu(rect, items) {
  if (!items || !items.length) return;
  ui.menu = { rect, items };
}

export function closeMenu() {
  ui.menu = null;
}

export function openLightbox(src) {
  ui.lightbox = src;
}

export function closeLightbox() {
  ui.lightbox = '';
}

export function confirmDialog({ title, text, ok = '确定', danger = true }) {
  return new Promise((resolve) => {
    openDialog({
      component: 'confirm',
      title,
      props: {
        text,
        ok,
        danger,
        resolve: (value) => {
          closeDialog();
          resolve(value);
        },
      },
    });
  });
}

export async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
    }
    toast('已复制');
  } catch (_) {
    toast('复制失败，请手动选择文本', 'error');
  }
}
