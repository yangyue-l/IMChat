import { API_BASE } from './config';

export { API_BASE };

/** 由 store 注入：拿当前令牌、401 时统一处理。 */
export const auth = {
  token: '',
  onUnauthorized: null,
};

export class ApiError extends Error {
  constructor(message, code) {
    super(message);
    this.code = code;
  }
}

export async function api(method, path, body) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (auth.token) headers.Authorization = `Bearer ${auth.token}`;

  let res;
  try {
    res = await fetch(API_BASE + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (_) {
    throw new ApiError('网络异常，请确认服务已启动', 0);
  }

  let json;
  try {
    json = await res.json();
  } catch (_) {
    json = { code: res.status, msg: `请求失败 (${res.status})` };
  }

  if (json.code !== 0) {
    if ((json.code === 40100 || json.code === 40101) && auth.token && auth.onUnauthorized) {
      auth.onUnauthorized();
    }
    throw new ApiError(json.msg || '请求失败', json.code);
  }
  return json.data;
}

export async function upload(file) {
  const fd = new FormData();
  fd.append('file', file);

  let res;
  try {
    res = await fetch(`${API_BASE}/upload`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${auth.token}` },
      body: fd,
    });
  } catch (_) {
    throw new ApiError('上传失败，请检查网络', 0);
  }

  const json = await res.json().catch(() => ({ code: res.status, msg: '上传失败' }));
  if (json.code !== 0) throw new ApiError(json.msg || '上传失败', json.code);
  return json.data.url;
}
