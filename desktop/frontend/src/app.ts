// 应用状态与浏览逻辑（对应原版 load/loadRemote/applyFolderView/网络轮询）。
import { api, ApiError } from './api';
import { $, clear, fuzzyMatch, h, zhCollate } from './dom';
import type { BrowseData, Entry, Loc, NetState, SortMode } from './types';

export interface FolderItem {
  name: string;
  path: string;
  mtime?: number;
  kind: 'local-root' | 'folder' | 'member' | 'cached' | 'visited';
  nodeId?: string; // member/cached/visited
  nodeAddr?: string;
  nodeName?: string; // visited
  nodeRoot?: string; // visited：对方根路径
  nodeFiles?: number; // cached/visited
  offline?: boolean;
  isLocal?: boolean; // 本机服务（同机 lotus-server）：节点卡片不再渲染（其根已并入本机）
  svc?: boolean; // 本机服务的根（并入本机图库、直接读盘）：图标区分
}

export const state = {
  loc: null as Loc | null,
  data: null as BrowseData | null,
  netState: null as NetState | null,
  folderSort: 'name' as SortMode,
  imageSort: 'name' as SortMode,
  filterText: '',
  mode: 'files' as 'files' | 'images',
  // 消费者模式：服务端判定不可管理（GET /api/config 的 writable=false）。
  // 隐藏全部管理入口（网络面板、配置编辑），viewer 用内存窗口加载图片。
  writable: true,
  // 当前视图按筛选过滤后是否一条不剩（refreshLayout 据此显示"没有匹配"空态）
  folderViewEmpty: false,
  // 预取跟踪：当前激活的远程文件夹 {node, folder}
  remoteActive: null as { node: string; folder: string } | null,
};

/* ---------------- 主页根分区（本机 / 网络·其他人） ----------------
 * 网络根可"按用户分组"或"平铺"；网络分区与用户分组均可折叠。
 * 折叠与显示模式持久化到 localStorage；筛选时忽略折叠，避免"搜到了却看不见"。 */
type RootsNetMode = 'grouped' | 'flat';

const ROOTS_NETMODE_KEY = 'lotus-roots-netmode';
const ROOTS_NET_COLLAPSED_KEY = 'lotus-roots-net-collapsed';
const ROOTS_USER_COLLAPSED_KEY = 'lotus-roots-user-collapsed';

function loadSet(key: string): Set<string> {
  try { return new Set(JSON.parse(localStorage.getItem(key) || '[]') as string[]); }
  catch { return new Set(); }
}

export const rootsUi = {
  netMode: (localStorage.getItem(ROOTS_NETMODE_KEY) === 'flat' ? 'flat' : 'grouped') as RootsNetMode,
  netCollapsed: localStorage.getItem(ROOTS_NET_COLLAPSED_KEY) === '1',
  userCollapsed: loadSet(ROOTS_USER_COLLAPSED_KEY),
};

function saveRootsUi(): void {
  try {
    localStorage.setItem(ROOTS_NETMODE_KEY, rootsUi.netMode);
    localStorage.setItem(ROOTS_NET_COLLAPSED_KEY, rootsUi.netCollapsed ? '1' : '0');
    localStorage.setItem(ROOTS_USER_COLLAPSED_KEY, JSON.stringify([...rootsUi.userCollapsed]));
  } catch { /* 存储不可用时仅在会话内生效 */ }
}

export type BusyKind = 'browse' | 'times' | 'net';
const busyCount: Record<BusyKind, number> = { browse: 0, times: 0, net: 0 };

export function setBusy(kind: BusyKind, on: boolean, text?: string): void {
  if (kind === 'net') {
    // 网络同步是单一指示（每秒轮询刷新文案），幂等设置避免计数泄漏
    busyCount.net = on ? 1 : 0;
  } else {
    busyCount[kind] += on ? 1 : -1;
    if (busyCount[kind] < 0) busyCount[kind] = 0;
  }
  const box = $('busy');
  const label = $('busy-text');
  if (busyCount.browse > 0) {
    label.textContent = '正在读取目录…';
    box.hidden = false;
  } else if (busyCount.net > 0) {
    label.textContent = text || '正在同步远程图片…';
    box.hidden = false;
  } else if (busyCount.times > 0) {
    label.textContent = '正在读取修改时间…';
    box.hidden = false;
  } else {
    box.hidden = true;
  }
}

export function sortCompare(sort: SortMode): (a: Entry, b: Entry) => number {
  if (sort === 'time-desc') return (a, b) => ((b.mtime || 0) - (a.mtime || 0)) || zhCollate(a.name, b.name);
  if (sort === 'time-asc') return (a, b) => ((a.mtime || 0) - (b.mtime || 0)) || zhCollate(a.name, b.name);
  return (a, b) => zhCollate(a.name, b.name);
}

/* ---------------- 滚动记忆：返回/重访回到离开时的位置 ----------------
 * main.ts 把 #main 的滚动位置持续写入这里（key = 位置标识）；
 * navigate 渲染完成后若该位置有记录则恢复。浏览器自带的同文档滚动恢复
 * 对 SPA 无效（内容重建会归零），故 scrollRestoration 设为 manual。 */
const scrollMemory = new Map<string, number>();

export function locKey(loc: Loc | null): string {
  if (!loc) return '';
  return (loc.remote && loc.node ? loc.node : '') + '|' + (loc.path || '');
}

export function rememberScroll(key: string, top: number): void {
  if (key) scrollMemory.set(key, top);
}

/* ---------------- 筛选/视图模式记忆：返回/重访恢复离开时的筛选 ----------------
 * 与滚动记忆同理：筛选词与文件/图片模式按位置记录（navigate 离开时写入），
 * navigate 渲染时恢复；首次访问为空筛选 + 默认模式。会话内有效。 */
const filterMemory = new Map<string, string>();
const modeMemory = new Map<string, 'files' | 'images'>();

/* ---------------- 导航 ---------------- */

let navGen = 0;

// SPA 历史集成：成功导航写入浏览器历史（安卓返回键/左划逐级回退，根视图再退才离开）。
// 首次用 replaceState 占住初始条目，之后的导航 pushState；popstate 回退时不重复写入。
let historySeeded = false;

export function goHome(): void {
  void navigate({ remote: false, path: '' });
}

/** 统一「返回上一级」动作（FAB/空态按钮/键盘 Backspace）：有筛选先清筛选留在
 * 本页，筛选已空才真正回退——与安卓返回键/右滑的历史哨兵行为保持一致。 */
export function goBack(): void {
  if (state.filterText) {
    clearFilterLocal();
    syncFilterHistory();
    return;
  }
  goUp();
}

export function goUp(): void {
  const { loc, data } = state;
  if (!loc || !data) return;
  if (loc.remote) {
    void navigate({ remote: true, node: loc.node, nodeName: loc.nodeName, path: data.parent || '' });
  } else if (data.parent) {
    void navigate({ remote: false, path: data.parent });
  } else {
    goHome();
  }
}

export function openFolder(path: string): void {
  const { loc } = state;
  if (!loc) return;
  if (loc.remote) {
    void navigate({ remote: true, node: loc.node, nodeName: loc.nodeName, path });
  } else {
    void navigate({ remote: false, path });
  }
}

export function openNode(nodeId: string, nodeName?: string): void {
  void navigate({ remote: true, node: nodeId, nodeName, path: '' });
}

export async function navigate(loc: Loc, fromPopstate = false): Promise<void> {
  const gen = ++navGen;
  // 离开当前位置前：记下该位置的筛选词与视图模式（返回/重访时恢复）
  const fromKey = locKey(state.loc);
  if (fromKey) {
    filterMemory.set(fromKey, state.filterText);
    modeMemory.set(fromKey, state.mode);
  }
  leaveRemoteFolder();
  setBusy('browse', true);
  try {
    let data: BrowseData;
    if (loc.remote && loc.node) {
      data = await api.netBrowse(loc.node, loc.path);
      const member = state.netState?.members.find((m) => m.id === loc.node);
      if (!loc.nodeName) {
        loc.nodeName = member?.name || (state.netState?.cached.find((c) => c.id === loc.node)?.name ?? '');
      }
    } else if (!loc.path) {
      const r = await api.roots();
      data = {
        name: 'roots', path: '', parent: null, isRoots: true,
        breadcrumbs: [],
        folders: r.roots.map((r2) => ({ name: r2.name, path: r2.path, mtime: r2.mtime, service: r2.service === true })),
        images: [],
      };
    } else {
      data = await api.browse(loc.path);
    }
    if (gen !== navGen) return;
    window.appViewer.exitFs?.(); // 导航即离开阅读视图：全屏态先退出，避免 fs-active 残留把新页面的头部吞掉
    state.loc = loc;
    state.data = data;
    rootsViewSig = ""; // 导航后强制重绘一次根视图
    // 恢复该位置的筛选词与视图模式（首次访问：空筛选 + 默认模式；根视图恒为文件模式）
    const key = locKey(loc);
    const savedFilter = filterMemory.get(key) ?? '';
    state.filterText = savedFilter;
    ($('filter') as HTMLInputElement).value = savedFilter;
    const savedMode = modeMemory.get(key);
    const mode: 'files' | 'images' =
      savedMode === 'files' || (savedMode === 'images' && data.images.length > 0)
        ? savedMode
        : (data.images.length > 0 ? 'images' : 'files');
    setMode(data.isRoots ? 'files' : mode);
    document.title = data.isRoots ? '图片浏览服务'
      : `${data.name}${data.offline ? ' 📴 离线缓存' : loc.remote ? ' 🌐 ' + (loc.nodeName || '远程') : ''}`;
    renderBreadcrumb();
    applyFolderView();
    window.appViewer.apply();
    refreshLayout();
    $('error-box').hidden = true;
    // 返回/重访：内容已同步重建，下一帧恢复离开时的滚动位置
    const savedTop = scrollMemory.get(locKey(loc));
    if (savedTop !== undefined) {
      requestAnimationFrame(() => { ($('main') as HTMLElement).scrollTop = savedTop; });
    }
    // 历史条目：成功导航才写（popstate 回退不重复写；首条 replace 保住 ?server= 参数）
    if (!fromPopstate) {
      if (historySeeded) history.pushState({ lotusLoc: loc }, '');
      else { history.replaceState({ lotusLoc: loc }, ''); historySeeded = true; }
      onFilterEntry = false; // 新条目是纯位置条目，旧的筛选哨兵已留在下面
      syncFilterHistory(); // 恢复了非空筛选的位置（前进进入）同样压入哨兵，返回时先清筛选
    }
    if (!data.isRoots) void loadTimes();
    // 远程文件夹：自动批量预取
    if (loc.remote && loc.node && loc.path && data.images.length) {
      enterRemoteFolder(loc.node, data.path || loc.path, data.images);
    }
  } catch (e) {
    if (gen !== navGen) return;
    state.loc = null;
    state.data = null;
    const stack = e instanceof Error && e.stack ? '\n[stack] ' + e.stack.split('\n').slice(0, 6).join('\n') : '';
    $('error-msg').textContent = '加载失败：' + (e instanceof ApiError ? e.message : String(e)) + stack;
    $('error-box').hidden = false;
    clear($('folder-grid'));
    window.appViewer.apply();
    refreshLayout();
  } finally {
    if (gen === navGen) setBusy('browse', false);
  }
}

/* ---------------- 筛选态与浏览器历史整合：返回先清筛选、再回退 ----------------
 * 筛选词非空时压入一条哨兵历史条目（lotusFlt）。安卓返回键/右滑手势与浏览器
 * 后退都先弹掉它——仅清空筛选、留在本页（不跳页）；筛选已空才真正回退上一页。
 * 手动清空筛选（✕/Esc/Backspace）时主动弹回哨兵，保证之后的返回不空按一档。 */
let onFilterEntry = false; // 当前历史条目是否为筛选哨兵
let pendingConsume = false; // 已发起 history.back() 消费哨兵，等 popstate 落地

/** 清空当前页筛选（不改历史；调用方决定是否跟着 syncFilterHistory）。 */
export function clearFilterLocal(): void {
  state.filterText = '';
  ($('filter') as HTMLInputElement).value = '';
  if (state.loc) filterMemory.set(locKey(state.loc), '');
  applyFolderView();
  refreshLayout();
}

/** 筛选词变化后同步哨兵条目（输入路径与导航路径都走这里）。 */
export function syncFilterHistory(): void {
  if (state.loc) filterMemory.set(locKey(state.loc), state.filterText);
  const has = !!state.filterText.trim();
  if (has && !onFilterEntry && !pendingConsume && historySeeded && state.loc) {
    history.pushState({ lotusLoc: state.loc, lotusFlt: 1 }, '');
    onFilterEntry = true;
  } else if (!has && onFilterEntry && !pendingConsume) {
    pendingConsume = true;
    onFilterEntry = false;
    history.back();
  }
}

/** popstate 统一入口（main.ts 注册）：区分「消费筛选哨兵」与「页面回退/前进」。 */
export function onPopstate(e: PopStateEvent): void {
  const st = e.state as { lotusLoc?: Loc; lotusFlt?: number } | null;
  if (pendingConsume) {
    // 手动清筛选引发的哨兵弹回：条目消费掉即可，页面内容不动
    pendingConsume = false;
    onFilterEntry = !!st?.lotusFlt;
    syncFilterHistory(); // 消费期间若又输入了新筛选，此处补压哨兵
    return;
  }
  if (
    onFilterEntry && st?.lotusLoc && !st.lotusFlt &&
    locKey(st.lotusLoc) === locKey(state.loc)
  ) {
    // 筛选态按返回：弹出的正是本页哨兵 → 只清筛选，留在本页
    onFilterEntry = false;
    clearFilterLocal();
    return;
  }
  onFilterEntry = !!st?.lotusFlt;
  if (st?.lotusLoc) void navigate(st.lotusLoc, true);
}

/* ---------------- 面包屑 ---------------- */

function renderBreadcrumb(): void {
  const { loc, data } = state;
  const bar = $('breadcrumbs');
  clear(bar);
  if (!loc || !data) return;
  bar.append(h('span', {
    class: 'crumb clickable',
    onclick: () => goHome(),
  }, '🏠 根目录'));
  if (loc.remote && loc.node) {
    const nodeName = '🌐 ' + (loc.nodeName || '节点 ' + loc.node.slice(0, 8));
    if (!loc.path) {
      bar.append(h('span', { class: 'crumb active' }, nodeName));
    } else {
      bar.append(h('span', { class: 'crumb clickable', onclick: () => openNode(loc.node!, loc.nodeName) }, nodeName));
    }
  }
  data.breadcrumbs.forEach((c, i) => {
    const last = i === data.breadcrumbs.length - 1;
    bar.append(h('span', {
      class: 'crumb' + (last ? ' active' : ' clickable'),
      onclick: last ? () => {} : () => {
        if (loc.remote) openFolder(c.path);
        else void navigate({ remote: false, path: c.path });
      },
    }, c.name));
  });
}

/* ---------------- 文件夹视图（根分区卡片 / 文件夹网格） ---------------- */

function folderCard(it: FolderItem): HTMLElement {
  const sub = it.kind === 'local-root' ? it.path
    : it.kind === 'member' ? (it.nodeAddr || it.nodeId)
    : it.kind === 'cached' ? `📴 离线缓存 · ${it.nodeFiles ?? 0} 张可看`
    : it.kind === 'visited' ? `${it.offline ? '📴' : '🌐'} ${it.nodeName} · 已缓存 ${it.nodeFiles ?? 0} 张`
    : '';
  return h('div', {
    class: 'card' + (it.kind === 'member' || it.kind === 'visited' ? ' net' : '') + (it.kind === 'cached' ? ' cached' : '') + (it.svc ? ' local-svc' : ''),
    onclick: () => {
      if (it.kind === 'visited') openVisited(it.nodeId!, it.nodeName, it.nodeRoot!);
      else if (it.kind === 'member' || it.kind === 'cached') openNode(it.nodeId!);
      else openFolder(it.path);
    },
  },
    h('div', { class: 'card-icon' },
      it.kind === 'member' ? '🌐'
        : it.kind === 'cached' || (it.kind === 'visited' && it.offline) ? '📴'
        : it.kind === 'visited' ? '🌐'
        : it.kind === 'local-root' ? (it.svc ? '🖥' : '🗂') : '📁'),
    h('div', { class: 'card-name' }, it.name),
    sub ? h('div', { class: 'card-sub' }, sub) : null,
  );
}

export function applyFolderView(): void {
  const { data, filterText } = state;
  const grid = $('folder-grid');
  const rootsView = $('roots-view');
  if (!data) { clear(grid); state.folderViewEmpty = false; return; }

  const q = filterText.trim().toLowerCase().replace(/\s+/g, '');
  const match = (it: FolderItem) => !q || fuzzyMatch(it.name.toLowerCase(), q);
  const sortFn = (a: FolderItem, b: FolderItem) => sortCompare(state.folderSort)(a, b);

  if (!data.isRoots) {
    ($('folder-head') as HTMLElement).hidden = false;
    rootsView.hidden = true;
    grid.hidden = false;
    const items = data.folders.map((f) => ({ name: f.name, path: f.path, mtime: f.mtime, kind: 'folder' as const }));
    const view = items.filter(match).sort(sortFn);
    clear(grid);
    for (const it of view) grid.append(folderCard(it));
    $('folder-count').textContent = q ? `${view.length}/${items.length}` : String(items.length);
    state.folderViewEmpty = q ? view.length === 0 : false;
    return;
  }

  // 根视图：本机 / 网络·其他人 两个分区
  ($('folder-head') as HTMLElement).hidden = true;
  grid.hidden = true;
  clear(grid);
  rootsView.hidden = false;
  state.folderViewEmpty = renderRootsView(match, q);
}

/** 渲染根分区视图，返回筛选后是否一条不剩。 */
function renderRootsView(match: (it: FolderItem) => boolean, q: string): boolean {
  const root = $('roots-view');
  clear(root);
  const sortFn = (a: FolderItem, b: FolderItem) => sortCompare(state.folderSort)(a, b);
  const all = buildRootsItems();
  // 本机分区 = 客户端自身根 + 本机服务的根（同机 lotus-server：根已并入本机图库，
  // 直接读盘，不走网络也不缓存；🖥 图标区分服务端根）
  const local = all.filter((it) => it.kind === 'local-root' && match(it)).sort(sortFn);
  const net = all.filter((it) => it.kind !== 'local-root' && match(it)).sort(sortFn);
  // 筛选时忽略折叠状态，保证搜到的东西可见
  const netCollapsed = q ? false : rootsUi.netCollapsed;

  if (local.length) {
    const grid = h('div', { class: 'folder-grid' });
    for (const it of local) grid.append(folderCard(it));
    root.append(h('section', { class: 'roots-group', 'data-group': 'local' },
      h('div', { class: 'section-head roots-head' },
        h('span', { class: 'roots-title' }, '本机'),
        h('span', { class: 'count' }, String(local.length)),
      ),
      grid,
    ));
  }

  if (net.length) {
    const body = h('div', { class: 'roots-body' });
    if (rootsUi.netMode === 'flat') {
      const grid = h('div', { class: 'folder-grid' });
      for (const it of net) grid.append(folderCard(it));
      body.append(grid);
    } else {
      // 按所属用户（节点）分组
      const groups = new Map<string, { label: string; items: FolderItem[] }>();
      for (const it of net) {
        const key = it.nodeId!;
        let g = groups.get(key);
        if (!g) { g = { label: netGroupLabel(it), items: [] }; groups.set(key, g); }
        g.items.push(it);
      }
      const ordered = [...groups.values()].sort((a, b) => zhCollate(a.label, b.label));
      for (const g of ordered) {
        const collapsed = q ? false : rootsUi.userCollapsed.has(g.items[0].nodeId!);
        const grid = h('div', { class: 'folder-grid' });
        for (const it of g.items) grid.append(folderCard(it));
        body.append(h('section', {
          class: 'roots-user' + (collapsed ? ' collapsed' : ''),
          'data-node': g.items[0].nodeId!,
        },
          h('div', { class: 'roots-user-head' },
            h('button', {
              class: 'roots-collapse', title: collapsed ? '展开' : '折叠',
              'data-user-collapse': g.items[0].nodeId!,
            }, collapsed ? '▸' : '▾'),
            h('span', { class: 'roots-user-name' }, g.label),
            h('span', { class: 'count' }, String(g.items.length)),
          ),
          grid,
        ));
      }
    }
    root.append(h('section', {
      class: 'roots-group' + (netCollapsed ? ' collapsed' : ''),
      'data-group': 'net',
    },
      h('div', { class: 'section-head roots-head' },
        h('button', {
          class: 'roots-collapse', title: netCollapsed ? '展开网络根' : '折叠网络根',
          'data-net-collapse': '',
        }, netCollapsed ? '▸' : '▾'),
        h('span', { class: 'roots-title' }, '网络 · 其他人'),
        h('span', { class: 'count' }, String(net.length)),
        h('span', { class: 'sorter roots-netmode' },
          h('button', { 'data-netmode': 'grouped', class: rootsUi.netMode === 'grouped' ? 'active' : '' }, '按用户'),
          h('button', { 'data-netmode': 'flat', class: rootsUi.netMode === 'flat' ? 'active' : '' }, '平铺'),
        ),
      ),
      body,
    ));
  }

  return local.length === 0 && net.length === 0;
}

/** 网络卡片归属的用户显示名（member/cached 的名字即用户名；visited 取 · 后的节点名）。 */
function netGroupLabel(it: FolderItem): string {
  if (it.nodeName) return it.nodeName;
  if (it.kind === 'visited') {
    const parts = it.name.split(' · ');
    return parts[parts.length - 1];
  }
  return it.name;
}

/** 根分区交互（事件委托，main 里 bindToolbar 之前调用一次）。 */
export function bindRootsView(): void {
  $('roots-view').addEventListener('click', (e) => {
    const el = e.target as HTMLElement;
    const modeBtn = el.closest<HTMLButtonElement>('[data-netmode]');
    if (modeBtn) {
      const m = modeBtn.dataset.netmode as RootsNetMode;
      if (m && m !== rootsUi.netMode) { rootsUi.netMode = m; saveRootsUi(); applyFolderView(); }
      return;
    }
    if (el.closest('[data-net-collapse]')) {
      rootsUi.netCollapsed = !rootsUi.netCollapsed;
      saveRootsUi();
      applyFolderView();
      return;
    }
    const userBtn = el.closest<HTMLButtonElement>('[data-user-collapse]');
    if (userBtn) {
      const id = userBtn.dataset.userCollapse!;
      if (rootsUi.userCollapsed.has(id)) rootsUi.userCollapsed.delete(id);
      else rootsUi.userCollapsed.add(id);
      saveRootsUi();
      applyFolderView();
    }
  });
}

function buildRootsItems(): FolderItem[] {
  const items: FolderItem[] = (state.data?.folders || []).map((f) => ({
    name: f.name, path: f.path, mtime: f.mtime, kind: 'local-root',
    svc: f.service === true, // 本机服务的根：与自身根并列展示，🖥 图标区分
  }));
  const ns = state.netState;
  if (ns) {
    // 本机服务（local）不渲染节点卡片：其根已并入本机图库（folderKey 见 /api/roots 的 service 标记），
    // 卡片重复（用户卡片 + 根卡片）由此消除；外部节点能力不变
    for (const m of ns.members) {
      if (m.local) continue;
      items.push({
        name: m.name || '节点 ' + m.id.slice(0, 8),
        path: 'net:' + m.id, kind: 'member', nodeId: m.id, nodeAddr: m.addr,
      });
    }
    for (const c of ns.cached) {
      if (c.local || ns.members.some((m) => m.id === c.id)) continue;
      items.push({
        name: (c.name || '节点 ' + c.id.slice(0, 8)) + '（离线）',
        path: 'net:' + c.id, kind: 'cached', nodeId: c.id, nodeFiles: c.files,
      });
    }
    // 已访问过的对方根：永久可见，离线可浏览本地镜像
    for (const v of ns.visited || []) {
      if (v.local) continue;
      items.push({
        name: `${v.rootName} · ${v.nodeName || '节点 ' + v.node.slice(0, 8)}`,
        path: 'net:' + v.node + ':' + v.root,
        kind: 'visited', nodeId: v.node, nodeName: v.nodeName,
        nodeRoot: v.root, nodeFiles: v.files, offline: !v.online,
      });
    }
  }
  return items;
}

/** 打开已访问过的对方根（在线实时浏览；离线自动回退本地镜像）。 */
export function openVisited(node: string, nodeName: string | undefined, root: string): void {
  void navigate({ remote: true, node, nodeName, path: root });
}

/* ---------------- 修改时间补齐（仅时间排序时重排） ---------------- */

async function loadTimes(): Promise<void> {
  const { loc } = state;
  if (!loc || !loc.path) return;
  const gen = navGen;
  setBusy('times', true);
  try {
    const r = loc.remote && loc.node
      ? await api.netTimes(loc.node, loc.path)
      : await api.times(loc.path);
    if (gen !== navGen || !state.data) return;
    const map = new Map(r.times.map((t) => [t.name, t.mtime]));
    for (const it of state.data.folders) it.mtime = map.get(it.name) ?? 0;
    for (const it of state.data.images) it.mtime = map.get(it.name) ?? 0;
    if (state.folderSort !== 'name') applyFolderView();
    window.appViewer.apply();
  } catch {
    /* 后台任务失败不影响浏览 */
  } finally {
    if (gen === navGen) setBusy('times', false);
  }
}

/* ---------------- 远程预取：进入文件夹自动批量预取 + 进度浮标 ---------------- */

let progressTimer = 0;

export function enterRemoteFolder(node: string, folder: string, images: Entry[]): void {
  leaveRemoteFolder();
  if (!images.length) return;
  state.remoteActive = { node, folder };
  void api.netPrefetch(node, folder, images.map((i) => i.path)).then(() => pollProgress()).catch(() => {});
}

export function leaveRemoteFolder(): void {
  const r = state.remoteActive;
  if (!r) return;
  state.remoteActive = null;
  clearTimeout(progressTimer);
  setBusy('net', false);
  void api.netCancel(r.node, r.folder).catch(() => {});
}

function pollProgress(): void {
  clearTimeout(progressTimer);
  const r = state.remoteActive;
  if (!r) return;
  void api.netProgress(r.node, r.folder).then((st) => {
    if (!state.remoteActive || state.remoteActive.node !== r.node || state.remoteActive.folder !== r.folder) return;
    const done = (st.done || 0) + (st.failed || 0);
    const remain = (st.total || 0) - done;
    if (st.total && remain > 0) {
      setBusy('net', true, `🌐 正在同步远程图片 ${done}/${st.total}${st.failed ? `（失败 ${st.failed}）` : ''}`);
      progressTimer = window.setTimeout(pollProgress, 1000);
    } else {
      setBusy('net', false);
    }
  }).catch(() => {
    progressTimer = window.setTimeout(pollProgress, 2000);
  });
}

/* ---------------- 布局与模式 ---------------- */

export function refreshLayout(): void {
  const { data, mode } = state;
  const has = !!data;
  const both = has && !data!.isRoots && data!.folders.length > 0 && data!.images.length > 0;
  $('folders-section').hidden = !(has && mode === 'files');
  $('images-section').hidden = !(has && mode === 'images');

  let emptyShown = false;
  if (has && data!.isRoots) {
    if (buildRootsItems().length === 0) {
      $('empty-text').textContent = '这里什么都没有';
      ($('empty-up') as HTMLElement).hidden = true;
      ($('empty-clear') as HTMLElement).hidden = true;
      emptyShown = true;
    } else if (mode === 'files' && state.filterText && state.folderViewEmpty) {
      $('empty-text').textContent = `没有匹配「${state.filterText.trim()}」的根`;
      ($('empty-up') as HTMLElement).hidden = false;
      ($('empty-clear') as HTMLElement).hidden = false;
      emptyShown = true;
    }
  } else if (has && !data!.folders.length && !data!.images.length) {
    $('empty-text').textContent = '这里没有子文件夹，也没有图片';
    ($('empty-up') as HTMLElement).hidden = false;
    ($('empty-clear') as HTMLElement).hidden = true;
    emptyShown = true;
  } else if (has && mode === 'files' && data!.folders.length > 0 && state.filterText && state.folderViewEmpty) {
    $('empty-text').textContent = `没有匹配「${state.filterText.trim()}」的文件夹`;
    ($('empty-up') as HTMLElement).hidden = false;
    ($('empty-clear') as HTMLElement).hidden = false;
    emptyShown = true;
  }
  $('empty').hidden = !emptyShown;

  const fsActive = document.body.classList.contains('fs-active');
  ($('fab-up') as HTMLElement).hidden = !(has && !data!.isRoots && !fsActive);
  ($('fab-mode') as HTMLElement).hidden = !both;
  $('fab-mode').textContent = mode === 'files' ? '🖼 图片' : '📁 文件';
}

export function setMode(m: 'files' | 'images'): void {
  state.mode = m;
  document.body.classList.toggle('mode-images', m === 'images');
  document.body.classList.toggle('mode-files', m === 'files');
  refreshLayout();
}

/* ---------------- 网络状态轮询（本机接口，开销极小） ---------------- */

let rootsViewSig = '';

let lastLibVer = -1;

/** 图库版本变化（本机服务根同步）时重拉根列表：根视图无需手动刷新。 */
async function refreshRootsData(): Promise<void> {
  try {
    const r = await api.roots();
    if (!state.data?.isRoots) return;
    state.data.folders = r.roots.map((r2) => ({
      name: r2.name, path: r2.path, mtime: r2.mtime, service: r2.service === true,
    }));
    rootsViewSig = "";
    applyFolderView();
  } catch { /* 失败静默，下一轮轮询重试 */ }
}

export async function refreshNetState(): Promise<void> {
  try {
    state.netState = await api.netState();
  } catch {
    state.netState = null;
  }
  updateNetChip();
  window.appNetPanel.renderIfOpen();
  if (state.netState?.libVersion !== undefined && state.netState.libVersion !== lastLibVer) {
    const first = lastLibVer === -1;
    lastLibVer = state.netState.libVersion;
    // 首次不重拉（导航时刚取过）；停在根视图时自动刷新
    if (!first && state.data?.isRoots) void refreshRootsData();
  }
  // 根视图内容有变化才重绘（避免每 5s 重建 DOM 干扰点击/悬停）
  if (state.loc && state.loc.path === "" && state.data?.isRoots) {
    const ns = state.netState;
    const sig = JSON.stringify([
      state.data.folders.map((f) => f.path),
      ns?.members.map((m) => [m.id, m.name, m.addr]),
      ns?.cached.map((c) => [c.id, c.name, c.files]),
      ns?.visited?.map((v) => [v.node, v.root, v.files, v.online]),
    ]);
    if (sig !== rootsViewSig) {
      rootsViewSig = sig;
      applyFolderView();
    }
  }
}

function updateNetChip(): void {
  const s = state.netState;
  const n = s ? s.members.length : 0;
  const online = !!(s && (s.joined || s.hub || n > 0));
  $('net-chip').classList.toggle('on', online);
  $('net-chip-text').textContent = online ? `🌐 在网 · ${n + 1} 节点` : '🌐 网络';
}

let netTimer = 0;
export function startNetPolling(): void {
  void refreshNetState();
  netTimer = window.setInterval(() => void refreshNetState(), 5000);
}
export function stopNetPolling(): void {
  clearInterval(netTimer);
}
