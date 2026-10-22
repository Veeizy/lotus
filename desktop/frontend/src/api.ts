// 类型化 API 客户端（契约见 PROTOCOL.md §2；与 Web 版 fetch 语义一致）。
import type {
  AppConfig, BrowseData, DiscoverItem, FirewallStatus, JoinResp, NetState, PrefetchResp, Progress,
  RootsResp, TimesResp, LanDiscoverResp } from './types';

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function getJSON<T>(url: string, timeoutMs = 45000): Promise<T> {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const r = await fetch(url, { signal: ctrl.signal });
    const data = (await r.json().catch(() => ({}))) as T & { error?: string };
    if (!r.ok) throw new ApiError(r.status, data.error || `HTTP ${r.status}`);
    return data;
  } finally {
    clearTimeout(timer);
  }
}

async function postJSON<T>(url: string, body: unknown, timeoutMs = 30000): Promise<T> {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body ?? {}),
      signal: ctrl.signal,
    });
    const data = (await r.json().catch(() => ({}))) as T & { error?: string };
    if (!r.ok) throw new ApiError(r.status, data.error || `HTTP ${r.status}`);
    return data;
  } finally {
    clearTimeout(timer);
  }
}

const enc = encodeURIComponent;

// 消费端基址：安卓壳以本地资产加载本前端时经 ?server=http://ip:port 指向服务端
// （跨域，服务端需开 api.cors）；无参数 = 同源相对路径（桌面端，零变化）。
const BASE = (() => {
  const s = new URLSearchParams(location.search).get('server')?.trim() || '';
  return s.replace(/\/+$/, '');
})();

export const api = {
  roots: () => getJSON<RootsResp>(`${BASE}/api/roots`),

  browse: (path: string) => getJSON<BrowseData>(`${BASE}/api/browse?path=${enc(path)}`),

  times: (path: string) => getJSON<TimesResp>(`${BASE}/api/times?path=${enc(path)}`, 60000),

  imageUrl: (p: string) => `${BASE}/api/image?p=${enc(p)}`,

  netState: () => getJSON<NetState>(`${BASE}/api/net/state`, 10000),

  netDiscover: () => getJSON<{ ok: boolean; discovered: DiscoverItem[] }>(`${BASE}/api/net/discover`, 10000),

  netBrowse: (node: string, path: string) =>
    getJSON<BrowseData>(`${BASE}/api/net/browse?node=${enc(node)}&path=${enc(path)}`, 45000),

  netTimes: (node: string, path: string) =>
    getJSON<TimesResp>(`${BASE}/api/net/times?node=${enc(node)}&path=${enc(path)}`, 45000),

  netImageUrl: (node: string, p: string) => `${BASE}/api/net/image?node=${enc(node)}&p=${enc(p)}`,

  netJoin: (addrs: string[]) => postJSON<JoinResp>(`${BASE}/api/net/join`, { addrs }, 60000),

  netLeave: () => postJSON<{ ok: boolean }>(`${BASE}/api/net/leave`, {}),

  netSettings: (patch: {
    name?: string; share?: boolean; cacheServe?: boolean; concurrency?: number; advertise?: string;
    lanDiscovery?: boolean; scanPorts?: number[];
  }) => postJSON<{ ok: boolean }>(`${BASE}/api/net/settings`, patch),

  netLanDiscover: () => postJSON<LanDiscoverResp>(`${BASE}/api/net/lan/discover`, {}, 30000),

  netPrefetch: (node: string, folder: string, paths: string[]) =>
    postJSON<PrefetchResp>(`${BASE}/api/net/prefetch`, { node, folder, paths }, 60000),

  netCancel: (node: string, folder: string) =>
    postJSON<{ ok: boolean }>(`${BASE}/api/net/cancel`, { node, folder }, 15000),

  netProgress: (node: string, folder: string) =>
    getJSON<Progress>(`${BASE}/api/net/progress?node=${enc(node)}&folder=${enc(folder)}`, 10000),

  configGet: () => getJSON<AppConfig>(`${BASE}/api/config`, 10000),

  configSetPaths: (paths: string[]) =>
    postJSON<{ ok: boolean; warns: string[]; roots: { name: string; path: string; mtime: number }[] }>(
      `${BASE}/api/config/paths`, { paths }, 30000),

  configSetCacheDir: (dir: string) =>
    postJSON<{ ok: boolean; cacheDir: string }>(`${BASE}/api/config/cache-dir`, { dir }, 30000),

  openFolder: (dir: string) => postJSON<{ ok: boolean }>(`${BASE}/api/desktop/open-folder`, { dir }, 15000),

  firewallStatus: () => getJSON<FirewallStatus>(`${BASE}/api/desktop/firewall`, 15000),

  firewallAllow: () =>
    postJSON<{ ok: boolean; message?: string }>(`${BASE}/api/desktop/firewall/allow`, {}, 30000),
};
