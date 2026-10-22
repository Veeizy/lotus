// 🌐 网络侧栏：状态/成员/发现/入网/设置（对应原版网络面板，语义一致）。
import { api, ApiError } from './api';
import { $, clear, h } from './dom';
import { applyFolderView, refreshNetState, state } from './app';

export function isOpen(): boolean {
  return !$('net-panel').hidden;
}

export function open(): void {
  ($('net-panel') as HTMLElement).hidden = false;
  ($('net-backdrop') as HTMLElement).hidden = false;
  void refreshNetState();
}

export function close(): void {
  ($('net-panel') as HTMLElement).hidden = true;
  ($('net-backdrop') as HTMLElement).hidden = true;
}

export function renderIfOpen(): void {
  if (isOpen()) render();
}

async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.append(ta);
    ta.select();
    try { document.execCommand('copy'); } catch { /* 忽略 */ }
    ta.remove();
  }
}

function msg(text: string): void {
  $('np-msg').textContent = text;
}

export function render(): void {
  const s = state.netState;
  if (!s) return;

  ($('np-name') as HTMLInputElement).value = s.me.name || '';
  $('np-id').textContent = s.me.id;
  $('np-pub').textContent = s.me.publicKey;
  const secret = $('np-secret');
  secret.textContent = s.me.secret || '（仅本机可见）';
  secret.dataset.has = s.me.secret ? '1' : '';

  ($('np-share') as HTMLInputElement).checked = !!s.share;
  ($('np-cacheserve') as HTMLInputElement).checked = !!s.cacheServe;
  ($('np-conc') as HTMLInputElement).value = String(s.concurrency);
  ($('np-landisc') as HTMLInputElement).checked = s.lanDiscovery !== false;
  ($('np-ports') as HTMLInputElement).value = (s.scanPorts || []).join(',');

  // 上层（hubs）连接状态
  const ups = $('np-uplinks');
  clear(ups);
  for (const u of s.uplinks || []) {
    ups.append(h('div', { class: 'np-item' },
      h('span', { class: `st ${u.status === 'connected' ? 'ok' : 'bad'}` },
        u.status === 'connected' ? '已连接' : u.status === 'connecting' ? '连接中…' : '重连中'),
      h('span', { class: 'grow addr' }, u.addr),
    ));
  }
  ($('np-leave-row') as HTMLElement).hidden = !(s.uplinks && s.uplinks.length);

  // 成员列表
  const list = $('np-members');
  clear(list);
  const members = s.members || [];
  for (const m of members) {
    list.append(h('div', { class: 'np-item' },
      h('span', { class: 'grow' },
        h('span', { class: 'name' }, m.name || '节点 ' + m.id.slice(0, 8)),
        h('span', { class: 'addr' }, `  ${m.addr || m.id}${m.direct ? ' · 直连' : ' · 中继'}`)),
      h('button', {
        class: 'mini',
        onclick: () => {
          close();
          void import('./app').then((app) => app.openNode(m.id, m.name));
        },
      }, '🖼 浏览图库'),
    ));
  }
  ($('np-members-empty') as HTMLElement).hidden = members.length > 0;

  // 发现列表（局域网 mDNS）
  void renderDiscovered();
}

async function renderDiscovered(): Promise<void> {
  try {
    const d = await api.netDiscover();
    const box = $('np-discovered');
    clear(box);
    const items = (d.discovered || []).filter((x) => x.id !== state.netState?.me.id);
    ($('np-discovered-empty') as HTMLElement).hidden = items.length > 0;
    for (const it of items) {
      const btn = h('button', {
        class: 'mini',
        disabled: it.known,
        onclick: async (e) => {
          const b = e.target as HTMLButtonElement;
          b.disabled = true;
          b.textContent = '连接中…';
          try {
            const r = await api.netJoin([it.addr]);
            const res = r.results?.[0];
            msg(res?.ok ? `✅ 已接入 ${it.addr}` : `❌ ${it.addr}：${res?.error || '失败'}`);
          } catch (e2) {
            msg('入网失败：' + (e2 instanceof ApiError ? e2.message : String(e2)));
          }
          void refreshNetState();
        },
      }, it.known ? '✓ 已在网络中' : '⚡ 一键入网');
      box.append(h('div', { class: 'np-item' },
        h('span', { class: 'grow' },
          h('span', { class: 'name' }, it.name || '节点 ' + it.id.slice(0, 8)),
          h('span', { class: 'addr' }, '  ' + it.addr)),
        btn,
      ));
    }
  } catch { /* 发现不可用不影响面板 */ }
}

export function bindNetPanel(): void {
  $('net-chip').addEventListener('click', () => (isOpen() ? close() : open()));
  $('np-close').addEventListener('click', close);
  $('net-backdrop').addEventListener('click', close);

  $('np-id-copy').addEventListener('click', () => void copyText($('np-id').textContent || ''));
  $('np-pub-copy').addEventListener('click', () => void copyText($('np-pub').textContent || ''));
  $('np-secret-copy').addEventListener('click', () => void copyText($('np-secret').textContent || ''));

  $('np-name-save').addEventListener('click', async () => {
    const name = ($('np-name') as HTMLInputElement).value.trim();
    if (!name) return;
    await api.netSettings({ name }).catch(() => {});
    void refreshNetState();
  });
  $('np-share').addEventListener('change', (e) => {
    void api.netSettings({ share: (e.target as HTMLInputElement).checked });
  });
  $('np-cacheserve').addEventListener('change', (e) => {
    void api.netSettings({ cacheServe: (e.target as HTMLInputElement).checked });
  });
  $('np-conc').addEventListener('change', (e) => {
    const n = Math.max(1, Math.min(16, parseInt((e.target as HTMLInputElement).value, 10) || 5));
    (e.target as HTMLInputElement).value = String(n);
    void api.netSettings({ concurrency: n });
  });
  $('np-landisc').addEventListener('change', (e) => {
    void api.netSettings({ lanDiscovery: (e.target as HTMLInputElement).checked });
    msg((e.target as HTMLInputElement).checked ? '已开启局域网自动发现（每分钟自动扫描）' : '已关闭局域网自动发现（本机服务连接不受影响）');
  });
  $('np-ports').addEventListener('change', (e) => {
    const input = e.target as HTMLInputElement;
    const ports = input.value.split(/[,，;；\s]+/).map((x) => parseInt(x, 10)).filter((x) => x >= 1 && x <= 65535);
    input.value = ports.join(',');
    void api.netSettings({ scanPorts: ports });
    msg('扫描端口已更新（默认 3456 恒在扫描列表）');
  });
  $('np-lan-scan').addEventListener('click', async () => {
    const b = $('np-lan-scan') as HTMLButtonElement;
    b.disabled = true;
    b.textContent = '扫描中…';
    msg('正在扫描局域网（mDNS + 端口探测，最长 20 秒）…');
    try {
      const r = await api.netLanDiscover();
      if (!r.results.length) {
        msg('未发现新的局域网节点');
      } else {
        const lines = r.results.map((x) =>
          x.ok ? `✅ ${x.addr}${x.name ? '（' + x.name + '）' : ''}` : `➖ ${x.addr}：${x.error || '未加入'}`);
        msg('扫描完成：\n' + lines.join('\n'));
      }
      void refreshNetState();
    } catch (e) {
      msg('扫描失败：' + (e instanceof ApiError ? e.message : String(e)));
    } finally {
      b.disabled = false;
      b.textContent = '立即扫描并入网';
    }
  });

  $('np-join').addEventListener('click', async () => {
    const raw = ($('np-addr') as HTMLInputElement).value.trim();
    if (!raw) return;
    const addrs = raw.split(/[,，;；\s]+/).filter(Boolean);
    try {
      const r = await api.netJoin(addrs);
      const lines = (r.results || []).map((x) =>
        x.ok ? `✅ ${x.addr}${x.node ? ' → ' + x.node : ''}` : `❌ ${x.addr}：${x.error}`);
      msg(lines.join('\n') || '没有有效地址');
      if (lines.some((l) => l[0] === '✅')) ($('np-addr') as HTMLInputElement).value = '';
      setTimeout(() => void refreshNetState(), 600);
    } catch (e) {
      msg('入网失败：' + (e instanceof ApiError ? e.message : String(e)));
    }
  });
  $('np-addr').addEventListener('keydown', (e) => {
    e.stopPropagation();
    if ((e as KeyboardEvent).key === 'Enter') ($('np-join') as HTMLButtonElement).click();
  });

  $('np-leave').addEventListener('click', async () => {
    await api.netLeave().catch(() => {});
    msg('已退出网络');
    void refreshNetState();
    applyFolderView();
  });
}
