// ⚙ 设置面板：外观（深色模式）、图库文件夹管理、网络缓存文件夹、Windows 防火墙。
import { api, ApiError } from './api';
import { $, clear, h } from './dom';
import { goHome, refreshNetState, state } from './app';
import type { FirewallStatus } from './types';

export function isOpen(): boolean {
  return !$('settings-panel').hidden;
}

export function open(): void {
  ($('settings-panel') as HTMLElement).hidden = false;
  ($('settings-backdrop') as HTMLElement).hidden = false;
  void render();
}

export function close(): void {
  ($('settings-panel') as HTMLElement).hidden = true;
  ($('settings-backdrop') as HTMLElement).hidden = true;
}

function msg(text: string): void {
  $('st-msg').textContent = text;
}

/* ---------------- 深色模式 ---------------- */

const THEME_KEY = 'lotus-theme';

export function applyTheme(theme: 'light' | 'dark'): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(THEME_KEY, theme);
  } catch { /* 忽略 */ }
  const btn = $('theme-toggle');
  if (btn) btn.textContent = theme === 'dark' ? '☀️ 浅色' : '🌙 深色';
  const sw = $('st-dark') as HTMLInputElement | null;
  if (sw) sw.checked = theme === 'dark';
}

export function currentTheme(): 'light' | 'dark' {
  return document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light';
}

/* ---------------- 渲染 ---------------- */

export async function render(): Promise<void> {
  applyTheme(currentTheme());
  void renderFirewall();
  try {
    const cfg = await api.configGet();
    // 编辑入口隐藏的两种情形：固定根（旧移动端）与消费者模式（服务端判定只读）
    const fixed = cfg.fixedRoots === true || cfg.writable === false;
    ($('st-path-row') as HTMLElement).hidden = fixed;
    ($('st-path-hint') as HTMLElement).hidden = fixed;
    ($('st-fixed-hint') as HTMLElement).hidden = !fixed;
    ($('st-cachedir-row') as HTMLElement).hidden = fixed;
    ($('st-cachedir-open') as HTMLElement).hidden = fixed;
    renderPaths(cfg.paths, fixed);
    ($('st-cachedir') as HTMLInputElement).value = cfg.cacheDir;
    $('st-cachedir-now').textContent = cfg.cacheDir;
  } catch (e) {
    msg('读取配置失败：' + (e instanceof ApiError ? e.message : String(e)));
  }
}

function renderPaths(paths: string[], fixed: boolean): void {
  const box = $('st-paths');
  clear(box);
  for (const p of paths) {
    const item = h('div', { class: 'np-item' },
      h('span', { class: 'grow ellip', title: p }, p));
    if (!fixed) {
      item.append(h('button', {
        class: 'mini', title: '在资源管理器中打开',
        onclick: () => void api.openFolder(p).catch(() => {}),
      }, '📂'));
      item.append(h('button', {
        class: 'mini danger',
        onclick: async () => {
          const next = paths.filter((x) => x !== p);
          await savePaths(next);
        },
      }, '移除'));
    }
    box.append(item);
  }
  if (!paths.length) {
    box.append(h('div', { class: 'np-hint' }, '还没有图库文件夹。'));
  }
}

async function savePaths(paths: string[]): Promise<void> {
  try {
    const r = await api.configSetPaths(paths);
    await render();
    msg((r.warns || []).join('\n'));
    void refreshNetState();
    goHome(); // 刷新根视图卡片
  } catch (e) {
    msg('保存失败：' + (e instanceof ApiError ? e.message : String(e)));
  }
}

/* ---------------- Windows 防火墙 ---------------- */

let fw: FirewallStatus | null = null;
let fwAllowing = false;

async function renderFirewall(): Promise<void> {
  if (!state.writable) { // 消费者模式（他人浏览器/移动端连服务端）：不显示
    $('st-fw').hidden = true;
    return;
  }
  try {
    fw = await api.firewallStatus();
  } catch {
    fw = null; // 旧版服务端无此端点等情形：整区隐藏
  }
  renderFirewallView();
}

function renderFirewallView(): void {
  const sec = $('st-fw');
  if (!fw || !fw.supported) {
    sec.hidden = true;
    return;
  }
  sec.hidden = false;
  const netName = (fw.activeProfiles || []).join('、') || '当前网络';
  const rows = $('st-fw-rows');
  clear(rows);
  for (const n of fw.needs) {
    rows.append(h('div', { class: 'np-item' },
      h('span', n.allowed ? {} : { style: 'color: var(--danger)' },
        `${n.allowed ? '✓' : '✗'} ${n.proto.toUpperCase()}${n.port ? ' ' + n.port : ''}`),
      h('span', { class: 'np-hint grow ellip', title: n.desc }, n.desc)));
  }
  const btnRow = $('st-fw-btnrow');
  const btn = $('st-fw-allow') as HTMLButtonElement;
  const hint = $('st-fw-hint');
  if (!fw.firewallOn) {
    btnRow.hidden = true;
    hint.textContent = `防火墙未启用（${netName}），无拦截。`;
    return;
  }
  if (fw.allowed) {
    btnRow.hidden = true;
    hint.textContent = `已放行本程序的局域网入站（${netName}）。`;
    return;
  }
  btnRow.hidden = false;
  btn.disabled = fwAllowing;
  btn.textContent = fwAllowing ? '等待 UAC 确认…' : '允许局域网直连…';
  hint.textContent = `${netName} 的 Windows 防火墙未放行本程序，局域网内其他设备无法直连（本机能访问是因为回环流量不经过防火墙）。点击放行会弹 UAC 授权，规则按「程序 + 协议」添加。`;
}

async function allowFirewall(): Promise<void> {
  if (fwAllowing) return;
  fwAllowing = true;
  renderFirewallView();
  try {
    await api.firewallAllow();
  } catch (e) {
    fwAllowing = false;
    msg('放行失败：' + (e instanceof ApiError ? e.message : String(e)));
    renderFirewallView();
    return;
  }
  // 提权子进程完成后规则生效，轮询确认
  for (let i = 0; i < 20; i++) {
    await new Promise((res) => setTimeout(res, 1500));
    try { fw = await api.firewallStatus(); } catch { continue; }
    renderFirewallView();
    if (fw.allowed) {
      fwAllowing = false;
      msg('✅ 已放行，局域网设备现在可以直连了');
      void refreshNetState();
      return;
    }
  }
  fwAllowing = false;
  msg('未见生效：可能在 UAC 弹窗点了取消，请重试');
  renderFirewallView();
}

/* ---------------- 事件绑定 ---------------- */

export function bindSettings(): void {
  $('settings-btn').addEventListener('click', () => (isOpen() ? close() : open()));
  $('st-close').addEventListener('click', close);
  $('settings-backdrop').addEventListener('click', close);

  $('theme-toggle').addEventListener('click', () =>
    applyTheme(currentTheme() === 'dark' ? 'light' : 'dark'));
  $('st-dark').addEventListener('change', (e) => {
    applyTheme((e.target as HTMLInputElement).checked ? 'dark' : 'light');
  });

  $('st-path-add').addEventListener('click', async () => {
    const input = $('st-path-new') as HTMLInputElement;
    const p = input.value.trim();
    if (!p) return;
    const cfg = await api.configGet().catch(() => null);
    const paths = cfg ? cfg.paths.slice() : [];
    if (paths.some((x) => x.toLowerCase() === p.toLowerCase())) {
      msg('该文件夹已在列表中');
      return;
    }
    paths.push(p);
    input.value = '';
    await savePaths(paths);
  });
  $('st-path-new').addEventListener('keydown', (e) => {
    if ((e as KeyboardEvent).key === 'Enter') ($('st-path-add') as HTMLButtonElement).click();
    e.stopPropagation();
  });

  $('st-cachedir-save').addEventListener('click', async () => {
    const dir = ($('st-cachedir') as HTMLInputElement).value.trim();
    if (!dir) return;
    try {
      const r = await api.configSetCacheDir(dir);
      $('st-cachedir-now').textContent = r.cacheDir;
      msg('✅ 缓存文件夹已更新：' + r.cacheDir);
    } catch (e) {
      msg('保存失败：' + (e instanceof ApiError ? e.message : String(e)));
    }
  });
  $('st-cachedir-open').addEventListener('click', async () => {
    const dir = ($('st-cachedir-now').textContent || '').trim();
    if (dir) await api.openFolder(dir).catch(() => {});
  });

  $('st-fw-allow').addEventListener('click', () => void allowFirewall());
}
