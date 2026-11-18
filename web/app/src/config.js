/**
 * 部署相关的三个基地址，默认全部同源 —— 与「gin 直接托管 web/index.html」完全一致。
 *
 * 完全分离部署（前端在 CDN / 静态站点，后端在 api.example.com）时，
 * 用 VITE_* 在构建期覆盖：
 *
 *   VITE_API_BASE=https://api.example.com/api/v1 \
 *   VITE_WS_BASE=wss://api.example.com \
 *   VITE_ASSET_BASE=https://api.example.com \
 *   npm run build
 *
 * VITE_WS_BASE 也接受 https:// 写法，构建时会自动换成 wss://。
 */
const trim = (value) => String(value || '').replace(/\/+$/, '');
const env = import.meta.env;

export const API_BASE = trim(env.VITE_API_BASE) || '/api/v1';
export const WS_BASE = trim(env.VITE_WS_BASE);
export const ASSET_BASE = trim(env.VITE_ASSET_BASE);

const ABSOLUTE = /^(?:[a-z][a-z0-9+.-]*:|\/\/)/i;

/** 把后端返回的 /uploads/... 补成可直接访问的地址；已经是绝对地址的原样返回。 */
export function assetUrl(url) {
  if (!url) return '';
  if (ABSOLUTE.test(url)) return url;
  return ASSET_BASE + (url.startsWith('/') ? url : `/${url}`);
}

/** 拼 WebSocket 地址：未配置时用当前站点，配置了就用配置的 origin。 */
export function wsUrl(path) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const base = WS_BASE ? WS_BASE.replace(/^http/i, 'ws') : `${proto}://${location.host}`;
  return `${base}${path}`;
}
