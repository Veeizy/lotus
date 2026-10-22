// 入口：装配全部面板、全局快捷键、启动轮询。
import 'swiper/css';
import 'swiper/css/zoom';
import { applyFolderView, bindRootsView, clearFilterLocal, goBack, goHome, locKey, navigate, onPopstate,
  refreshLayout, refreshNetState, rememberScroll, setMode, setBusy, startNetPolling, state, stopNetPolling,
  syncFilterHistory, applyFolderView as applyFolders } from './app';
import { api } from './api';
import { $, h } from './dom';
import { bindNetPanel, close as closeNetPanel, isOpen as netOpen } from './netpanel';
import { applyTheme, bindSettings, isOpen as settingsOpen, currentTheme } from './settings';
import * as viewer from './viewer';

// 全局句柄（模块间少量互调，避免循环依赖）
declare global {
  interface Window {
    appViewer: { apply: (keepName?: string) => void; exitFs: () => boolean };
    appNetPanel: { renderIfOpen: () => void };
  }
}

function bindToolbar(): void {
  $('filter').addEventListener('input', (e) => {
    state.filterText = (e.target as HTMLInputElement).value;
    syncFilterHistory(); // 立即同步哨兵条目：输入后立刻按返回也按「先清筛选」处理
    clearTimeout(filterTimer);
    filterTimer = window.setTimeout(() => {
      applyFolderView();
      refreshLayout();
    }, 120);
  });
  $('filter').addEventListener('keydown', (e) => {
    const ke = e as KeyboardEvent;
    if (ke.key === 'Escape') (e.target as HTMLInputElement).blur();
    ke.stopPropagation();
  });
  $('filter-clear').addEventListener('click', () => {
    clearFilterLocal();
    syncFilterHistory();
  });
  $('empty-clear').addEventListener('click', () => {
    clearFilterLocal();
    syncFilterHistory();
  });
  $('empty-up').addEventListener('click', goBack);

  $('home-btn').addEventListener('click', goHome);
  $('fab-up').addEventListener('click', goBack);
  $('fab-mode').addEventListener('click', () => setMode(state.mode === 'files' ? 'images' : 'files'));

  document.querySelectorAll<HTMLButtonElement>('#folder-sort button').forEach((b) => {
    b.addEventListener('click', () => {
      state.folderSort = b.dataset.sort as typeof state.folderSort;
      applyFolders();
      syncSortButtons();
    });
  });
  document.querySelectorAll<HTMLButtonElement>('#img-sort button').forEach((b) => {
    b.addEventListener('click', () => {
      viewer.setSortButtons(b.dataset.sort as typeof state.imageSort);
    });
  });
  syncSortButtons();
}

function syncSortButtons(): void {
  for (const b of document.querySelectorAll<HTMLButtonElement>('#folder-sort button')) {
    b.classList.toggle('active', b.dataset.sort === state.folderSort);
  }
  for (const b of document.querySelectorAll<HTMLButtonElement>('#img-sort button')) {
    b.classList.toggle('active', b.dataset.sort === state.imageSort);
  }
}

let filterTimer = 0;

function globalKeydown(e: KeyboardEvent): void {
  const target = e.target as HTMLElement;
  if (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA') return;
  if (netOpen()) {
    if (e.key === 'Escape') closeNetPanel();
    return;
  }
  if (settingsOpen()) {
    if (e.key === 'Escape') import('./settings').then((m) => m.close());
    return;
  }
  if (e.key === '/' && state.mode === 'files') {
    e.preventDefault();
    ($('filter') as HTMLInputElement).focus();
    return;
  }
  if (e.key === 'Escape') {
    if (viewer.isFullscreen()) {
      viewer.toggleFullscreen();
      return;
    }
    if (state.filterText) {
      clearFilterLocal();
      syncFilterHistory();
      return;
    }
  }
  if (viewer.viewerKeydown(e)) return;
  if (e.key === 'Backspace' || (e.altKey && e.key === 'ArrowLeft')) {
    e.preventDefault();
    goBack(); // 有筛选先清筛选留在本页，筛选已空才回退（与返回键/右滑一致）
  }
}

import { sortCompare } from './app';
(window as unknown as { __viewerSort: unknown }).__viewerSort =
  sortCompare(state.imageSort);
window.appViewer = { apply: (keepName?: string) => viewer.apply(keepName), exitFs: () => viewer.exitFullscreenIfActive() };
window.appNetPanel = { renderIfOpen: () => import('./netpanel').then((m) => m.renderIfOpen()) };

function boot(): void {
  applyTheme(currentTheme());
  bindRootsView();
  bindToolbar();
  viewer.bindViewer();
  bindNetPanel();
  bindSettings();

  // 消费者模式判定（服务端 writable=false → 隐藏管理入口、viewer 内存窗口）；
  // 失败按可管理处理（桌面端常态）。放在 navigate 前保证首屏即正确。
  void api.configGet().then((cfg) => {
    state.writable = cfg.writable !== false;
    document.body.classList.toggle('consumer', !state.writable);
    if (!state.writable) window.appViewer.apply();
  }).catch(() => {});

  // 返回键/浏览器后退/安卓右滑手势 → onPopstate 统一处理（app.ts）：
  // 有筛选哨兵先清筛选留在本页，否则回退到上一个访问位置；根视图再退由壳层结束页面
  window.addEventListener('popstate', onPopstate);

  // 滚动记忆：持续记录当前位置的滚动条（返回/重访时恢复，见 app.ts）
  const scroller = $('main') as HTMLElement;
  scroller.addEventListener('scroll', () => {
    rememberScroll(locKey(state.loc), scroller.scrollTop);
  }, { passive: true });
  // SPA 内容重建会让浏览器的同文档滚动恢复错位，关掉由我们接管
  history.scrollRestoration = 'manual';

  window.addEventListener('keydown', globalKeydown);
  window.addEventListener('beforeunload', () => {
    stopNetPolling();
    setBusy('net', false);
  });

  setMode('files');
  void navigate({ remote: false, path: '' });
  startNetPolling();
  // 失焦时暂停轮询（桌面端省资源），聚焦恢复
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) stopNetPolling();
    else startNetPolling();
  });

  // 面板互斥的 body 类初始态
  document.body.classList.add('mode-files');
  void refreshNetState();
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', boot);
} else {
  boot();
}

export { h };
