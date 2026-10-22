// lotus-server 控制台（dashboard）：轮询 daemon 管理面 + 本机控制端点。
// 所有 daemon 调用走相对路径 /p/*（同源，token 由 Go 宿主附加）；本机控制走 /ctl/*。

interface CtlConfig {
  cfgPath: string;
  daemonBase: string;
  exe: string;
  service: { installed: boolean; running: boolean };
}

interface KindCount { kind: string; count: number; ok: number; fail: number; bytes: number; durMs: number }
interface PeerTraffic { peer: string; in: number; out: number }
interface StatusReply {
  id: string; name: string; startedAt: number; uptimeSec: number;
  httpPort: number; quicPort: number; joined: boolean; hub: boolean; requests: number;
  folders: { name: string; path: string; mtime: number }[];
  uplinks: { addr: string; status: string }[];
  members: { id: string; name: string; addr: string; direct: boolean; local?: boolean }[];
  kinds: KindCount[];
  traffic: { peers: PeerTraffic[]; totalIn: number; totalOut: number };
}
interface AccessEntry {
  ts: number; kind: string; peer: string; path?: string;
  bytes?: number; durMs?: number; ok: boolean; extra?: string;
}
interface FirewallNeed {
  proto: string; port: number; desc: string; allowed: boolean;
  ruleName?: string; blockBy?: string;
}
interface FirewallStatus {
  supported: boolean; reason?: string; firewallOn: boolean;
  activeProfiles: string[]; exe: string; needs: FirewallNeed[]; allowed: boolean;
}

const $ = <T extends HTMLElement = HTMLElement>(id: string): T => document.getElementById(id) as T;

let ctl: CtlConfig | null = null;
let netStatus: StatusReply | null = null;
let online = false;
let tab = 'overview';
let fw: FirewallStatus | null = null;
let fwAllowing = false;
let fwDismissed = false;

/* ---------------- 基础请求 ---------------- */

async function pfetch(path: string, opts?: RequestInit): Promise<any> {
  const r = await fetch('/p' + path, opts);
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
  return body;
}

async function ctlFetch(path: string, opts?: RequestInit): Promise<any> {
  const r = await fetch(path, opts);
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
  return body;
}

let toastTimer = 0;
function toast(msg: string): void {
  const el = $('toast');
  el.textContent = msg;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => { el.hidden = true; }, 4000);
}

/* ---------------- 格式化 ---------------- */

function fmtBytes(n: number): string {
  if (!n) return '0 B';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return i === 0 ? `${v} ${u[i]}` : `${v.toFixed(1)} ${u[i]}`;
}

function fmtUptime(sec: number): string {
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d) return `${d}天${h}时${m}分`;
  if (h) return `${h}时${m}分`;
  return `${m}分`;
}

function shortID(id: string): string { return id.length > 16 ? id.slice(0, 16) + '…' : id; }

function h(tag: string, attrs: Record<string, string> | null, ...children: (string | Node)[]): HTMLElement {
  const el = document.createElement(tag);
  if (attrs) for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  for (const c of children) el.append(c);
  return el;
}

/* ---------------- 状态轮询 ---------------- */

async function refreshStatus(): Promise<void> {
  try {
    netStatus = await pfetch('/api/v1/admin/status') as StatusReply;
    setOnline(true);
  } catch {
    netStatus = null;
    setOnline(false);
  }
  renderHeader();
  if (tab === 'overview') renderOverview();
  if (tab === 'traffic') renderTraffic();
  // 管理页的列表也随轮询即时刷新（否则增删后要切页签才能看到）
  if (tab === 'manage') {
    renderFolders();
    renderUplinks();
  }
}

function setOnline(on: boolean): void {
  if (online === on) return;
  online = on;
  $('conn-dot').className = 'dot ' + (on ? 'ok' : 'bad');
  $('offline-banner').hidden = on;
}

function renderHeader(): void {
  if (netStatus) {
    $('meta-name').textContent = netStatus.name || '（未命名节点）';
    $('meta-id').textContent = shortID(netStatus.id);
    $('meta-uptime').textContent = `已运行 ${fmtUptime(netStatus.uptimeSec)}`;
  } else {
    $('meta-name').textContent = '—';
    $('meta-id').textContent = '';
    $('meta-uptime').textContent = '';
  }
}

/* ---------------- 概览 ---------------- */

function renderOverview(): void {
  if (!netStatus) {
    $('st-uptime').textContent = '—';
    return;
  }
  const started = new Date(netStatus.startedAt).toLocaleString();
  $('st-uptime').textContent = fmtUptime(netStatus.uptimeSec);
  $('st-started').textContent = `启动于 ${started}`;
  $('st-traffic').textContent = `${fmtBytes(netStatus.traffic.totalIn)} / ${fmtBytes(netStatus.traffic.totalOut)}`;
  $('st-traffic-peers').textContent = `${netStatus.traffic.peers.length} 个对端`;
  $('st-requests').textContent = String(netStatus.requests);
  $('st-requests-hint').textContent = `HTTP :${netStatus.httpPort} · QUIC :${netStatus.quicPort}`;
  const connUplinks = netStatus.uplinks.filter((u) => u.status === 'connected').length;
  $('st-net').textContent = `${connUplinks}/${netStatus.uplinks.length} 网络`;
  $('st-net-hint').textContent = `成员 ${netStatus.members.length} · 中转${netStatus.hub ? '中' : '空闲'}${netStatus.joined ? '' : ' · 未加入'}`;

  const folders = $('ov-folders');
  folders.replaceChildren();
  if (!netStatus.folders.length) folders.append(h('li', { class: 'muted' }, '（无 —— 纯中转模式）'));
  for (const f of netStatus.folders) folders.append(h('li', null, f.path));

  const kindsBody = $('ov-kinds').querySelector('tbody')!;
  kindsBody.replaceChildren();
  if (!netStatus.kinds.length) kindsBody.append(h('tr', null, h('td', { colspan: '6', class: 'muted' }, '（暂无访问）')));
  for (const k of netStatus.kinds) {
    const avg = k.count ? Math.round(k.durMs / k.count) : 0;
    kindsBody.append(h('tr', null,
      h('td', null, k.kind), h('td', null, String(k.count)),
      h('td', null, String(k.ok)), h('td', null, String(k.fail)),
      h('td', null, k.bytes ? fmtBytes(k.bytes) : '—'),
      h('td', null, k.count ? `${avg}ms` : '—'),
    ));
  }

  const membersBody = $('ov-members').querySelector('tbody')!;
  membersBody.replaceChildren();
  if (!netStatus.members.length) membersBody.append(h('tr', null, h('td', { colspan: '4', class: 'muted' }, '（暂无成员）')));
  for (const m of netStatus.members) {
    membersBody.append(h('tr', null,
      h('td', null, m.name || '节点 ' + m.id.slice(0, 8)),
      h('td', null, m.addr || '—'),
      h('td', null, m.direct ? '直连' : '中继'),
      h('td', null, m.local ? '🖥 本机' : ''),
    ));
  }
}

/* ---------------- 访问记录 ---------------- */

async function refreshAccess(): Promise<void> {
  if (tab !== 'access' || !online) return;
  let entries: AccessEntry[] = [];
  try {
    entries = (await pfetch('/api/v1/admin/access?limit=100')).entries as AccessEntry[];
  } catch { return; }
  const body = $('access-table').querySelector('tbody')!;
  body.replaceChildren();
  if (!entries.length) body.append(h('tr', null, h('td', { colspan: '8', class: 'muted' }, '（暂无访问记录）')));
  for (const e of entries) {
    body.append(h('tr', { class: e.ok ? '' : 'fail' },
      h('td', null, new Date(e.ts).toLocaleTimeString()),
      h('td', null, e.ok ? '✓' : '✗'),
      h('td', null, e.kind),
      h('td', null, shortID(e.peer)),
      h('td', { class: 'path' }, e.path || '—'),
      h('td', null, e.bytes ? fmtBytes(e.bytes) : '—'),
      h('td', null, e.durMs ? `${e.durMs}ms` : '—'),
      h('td', null, e.extra || ''),
    ));
  }
}

/* ---------------- 流量 ---------------- */

function renderTraffic(): void {
  if (!netStatus) return;
  $('tf-in').textContent = fmtBytes(netStatus.traffic.totalIn);
  $('tf-out').textContent = fmtBytes(netStatus.traffic.totalOut);
  const body = $('traffic-table').querySelector('tbody')!;
  body.replaceChildren();
  if (!netStatus.traffic.peers.length) body.append(h('tr', null, h('td', { colspan: '3', class: 'muted' }, '（暂无流量）')));
  for (const p of netStatus.traffic.peers) {
    body.append(h('tr', null,
      h('td', null, shortID(p.peer)),
      h('td', null, fmtBytes(p.in)),
      h('td', null, fmtBytes(p.out)),
    ));
  }
}

/* ---------------- 管理 ---------------- */

// refreshServiceState 服务注册状态（任何页签都要刷新：离线横幅按钮依赖它）
async function refreshServiceState(): Promise<void> {
  try { ctl = await ctlFetch('/ctl/config') as CtlConfig; } catch { return; }
  const { installed, running } = ctl.service;
  const svcEl = $('mg-svc-state');
  svcEl.textContent = installed ? (running ? '运行中' : '已注册 · 未运行') : '未注册';
  svcEl.className = 'pill ' + (running ? 'ok' : installed ? 'warn' : '');
  $('mg-svc-note').textContent =
    `CLI: ${ctl.exe} · 注册/启停需要管理员权限（会弹 UAC）`;
  $('btn-svc-start').hidden = !installed;
  $('offline-hint').textContent = installed ? '服务已注册但未运行。' : '服务未注册，可前台启动或先注册系统服务。';
}

async function refreshManage(): Promise<void> {
  if (tab !== 'manage') return;
  renderFolders();
  renderUplinks();
  await refreshServiceState();
}

function renderFolders(): void {
  const ul = $('mg-folders');
  ul.replaceChildren();
  const folders = netStatus?.folders || [];
  if (!folders.length) ul.append(h('li', { class: 'muted' }, '（无共享文件夹）'));
  for (const f of folders) {
    const rm = h('button', { class: 'btn mini danger', title: '移除' }, '✕');
    rm.onclick = () => void setFolders(folders.filter((x) => x.path !== f.path).map((x) => x.path));
    ul.append(h('li', null, h('span', null, f.path), rm));
  }
}

async function setFolders(paths: string[]): Promise<void> {
  try {
    const r = await pfetch('/api/config/paths', { method: 'POST', body: JSON.stringify({ paths }) });
    for (const w of r.warns || []) toast(w);
    toast('文件夹已更新（热生效）');
  } catch (e) {
    toast('更新失败: ' + (e as Error).message);
  }
  await refreshStatus();
}

function renderUplinks(): void {
  const ul = $('mg-uplinks');
  ul.replaceChildren();
  const uplinks = netStatus?.uplinks || [];
  if (!uplinks.length) ul.append(h('li', { class: 'muted' }, '（未加入任何网络）'));
  for (const u of uplinks) {
    const rm = h('button', { class: 'btn mini danger', title: '断开' }, '✕');
    rm.onclick = () => void (async () => {
      try {
        await pfetch('/api/v1/admin/hub/remove', { method: 'POST', body: JSON.stringify({ addr: u.addr }) });
        toast(`已离开 ${u.addr}`);
      } catch (e) { toast('失败: ' + (e as Error).message); }
      await refreshStatus();
    })();
    ul.append(h('li', null,
      h('span', null, u.addr, ' ', h('span', { class: 'muted' }, u.status === 'connected' ? '· 已连接' : '· 未连接')),
      rm));
  }
}

async function serviceAction(action: string): Promise<void> {
  toast(`正在${action}…`);
  try {
    const r = await ctlFetch('/ctl/service', { method: 'POST', body: JSON.stringify({ action }) });
    toast(r.message || '已执行');
  } catch (e) {
    toast('失败: ' + (e as Error).message);
  }
  await new Promise((res) => setTimeout(res, 1500));
  await refreshManage();
}

/* ---------------- Windows 防火墙 ---------------- */

// refreshFirewall 拉取防火墙放行状态（本机 COM 检测，daemon 没跑也能查）。
async function refreshFirewall(): Promise<void> {
  try { fw = await ctlFetch('/ctl/firewall') as FirewallStatus; } catch { fw = null; }
  renderFirewall();
  renderFwBanner();
}

function netLabel(f: FirewallStatus): string {
  return (f.activeProfiles || []).join('、') || '—';
}

function renderFirewall(): void {
  const pill = $('mg-fw-state');
  const btn = $('mg-fw-allow') as HTMLButtonElement;
  const ul = $('mg-fw-rows');
  const note = $('mg-fw-note');
  if (!fw || !fw.supported) {
    pill.textContent = '—';
    pill.className = 'pill';
    btn.hidden = true;
    ul.replaceChildren();
    note.textContent = fw ? (fw.reason || '检测不可用') : '检测不可用';
    return;
  }
  ul.replaceChildren();
  for (const n of fw.needs) {
    ul.append(h('li', { class: n.allowed ? '' : 'fail' },
      h('span', null, `${n.allowed ? '✓' : '✗'} ${n.proto.toUpperCase()}${n.port ? ' ' + n.port : ''} `, h('span', { class: 'muted' }, n.desc)),
      h('span', { class: 'muted' }, n.blockBy ? `被拦截：${n.blockBy}` : n.ruleName || ''),
    ));
  }
  if (!fw.firewallOn) {
    pill.textContent = '防火墙未启用';
    pill.className = 'pill ok';
    btn.hidden = true;
    note.textContent = `当前网络：${netLabel(fw)}，无拦截。`;
    return;
  }
  if (fw.allowed) {
    pill.textContent = '已放行';
    pill.className = 'pill ok';
    btn.hidden = true;
    note.textContent = `当前网络：${netLabel(fw)}。规则按「程序 + 协议」放行（不限端口），改端口后依然有效。`;
    return;
  }
  pill.textContent = '未放行';
  pill.className = 'pill warn';
  btn.hidden = false;
  btn.disabled = fwAllowing;
  btn.textContent = fwAllowing ? '等待 UAC 确认…' : '一键放行…';
  note.textContent = `当前网络：${netLabel(fw)}。不放行时局域网设备无法访问服务端口（本机能访问是因为回环流量不经过防火墙）。点击会弹 UAC 授权。`;
}

function renderFwBanner(): void {
  const show = !!(fw && fw.supported && fw.firewallOn && !fw.allowed && !fwDismissed);
  $('fw-banner').hidden = !show;
  const btn = $('fw-allow') as HTMLButtonElement;
  btn.disabled = fwAllowing;
  btn.textContent = fwAllowing ? '等待 UAC 确认…' : '一键放行…';
}

// allowFirewall 一键放行：POST 提权请求（弹 UAC），随后轮询状态确认生效。
async function allowFirewall(): Promise<void> {
  if (fwAllowing) return;
  fwAllowing = true;
  renderFirewall();
  renderFwBanner();
  toast('已请求提权，请在 UAC 弹窗中确认');
  try {
    await ctlFetch('/ctl/firewall/allow', { method: 'POST', body: '{}' });
  } catch (e) {
    toast('放行失败: ' + (e as Error).message);
    fwAllowing = false;
    renderFirewall();
    renderFwBanner();
    return;
  }
  for (let i = 0; i < 20; i++) {
    await new Promise((res) => setTimeout(res, 1500));
    try { fw = await ctlFetch('/ctl/firewall') as FirewallStatus; } catch { continue; }
    if (fw.allowed) break;
  }
  fwAllowing = false;
  if (fw?.allowed) toast('✅ 已放行，局域网设备现在可以访问了');
  else toast('未见生效：可能在 UAC 弹窗点了取消，请重试');
  renderFirewall();
  renderFwBanner();
}

/* ---------------- 事件绑定 ---------------- */

function bindEvents(): void {
  for (const btn of document.querySelectorAll('#tabs button')) {
    (btn as HTMLElement).onclick = () => {
      tab = (btn as HTMLElement).dataset.tab!;
      for (const b of document.querySelectorAll('#tabs button')) b.classList.toggle('active', b === btn);
      for (const s of document.querySelectorAll('.tab')) s.classList.toggle('active', s.id === 'tab-' + tab);
      if (tab === 'access') void refreshAccess();
      if (tab === 'manage') void refreshManage();
      if (tab === 'traffic') renderTraffic();
      if (tab === 'overview') renderOverview();
    };
  }
  // 浏览…：弹原生文件夹选择框，选中即添加（取消则无动作）
  $('mg-folder-browse').onclick = () => {
    void (async () => {
      try {
        const r = await ctlFetch('/ctl/browse', { method: 'POST', body: '{}' });
        if (r.canceled || !r.path) return;
        const cur = (netStatus?.folders || []).map((f) => f.path);
        if (cur.includes(r.path)) { toast('该文件夹已在共享列表'); return; }
        await setFolders([...cur, r.path]);
        toast(`已添加 ${r.path}`);
      } catch (e) {
        toast('打开文件夹选择器失败，请手动输入路径：' + (e as Error).message);
      }
    })();
  };
  $('mg-folder-add').onclick = () => {
    const input = $('mg-folder-input') as HTMLInputElement;
    const p = input.value.trim();
    if (!p) return;
    const cur = (netStatus?.folders || []).map((f) => f.path);
    if (cur.includes(p)) { toast('该文件夹已存在'); return; }
    input.value = '';
    void setFolders([...cur, p]);
  };
  $('mg-join').onclick = () => {
    const input = $('mg-join-input') as HTMLInputElement;
    const addr = input.value.trim();
    if (!addr) return;
    const msg = $('mg-net-msg');
    msg.textContent = '加入中…';
    void (async () => {
      try {
        const r = await pfetch('/api/net/join', { method: 'POST', body: JSON.stringify({ addrs: [addr] }) });
        const res = r.results?.[0];
        msg.textContent = res ? (res.ok ? `✓ ${res.addr} → ${res.node}${res.note ? '（' + res.note + '）' : ''}` : `✗ ${res.error}`) : '';
      } catch (e) { msg.textContent = '失败: ' + (e as Error).message; }
      input.value = '';
      await refreshStatus();
    })();
  };
  $('mg-leave-all').onclick = () => {
    if (!confirm('确定退出全部网络？')) return;
    void (async () => {
      try {
        await pfetch('/api/net/leave', { method: 'POST', body: '{}' });
        toast('已退出全部网络');
      } catch (e) { toast('失败: ' + (e as Error).message); }
      await refreshStatus();
    })();
  };
  $('mg-svc-install').onclick = () => void serviceAction('install');
  $('mg-svc-start').onclick = () => void serviceAction('start');
  $('mg-svc-stop').onclick = () => void serviceAction('stop');
  $('mg-svc-uninstall').onclick = () => void serviceAction('uninstall');
  $('mg-fw-allow').onclick = () => void allowFirewall();
  $('fw-allow').onclick = () => void allowFirewall();
  $('fw-dismiss').onclick = () => { fwDismissed = true; renderFwBanner(); };
  $('btn-spawn').onclick = () => {
    void (async () => {
      try {
        const r = await ctlFetch('/ctl/daemon/start', { method: 'POST', body: '{}' });
        toast(r.message || '已启动');
      } catch (e) { toast('失败: ' + (e as Error).message); }
    })();
  };
  $('btn-svc-start').onclick = () => void serviceAction('start');
}

/* ---------------- 启动 ---------------- */

async function main(): Promise<void> {
  bindEvents();
  try { ctl = await ctlFetch('/ctl/config') as CtlConfig; } catch { /* 忽略 */ }
  await refreshStatus();
  await refreshServiceState();
  await refreshFirewall();
  window.setInterval(() => void refreshStatus(), 5000);
  window.setInterval(() => void refreshAccess(), 5000);
  window.setInterval(() => void refreshServiceState(), 8000);
  window.setInterval(() => void refreshFirewall(), 8000);
}

void main();
