import { computed, reactive, watchEffect } from 'vue';
import { api, auth, upload } from '../api';
import { wsUrl } from '../config';
import { closeDialog, closeMenu, confirmDialog, toast, ui } from '../ui';
import { cmpId, previewOf, rid, sameId } from '../utils';

const TOKEN_KEY = 'imchat_token';
const THEME_KEY = 'imchat_theme';

export const state = reactive({
  booting: true,
  token: localStorage.getItem(TOKEN_KEY) || '',
  theme: localStorage.getItem(THEME_KEY) || 'light',
  me: null,
  wsOpen: false,

  tab: 'convs',
  filter: '',

  convs: [],
  convsLoaded: false,
  friends: [],
  groups: [],
  requests: [],
  blocked: [],

  current: null,
  messages: {},
  historyEnded: {},
  loadingMore: false,
  groupDetails: {},
  typing: {},

  quote: null,
  info: null,

  moments: [],
  momentsLoaded: false,
  momentsEnded: false,

  pendingAvatar: '',
});

export const unreadTotal = computed(() => state.convs.reduce((n, c) => n + (c.unread || 0), 0));
export const pendingCount = computed(() => state.requests.filter((r) => r.status === 0).length);

watchEffect(() => {
  document.title = unreadTotal.value ? `(${unreadTotal.value}) IMChat` : 'IMChat · 即时通讯';
});

const isNarrow = () => window.matchMedia('(max-width: 900px)').matches;

/* ------------------------------- 主题 ------------------------------- */
export function applyTheme(theme) {
  state.theme = theme;
  document.documentElement.dataset.theme = theme;
  localStorage.setItem(THEME_KEY, theme);
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', theme === 'dark' ? '#111719' : '#f6f8f7');
}

export function toggleTheme() {
  applyTheme(state.theme === 'dark' ? 'light' : 'dark');
}

/* ------------------------------- 启动 ------------------------------- */
export async function boot() {
  applyTheme(state.theme);
  auth.token = state.token;
  auth.onUnauthorized = () => forceLogout('登录状态已失效，请重新登录');

  if (state.token) {
    try {
      state.me = await api('GET', '/users/me');
      await enterApp();
    } catch (_) {
      clearSession();
    }
  }
  state.booting = false;
}

async function enterApp() {
  connectWS();
  await Promise.all([loadConvs(), loadFriends(), loadGroups(), loadRequests()]);
}

/* ------------------------------- 登录 ------------------------------- */
export async function submitAuth(mode, payload) {
  const data = await api('POST', mode === 'register' ? '/auth/register' : '/auth/login', payload);
  state.token = data.token;
  state.me = data.user;
  auth.token = data.token;
  localStorage.setItem(TOKEN_KEY, data.token);
  await enterApp();
}

export async function logout() {
  const ok = await confirmDialog({
    title: '退出登录',
    text: '退出后需要重新输入账号密码。确定要退出吗？',
    ok: '退出',
  });
  if (!ok) return;
  api('POST', '/auth/logout').catch(() => {});
  clearSession();
}

function forceLogout(message) {
  if (message) toast(message, 'error');
  clearSession();
}

function clearSession() {
  if (ws) {
    ws.onclose = null;
    try { ws.close(); } catch (_) { /* 已断开 */ }
    ws = null;
  }
  clearInterval(hbTimer);
  outbox = [];
  retry = 0;

  state.token = '';
  state.me = null;
  state.wsOpen = false;
  auth.token = '';
  localStorage.removeItem(TOKEN_KEY);

  state.convs = [];
  state.convsLoaded = false;
  state.friends = [];
  state.groups = [];
  state.requests = [];
  state.blocked = [];
  state.current = null;
  state.messages = {};
  state.historyEnded = {};
  state.groupDetails = {};
  state.typing = {};
  state.moments = [];
  state.momentsLoaded = false;
  state.momentsEnded = false;
  state.quote = null;
  state.info = null;
  state.pendingAvatar = '';
  state.tab = 'convs';
  state.filter = '';
  ui.mobileChat = false;

  closeMenu();
  closeDialog();
}

/* ----------------------------- WebSocket ----------------------------- */
let ws = null;
let hbTimer = null;
let retry = 0;
let outbox = [];

function connectWS() {
  if (!state.token) return;
  let socket;
  try {
    socket = new WebSocket(wsUrl(`/ws?token=${encodeURIComponent(state.token)}&device=web`));
  } catch (_) {
    return;
  }
  ws = socket;

  socket.onopen = () => {
    state.wsOpen = true;
    retry = 0;
    while (outbox.length) socket.send(outbox.shift());
    clearInterval(hbTimer);
    hbTimer = setInterval(() => {
      if (socket.readyState === WebSocket.OPEN) socket.send('{"type":"ping"}');
    }, 25000);
  };

  socket.onclose = (ev) => {
    state.wsOpen = false;
    clearInterval(hbTimer);
    if (!state.token) return;
    if (ev.code === 4001) {
      forceLogout('账号已在其他设备登录');
      return;
    }
    retry += 1;
    const delay = Math.min(1200 * 1.7 ** Math.min(retry, 5), 15000);
    setTimeout(() => { if (state.token) connectWS(); }, delay);
  };

  socket.onerror = () => {};

  socket.onmessage = (ev) => {
    let frame;
    try { frame = JSON.parse(ev.data); } catch (_) { return; }
    handleFrame(frame);
  };
}

function wsSend(payload) {
  const raw = JSON.stringify(payload);
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(raw);
  else outbox.push(raw);
}

function handleFrame(frame) {
  const { type, data } = frame || {};
  switch (type) {
    case 'message':
      onIncoming(data);
      break;
    case 'ack':
      if (data && data.conv_id) replaceTemp(data.conv_id, data.client_msg_id, data);
      break;
    case 'message_recall':
      onRecall(data);
      break;
    case 'typing':
      onTyping(data);
      break;
    case 'friend_request':
      toast(`${(data.from_user && data.from_user.nickname) || '有人'} 请求添加你为好友`);
      loadRequests();
      break;
    case 'friend_accept':
      toast(`${(data.user && data.user.nickname) || '对方'} 通过了你的好友申请`);
      loadFriends();
      break;
    case 'group_event':
      onGroupEvent(data);
      break;
    case 'notice':
      if (data.kind === 'moment_like') toast('有人赞了你的动态');
      else if (data.kind === 'moment_comment') toast('有人评论了你的动态');
      break;
    case 'kick':
      if (data && data.device && data.device !== 'web') break;
      forceLogout((data && data.reason) || '连接已被断开');
      break;
    case 'error':
      toast((data && data.msg) || '操作失败', 'error');
      break;
    default:
      break;
  }
}

function onGroupEvent(data) {
  loadGroups();
  loadConvs();
  const gid = data && data.group_id;
  if (gid && state.groupDetails[gid]) {
    delete state.groupDetails[gid];
    loadGroupDetail(gid);
  }
  if (state.current && sameId(state.current.group_id, gid) && data.action === 'dissolve') {
    toast('该群已解散');
    state.current = null;
    state.info = null;
  }
}

function onTyping(data) {
  if (!data || !data.conv_id) return;
  const conv = state.current;
  if (!conv || conv.conv_id !== data.conv_id) return;
  if (sameId(data.from, state.me && state.me.id)) return;

  let name = '对方';
  if (conv.conv_type === 2) {
    const detail = state.groupDetails[conv.group_id];
    const member = detail && (detail.members || []).find((m) => sameId(m.id, data.from));
    name = member ? member.group_nickname || member.nickname : '有人';
  } else {
    name = conv.name || '对方';
  }
  state.typing[conv.conv_id] = { name, until: Date.now() + 4000 };
  clearTimeout(typingTimers[conv.conv_id]);
  typingTimers[conv.conv_id] = setTimeout(() => {
    delete state.typing[conv.conv_id];
  }, 4200);
}

const typingTimers = {};
const lastTyping = {};

export function notifyTyping() {
  const conv = state.current;
  if (!conv || !state.wsOpen) return;
  const now = Date.now();
  if (now - (lastTyping[conv.conv_id] || 0) < 2200) return;
  lastTyping[conv.conv_id] = now;
  wsSend({ type: 'typing', data: { conv_id: conv.conv_id } });
}

/* ------------------------------ 列表加载 ------------------------------ */
export async function loadConvs() {
  try {
    const list = await api('GET', '/conversations');
    state.convs = Array.isArray(list) ? list : [];
  } catch (err) {
    if (!state.convsLoaded) toast(err.message, 'error');
  }
  state.convsLoaded = true;
}

export async function loadFriends() {
  try {
    const list = await api('GET', '/friends');
    state.friends = Array.isArray(list) ? list : [];
  } catch (_) {
    state.friends = state.friends || [];
  }
}

export async function loadGroups() {
  try {
    const list = await api('GET', '/groups');
    state.groups = Array.isArray(list) ? list : [];
  } catch (_) {
    state.groups = state.groups || [];
  }
}

export async function loadRequests() {
  try {
    const list = await api('GET', '/friend-requests?type=incoming');
    state.requests = Array.isArray(list) ? list : [];
  } catch (_) {
    state.requests = state.requests || [];
  }
}

let convTimer = null;
function scheduleConvsRefresh() {
  clearTimeout(convTimer);
  convTimer = setTimeout(loadConvs, 320);
}

/* ------------------------------ 会话 ------------------------------ */
export function switchTab(tab) {
  state.tab = tab;
  state.filter = '';
  if (tab === 'moments') {
    ui.mobileChat = true;
    if (!state.momentsLoaded) loadTimeline();
  } else {
    ui.mobileChat = false;
    if (tab === 'convs') loadConvs();
  }
}

export function privateConvId(a, b) {
  const low = cmpId(a, b) < 0 ? String(a) : String(b);
  const high = cmpId(a, b) < 0 ? String(b) : String(a);
  return `p_${low}_${high}`;
}

export function openConv(conv) {
  const known = state.convs.find((c) => c.conv_id === conv.conv_id);
  if (known) {
    conv.name = known.name || conv.name;
    conv.avatar = known.avatar || conv.avatar;
    conv.member_count = known.member_count || conv.member_count;
    conv.pinned = known.pinned;
    conv.muted = known.muted;
  }
  state.current = conv;
  state.quote = null;
  state.info = null;
  if (isNarrow()) ui.mobileChat = true;

  loadHistory(conv.conv_id);
  if (conv.conv_type === 2) loadGroupDetail(conv.group_id);
}

export function openFriendChat(friendId, name = '', avatar = '') {
  const f = state.friends.find((x) => sameId(x.id, friendId));
  openConv({
    conv_id: privateConvId(friendId, state.me.id),
    conv_type: 1,
    target_id: String(friendId),
    name: (f && (f.remark || f.nickname)) || name,
    avatar: (f && f.avatar) || avatar,
    group_id: '0',
  });
}

export function openGroupChat(groupId, name = '', avatar = '') {
  const g = state.groups.find((x) => sameId(x.id, groupId));
  openConv({
    conv_id: `g_${groupId}`,
    conv_type: 2,
    target_id: String(groupId),
    name: (g && g.name) || name || '群聊',
    avatar: (g && g.avatar) || avatar,
    group_id: String(groupId),
    member_count: (g && g.member_count) || 0,
  });
}

export function closeInfo() {
  state.info = null;
}

export function openInfo() {
  const conv = state.current;
  if (!conv) return;
  state.info = conv.conv_type === 2
    ? { type: 'group', id: String(conv.group_id) }
    : { type: 'user', id: String(conv.target_id) };
}

export function openUserInfo(userId) {
  state.info = { type: 'user', id: String(userId) };
}

export async function loadGroupDetail(groupId) {
  try {
    const detail = await api('GET', `/groups/${groupId}`);
    state.groupDetails[groupId] = detail;
    if (state.current && sameId(state.current.group_id, groupId)) {
      state.current.member_count = detail.group.member_count;
      state.current.name = detail.group.name || state.current.name;
      state.current.avatar = detail.group.avatar || state.current.avatar;
    }
  } catch (_) {
    /* 详情加载失败不阻塞聊天 */
  }
}

/* ------------------------------ 消息 ------------------------------ */
function mergeMessages(convId, fetched) {
  const cached = state.messages[convId] || [];
  const ids = new Set(fetched.map((m) => String(m.id)));
  const extra = cached.filter((m) => !ids.has(String(m.id)) && !String(m.id).startsWith('tmp_'));
  return fetched.concat(extra).sort((a, b) => (a.seq || 0) - (b.seq || 0));
}

export async function loadHistory(convId) {
  try {
    const list = await api('GET', `/messages?conv_id=${encodeURIComponent(convId)}&limit=50`);
    const fetched = Array.isArray(list) ? list : [];
    state.messages[convId] = mergeMessages(convId, fetched);
    state.historyEnded[convId] = fetched.length < 50;
  } catch (err) {
    if (!state.messages[convId]) state.messages[convId] = [];
    toast(err.message, 'error');
  }
  if (state.current && state.current.conv_id === convId) markRead();
}

export async function loadMore() {
  const conv = state.current;
  if (!conv || state.loadingMore) return;
  const list = state.messages[conv.conv_id] || [];
  const first = list.find((m) => m.seq);
  if (!first || state.historyEnded[conv.conv_id]) return;

  state.loadingMore = true;
  try {
    const older = await api('GET', `/messages?conv_id=${encodeURIComponent(conv.conv_id)}&before_seq=${first.seq}&limit=50`);
    const arr = Array.isArray(older) ? older : [];
    state.messages[conv.conv_id] = arr.concat(list);
    state.historyEnded[conv.conv_id] = arr.length < 50;
  } catch (err) {
    toast(err.message, 'error');
  } finally {
    state.loadingMore = false;
  }
}

export function markRead() {
  const conv = state.current;
  if (!conv || document.visibilityState !== 'visible') return;
  const list = state.messages[conv.conv_id] || [];
  let last = null;
  for (let i = list.length - 1; i >= 0; i -= 1) {
    if (list[i].seq && list[i].status !== -1) { last = list[i]; break; }
  }
  if (!last) return;

  api('POST', '/messages/read', { conv_id: conv.conv_id, seq: last.seq }).catch(() => {});
  const known = state.convs.find((c) => c.conv_id === conv.conv_id);
  if (known && known.unread) known.unread = 0;
}

function pushMessage(convId, message) {
  const arr = state.messages[convId] || (state.messages[convId] = []);
  if (message.id && !String(message.id).startsWith('tmp_') && arr.some((x) => sameId(x.id, message.id))) return;
  arr.push(message);
}

function replaceTemp(convId, tempId, dto) {
  const arr = state.messages[convId];
  if (!arr) return;
  const idx = arr.findIndex((m) => sameId(m.id, tempId) || (dto.client_msg_id && m.client_msg_id === dto.client_msg_id));
  if (idx >= 0) arr[idx] = dto;
  else if (!arr.some((m) => sameId(m.id, dto.id))) arr.push(dto);
  scheduleConvsRefresh();
}

function messageEndpoint(conv) {
  return conv.conv_type === 2 ? '/messages/group' : '/messages/private';
}

function targetBody(conv) {
  return conv.conv_type === 2
    ? { group_id: String(conv.group_id) }
    : { to_id: String(conv.target_id) };
}

export async function sendText(text) {
  const conv = state.current;
  if (!conv || !text.trim()) return;
  const content = text.trim();
  const quote = state.quote;
  const quoteId = quote ? String(quote.id) : '';

  const temp = {
    id: `tmp_${rid()}`,
    client_msg_id: rid(),
    conv_id: conv.conv_id,
    from_id: state.me.id,
    from_user: state.me,
    msg_type: 1,
    content,
    quote: quote ? { msg_id: quoteId, from_id: quote.from_id, preview: previewOf(quote) } : undefined,
    created_at: new Date().toISOString(),
    status: -1,
  };

  state.quote = null;
  pushMessage(conv.conv_id, temp);

  try {
    const dto = await api('POST', messageEndpoint(conv), {
      client_msg_id: temp.client_msg_id,
      msg_type: 1,
      content,
      ...(quoteId ? { quote_msg_id: quoteId } : {}),
      ...targetBody(conv),
    });
    replaceTemp(conv.conv_id, temp.id, dto);
  } catch (err) {
    temp.status = -2;
    temp.failed = true;
    toast(err.message, 'error');
  }
}

export async function sendImage(file) {
  const conv = state.current;
  if (!conv || !file) return;

  const localUrl = URL.createObjectURL(file);
  const temp = {
    id: `tmp_${rid()}`,
    client_msg_id: rid(),
    conv_id: conv.conv_id,
    from_id: state.me.id,
    from_user: state.me,
    msg_type: 2,
    content: localUrl,
    extra: JSON.stringify({ url: localUrl }),
    created_at: new Date().toISOString(),
    status: -1,
  };
  pushMessage(conv.conv_id, temp);

  try {
    const url = await upload(file);
    const dto = await api('POST', messageEndpoint(conv), {
      client_msg_id: temp.client_msg_id,
      msg_type: 2,
      content: url,
      extra: JSON.stringify({ url }),
      ...targetBody(conv),
    });
    replaceTemp(conv.conv_id, temp.id, dto);
    URL.revokeObjectURL(localUrl);
  } catch (err) {
    temp.status = -2;
    temp.failed = true;
    toast(err.message, 'error');
  }
}

export async function recallMessage(message) {
  const ok = await confirmDialog({
    title: '撤回消息',
    text: '撤回后对方将看不到这条内容，确定撤回吗？',
    ok: '撤回',
  });
  if (!ok) return;
  try {
    await api('POST', `/messages/${message.id}/recall`);
    message.status = 1;
    toast('已撤回');
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function setConvFlag(convId, patch) {
  try {
    await api('PATCH', `/conversations/${encodeURIComponent(convId)}`, patch);
    const conv = state.convs.find((c) => c.conv_id === convId);
    if (conv) Object.assign(conv, patch);
    if (state.current && state.current.conv_id === convId) Object.assign(state.current, patch);
    toast('已更新');
  } catch (err) {
    toast(err.message, 'error');
  }
}

function onIncoming(message) {
  if (!message || !message.conv_id) return;
  const cached = state.messages[message.conv_id];
  if (cached) {
    if (!cached.some((x) => sameId(x.id, message.id))) cached.push(message);
  } else if (state.current && state.current.conv_id === message.conv_id) {
    state.messages[message.conv_id] = [message];
  }

  const isCurrent = state.current && state.current.conv_id === message.conv_id;
  if (isCurrent) {
    markRead();
  } else if (!(message.from_user && sameId(message.from_user.id, state.me.id))) {
    const who = (message.from_user && message.from_user.nickname) || '新消息';
    toast(`${who}：${previewOf(message)}`);
  }
  scheduleConvsRefresh();
}

function onRecall(data) {
  const arr = state.messages[data.conv_id];
  if (!arr) return;
  const message = arr.find((x) => sameId(x.id, data.msg_id));
  if (message) message.status = 1;
}

/* ------------------------------ 好友 ------------------------------ */
export async function handleRequest(requestId, accept) {
  try {
    await api('POST', `/friend-requests/${requestId}/handle`, { accept });
    toast(accept ? '已同意好友申请' : '已拒绝');
    await Promise.all([loadRequests(), loadFriends(), loadConvs()]);
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function sendFriendRequest(userId) {
  try {
    await api('POST', '/friend-requests', { to_user_id: String(userId), message: '你好，我想加你为好友' });
    toast('好友申请已发送');
    return true;
  } catch (err) {
    toast(err.message, 'error');
    return false;
  }
}

export async function blockUser(userId) {
  const ok = await confirmDialog({ title: '加入黑名单', text: '拉黑后你们将无法互发消息、互相查看动态。', ok: '拉黑' });
  if (!ok) return;
  try {
    await api('POST', '/blacklist', { user_id: String(userId) });
    toast('已加入黑名单');
    await loadFriends();
    state.info = null;
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function unblockUser(userId) {
  try {
    await api('DELETE', `/blacklist/${userId}`);
    toast('已解除拉黑');
    await Promise.all([loadBlacklist(), loadFriends()]);
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function loadBlacklist() {
  try {
    const list = await api('GET', '/blacklist');
    state.blocked = Array.isArray(list) ? list : [];
  } catch (_) {
    state.blocked = [];
  }
  return state.blocked;
}

export async function toggleStar(friendId, starred) {
  try {
    await api('PATCH', `/friends/${friendId}`, { starred });
    await loadFriends();
    toast(starred ? '已设为星标' : '已取消星标');
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function saveRemark(friendId, remark) {
  try {
    await api('PATCH', `/friends/${friendId}`, { remark });
    await loadFriends();
    if (state.current && sameId(state.current.target_id, friendId)) {
      const f = state.friends.find((x) => sameId(x.id, friendId));
      if (f) state.current.name = f.remark || f.nickname;
    }
    toast('备注已更新');
    return true;
  } catch (err) {
    toast(err.message, 'error');
    return false;
  }
}

export async function deleteFriend(friendId, name) {
  const ok = await confirmDialog({
    title: '删除好友',
    text: `确定删除「${name}」吗？聊天记录会保留，但你们将不再是好友。`,
    ok: '删除',
  });
  if (!ok) return;
  try {
    await api('DELETE', `/friends/${friendId}`);
    toast('已删除好友');
    state.info = null;
    await Promise.all([loadFriends(), loadConvs()]);
  } catch (err) {
    toast(err.message, 'error');
  }
}

/* ------------------------------ 群聊 ------------------------------ */
export async function createGroup(name, memberIds) {
  const group = await api('POST', '/groups', { name, member_ids: memberIds });
  await loadGroups();
  openGroupChat(group.id, group.name, group.avatar);
  toast('群聊已创建');
  return group;
}

export async function saveGroup(groupId, patch) {
  try {
    await api('PATCH', `/groups/${groupId}`, patch);
    delete state.groupDetails[groupId];
    await Promise.all([loadGroupDetail(groupId), loadGroups(), loadConvs()]);
    toast('群资料已更新');
    return true;
  } catch (err) {
    toast(err.message, 'error');
    return false;
  }
}

export async function inviteMembers(groupId, userIds) {
  try {
    await api('POST', `/groups/${groupId}/members`, { user_ids: userIds });
    delete state.groupDetails[groupId];
    await Promise.all([loadGroupDetail(groupId), loadGroups(), loadConvs()]);
    toast('邀请已发送');
    return true;
  } catch (err) {
    toast(err.message, 'error');
    return false;
  }
}

async function groupAction(method, path, body, groupId, message) {
  try {
    await api(method, path, body);
    if (message) toast(message);
    if (groupId) {
      delete state.groupDetails[groupId];
      await Promise.all([loadGroupDetail(groupId), loadGroups()]);
    }
  } catch (err) {
    toast(err.message, 'error');
  }
}

export function muteMember(groupId, userId, muted) {
  return groupAction('POST', `/groups/${groupId}/mute`, { user_id: String(userId), muted }, groupId, muted ? '已禁言' : '已解除禁言');
}

export function setAdmin(groupId, userId, admin) {
  return groupAction('POST', `/groups/${groupId}/admin`, { user_id: String(userId), admin }, groupId, admin ? '已设为管理员' : '已取消管理员');
}

export function removeMember(groupId, userId) {
  return groupAction('DELETE', `/groups/${groupId}/members/${userId}`, undefined, groupId, '已移出群聊');
}

export async function dissolveGroup(groupId) {
  const ok = await confirmDialog({ title: '解散群聊', text: '解散后所有成员都会被移出，且无法恢复。', ok: '解散' });
  if (!ok) return;
  try {
    await api('DELETE', `/groups/${groupId}`);
    toast('群聊已解散');
    state.info = null;
    state.current = null;
    await Promise.all([loadGroups(), loadConvs()]);
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function leaveGroup(groupId) {
  const ok = await confirmDialog({ title: '退出群聊', text: '退出后将不再接收该群消息。', ok: '退出' });
  if (!ok) return;
  try {
    await api('DELETE', `/groups/${groupId}/members/${state.me.id}`);
    toast('已退出群聊');
    state.info = null;
    state.current = null;
    await Promise.all([loadGroups(), loadConvs()]);
  } catch (err) {
    toast(err.message, 'error');
  }
}

/* ------------------------------ 资料 ------------------------------ */
export async function pickAvatar(file) {
  const url = await upload(file);
  state.pendingAvatar = url;
  return url;
}

export async function saveProfile({ nickname, signature }) {
  try {
    const patch = { nickname, signature };
    if (state.pendingAvatar) patch.avatar = state.pendingAvatar;
    const user = await api('PATCH', '/users/me', patch);
    state.me = user;
    state.pendingAvatar = '';
    toast('资料已保存');
    return true;
  } catch (err) {
    toast(err.message, 'error');
    return false;
  }
}

export async function loadUser(userId) {
  return api('GET', `/users/${userId}`);
}

/* ------------------------------ 朋友圈 ------------------------------ */
export async function loadTimeline(more = false) {
  const last = state.moments[state.moments.length - 1];
  const query = `limit=10${more && last ? `&before_id=${encodeURIComponent(last.id)}` : ''}`;
  try {
    const list = await api('GET', `/moments/timeline?${query}`);
    const arr = Array.isArray(list) ? list : [];
    state.moments = more ? state.moments.concat(arr) : arr;
    state.momentsEnded = arr.length < 10;
    state.momentsLoaded = true;
  } catch (err) {
    state.momentsLoaded = true;
    toast(err.message, 'error');
  }
}

export function findMoment(id) {
  return state.moments.find((m) => sameId(m.id, id));
}

export async function toggleLike(id) {
  const moment = findMoment(id);
  if (!moment) return;
  const next = !moment.liked;
  moment.liked = next;
  moment.like_count = Math.max(0, (moment.like_count || 0) + (next ? 1 : -1));
  moment.likes = moment.likes || [];
  if (next) {
    if (!moment.likes.some((u) => sameId(u.id, state.me.id))) {
      moment.likes.unshift({ id: state.me.id, nickname: state.me.nickname, avatar: state.me.avatar });
    }
  } else {
    moment.likes = moment.likes.filter((u) => !sameId(u.id, state.me.id));
  }
  try {
    if (next) await api('POST', `/moments/${id}/like`);
    else await api('DELETE', `/moments/${id}/like`);
  } catch (err) {
    toast(err.message, 'error');
    moment.liked = !next;
    moment.like_count = Math.max(0, (moment.like_count || 0) + (next ? -1 : 1));
    await loadTimeline();
  }
}

export async function addComment(id, content) {
  const moment = findMoment(id);
  if (!moment || !content.trim()) return false;
  try {
    const comment = await api('POST', `/moments/${id}/comments`, { content: content.trim() });
    moment.comments = (moment.comments || []).concat(comment);
    moment.comment_count = (moment.comment_count || 0) + 1;
    return true;
  } catch (err) {
    toast(err.message, 'error');
    return false;
  }
}

export async function deleteComment(momentId, commentId) {
  try {
    await api('DELETE', `/moments/comments/${commentId}`);
    const moment = findMoment(momentId);
    if (moment) {
      moment.comments = (moment.comments || []).filter((c) => !sameId(c.id, commentId));
      moment.comment_count = Math.max(0, (moment.comment_count || 0) - 1);
    }
    toast('评论已删除');
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function deleteMoment(id) {
  const ok = await confirmDialog({ title: '删除动态', text: '删除后无法恢复，确定删除这条动态吗？', ok: '删除' });
  if (!ok) return;
  try {
    await api('DELETE', `/moments/${id}`);
    state.moments = state.moments.filter((m) => !sameId(m.id, id));
    toast('已删除');
  } catch (err) {
    toast(err.message, 'error');
  }
}

export async function publishMoment({ content, files, visibility }) {
  const images = [];
  for (const file of files) images.push(await upload(file));
  await api('POST', '/moments', { content, images, visibility });
  await loadTimeline();
  toast('动态已发布');
}

export async function loadUserMoments(userId) {
  return api('GET', `/moments/users/${userId}?limit=20`);
}

export async function searchMessages(convId, keyword) {
  return api('GET', `/messages/search?conv_id=${encodeURIComponent(convId)}&keyword=${encodeURIComponent(keyword)}&limit=30`);
}
