export const sameId = (a, b) => String(a) === String(b);

/** 雪花 ID 超出 Number 安全整数范围，比较大小必须走字符串。 */
export function cmpId(a, b) {
  const x = String(a);
  const y = String(b);
  if (x.length !== y.length) return x.length - y.length;
  return x < y ? -1 : x > y ? 1 : 0;
}

export const rid = () =>
  crypto.randomUUID
    ? crypto.randomUUID().replace(/-/g, '')
    : Date.now().toString(36) + Math.random().toString(36).slice(2, 10);

const pad2 = (n) => String(n).padStart(2, '0');

export const toDate = (value) => {
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
};

export const clock = (d) => `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;

const startOfDay = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());

const dayDiff = (d) => Math.round((startOfDay(new Date()) - startOfDay(d)) / 86400000);

export function fmtListTime(value) {
  const d = toDate(value);
  if (!d) return '';
  const diff = dayDiff(d);
  if (diff === 0) return clock(d);
  if (diff === 1) return '昨天';
  if (d.getFullYear() === new Date().getFullYear()) return `${d.getMonth() + 1}/${d.getDate()}`;
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
}

export function fmtDay(d) {
  const diff = dayDiff(d);
  if (diff === 0) return '今天';
  if (diff === 1) return '昨天';
  if (d.getFullYear() === new Date().getFullYear()) return `${d.getMonth() + 1} 月 ${d.getDate()} 日`;
  return `${d.getFullYear()} 年 ${d.getMonth() + 1} 月 ${d.getDate()} 日`;
}

export function fmtRelative(value) {
  const d = toDate(value);
  if (!d) return '';
  const diff = Date.now() - d.getTime();
  if (diff < 60e3) return '刚刚';
  if (diff < 3600e3) return `${Math.floor(diff / 60e3)} 分钟前`;
  if (diff < 86400e3) return `${Math.floor(diff / 3600e3)} 小时前`;
  if (diff < 7 * 86400e3) return `${Math.floor(diff / 86400e3)} 天前`;
  return `${d.getMonth() + 1} 月 ${d.getDate()} 日`;
}

export const dayKey = (d) => `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;

/** 用昵称生成稳定的头像色相，避免同名不同色。 */
export function hueOf(name) {
  let hue = 7;
  for (const ch of String(name || '?')) hue = (hue * 31 + ch.codePointAt(0)) % 360;
  return hue;
}

export const initialOf = (name) => (Array.from(String(name || '?'))[0] || '?').toUpperCase();

export function escapeHtml(value) {
  return String(value == null ? '' : value).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  }[c]));
}

/** 把纯文本里的链接变成可点击的 <a>，其余内容全部转义。 */
export function linkify(text) {
  const raw = String(text == null ? '' : text);
  const re = /https?:\/\/[^\s<>"']+/g;
  let out = '';
  let last = 0;
  let m;
  while ((m = re.exec(raw))) {
    out += escapeHtml(raw.slice(last, m.index));
    const url = m[0];
    out += `<a href="${escapeHtml(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(url)}</a>`;
    last = m.index + url.length;
  }
  return out + escapeHtml(raw.slice(last));
}

export function previewOf(m) {
  if (!m) return '';
  if (m.msg_type === 2) return '[图片]';
  if (m.msg_type === 3) return `[文件] ${m.content || ''}`;
  return String(m.content || '').slice(0, 64);
}

export function imageUrlOf(m) {
  let url = m.content;
  try {
    const extra = JSON.parse(m.extra || '{}');
    if (extra.url) url = extra.url;
  } catch (_) {
    /* extra 不是 JSON 时直接用 content */
  }
  return url;
}
