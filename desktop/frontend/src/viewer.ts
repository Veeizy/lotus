// 图片面板：基于 Swiper 的漫画式阅读器。
// 滑动/拖拽翻页（含惯性、端点橡皮筋）、双指捏合缩放 + 缩放平移、步进/滑杆缩放、键盘翻页、页码跳页、全屏。
import Swiper from 'swiper';
import { Zoom, Keyboard } from 'swiper/modules';

import 'swiper/css';
import 'swiper/css/zoom';
import { api } from './api';
import { $, h } from './dom';
import { state } from './app';

let view: { name: string; path: string }[] = [];
let swiper: Swiper | null = null;

// 安卓壳注入的保存桥（addJavascriptInterface）；存在才启用长按保存
declare global {
  interface Window {
    LotusAndroid?: { saveImage: (url: string) => void };
  }
}

// 缩放范围 10%–1000%；＋/－ 按钮与 +/- 键每次步进 50 个百分点
const ZOOM_MIN = 0.1;
const ZOOM_MAX = 10;
const ZOOM_STEP = 0.5;

// 便签缩放：翻页保持倍率（新页以中心缩放），仅退出当前文件夹时复位。
// turning=翻页过渡中（Swiper 复位旧页会发出 zoomChange(1)，需吞掉）；
// programmatic=程序触发的缩放（不回写便签倍率，由调用方显式赋值）。
let stickyZoom = 1;
let lastFolderKey: string | null = null;
let turning = false;
let programmatic = false;

export function images(): { name: string; path: string }[] {
  return view;
}

export function currentIndex(): number {
  return swiper ? swiper.activeIndex : 0;
}

function imgUrl(p: string): string {
  const { loc } = state;
  return loc?.remote && loc.node ? api.netImageUrl(loc.node, p) : api.imageUrl(p);
}

function slideHtml(img: { name: string; path: string }): string {
  // 消费者模式：slide 只留壳，图片由内存窗口按需挂载（见下方 syncWindow）
  if (!state.writable) {
    return '<div class="swiper-slide"><div class="swiper-zoom-container"></div></div>';
  }
  return `<div class="swiper-slide"><div class="swiper-zoom-container"><img src="${imgUrl(img.path)}" alt="" loading="lazy" draggable="false"></div></div>`;
}

/* ---------------- 消费者模式的图片内存窗口 ----------------
 * 只缓存当前 ±PRELOAD 共 PRELOAD*2+1 张（头尾时钳制为头尾同样张数），全部在
 * JS 内存（fetch → blob → objectURL），以 path 为键——排序变化/异步 mtime 重排
 * 不抖动缓存；出窗立即 abort/revoke。窗口跟随 view 数组（即当前生效的排序）。
 * 磁盘零缓存由安卓壳 WebView 的 LOAD_NO_CACHE 保证；桌面端（writable）不启用。 */
const PRELOAD = 5;
const MAX_BLOB_BYTES = 32 * 1024 * 1024; // 超大图不进内存缓存（当前图回退直连 URL 展示）

interface BlobEntry {
  state: 'loading' | 'ready' | 'error' | 'oversize';
  url?: string; // ready：blob objectURL
  size?: number;
  ctrl?: AbortController;
}
const blobCache = new Map<string, BlobEntry>();

function slideEl(i: number): HTMLElement | null {
  return swiper?.slides[i] ?? null;
}

/** 当前排序下的预取窗口 [lo, hi]（约 cur±5，头尾钳制后仍满 PRELOAD*2+1 张）。 */
function windowRange(cur: number, n: number): [number, number] {
  const size = PRELOAD * 2 + 1;
  const lo = Math.max(0, Math.min(cur - PRELOAD, n - size));
  return [lo, Math.min(n - 1, lo + size - 1)];
}

function clearBlobCache(): void {
  for (const p of [...blobCache.keys()]) evictBlob(p);
}

function evictBlob(path: string): void {
  const e = blobCache.get(path);
  if (!e) return;
  e.ctrl?.abort();
  if (e.url) URL.revokeObjectURL(e.url);
  blobCache.delete(path);
}

function ensureBlob(path: string): BlobEntry {
  const hit = blobCache.get(path);
  if (hit && hit.state !== 'error') return hit;
  if (hit) blobCache.delete(path); // 出错后重新进入窗口时重试
  const e: BlobEntry = { state: 'loading' };
  const ctrl = new AbortController();
  e.ctrl = ctrl;
  fetch(imgUrl(path), { signal: ctrl.signal }).then(async (r) => {
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const len = Number(r.headers.get('Content-Length') || 0);
    if (len > MAX_BLOB_BYTES) {
      try { await r.body?.cancel(); } catch { /* 忽略 */ }
      return 'oversize';
    }
    const b = await r.blob();
    if (b.size > MAX_BLOB_BYTES) return 'oversize';
    e.state = 'ready';
    e.url = URL.createObjectURL(b);
    e.size = b.size;
    return 'ready';
  }).then((st) => {
    if (st === 'oversize') e.state = 'oversize';
    mountNear(path); // 邻页（±1）若在等这张图，就绪即挂载
  }).catch(() => {
    if (!ctrl.signal.aborted) {
      e.state = 'error';
      mountNear(path); // 当前/邻页回退直连 URL 展示；重试等下一次进窗
    }
  });
  blobCache.set(path, e);
  return e;
}

/** 重算窗口：预取 [lo,hi]、仅挂载当前 ±1、出窗即释放。 */
function syncWindow(): void {
  if (!swiper || !view.length) return;
  const cur = swiper.activeIndex;
  const n = view.length;
  const [lo, hi] = windowRange(cur, n);
  for (let i = 0; i < n; i++) {
    const p = view[i].path;
    if (i < lo || i > hi) {
      unmountSlide(i);
      evictBlob(p);
      continue;
    }
    if (Math.abs(i - cur) <= 1) mountSlide(i, p, ensureBlob(p));
    else unmountSlide(i);
  }
}

function mountNear(path: string): void {
  if (!swiper) return;
  const cur = swiper.activeIndex;
  for (let i = Math.max(0, cur - 1); i <= Math.min(view.length - 1, cur + 1); i++) {
    if (view[i]?.path === path) mountSlide(i, path, blobCache.get(path)!);
  }
}

function mountSlide(i: number, path: string, e: BlobEntry): void {
  const box = slideEl(i)?.querySelector('.swiper-zoom-container');
  if (!box) return;
  let src: string | null = null;
  if (e.state === 'ready') src = e.url!;
  else if (e.state === 'oversize' || e.state === 'error') src = imgUrl(path); // 超大/失败：直连兜底（壳层不落盘）
  const img = box.querySelector('img');
  if (!src) {
    if (!img) {
      const placeholder = document.createElement('img');
      placeholder.alt = '';
      placeholder.draggable = false;
      box.append(placeholder); // blob 就绪后经 mountNear 补 src
    }
    return;
  }
  if (img?.getAttribute('src') === src) return;
  img?.remove();
  const next = document.createElement('img');
  next.src = src;
  next.alt = '';
  next.draggable = false;
  box.append(next);
}

function unmountSlide(i: number): void {
  slideEl(i)?.querySelectorAll('img').forEach((im) => im.remove());
}

/** 文件夹标识：跨图翻页保持缩放、换文件夹才复位的判定依据。 */
function folderKey(): string | null {
  const { loc, data } = state;
  if (!loc || !data || data.isRoots) return null;
  return (loc.remote && loc.node ? loc.node : '') + '|' + (data.path || loc.path || '');
}

/** 应用当前数据：重建滑动列表并保持当前位置。 */
export function apply(keepName?: string): void {
  const { data } = state;
  const prevPath = keepName ?? (swiper ? view[swiper.activeIndex]?.path : undefined);
  view = data && !data.isRoots
    ? data.images.slice().sort(sortCompareForViewer()).map((i) => ({ name: i.name, path: i.path }))
    : [];
  // 仅换文件夹复位便签倍率；同文件夹重建（重排/刷新）保持
  const key = folderKey();
  if (key !== lastFolderKey) {
    stickyZoom = 1;
    lastFolderKey = key;
    clearBlobCache(); // 换文件夹后旧缓存全作废（path 属于上一个目录）
  }
  const sw = ensureSwiper();
  closeZoomPop();
  // 幻灯片即将整体重建：先清掉旧缩放状态（否则手势引用已脱离子树的节点）
  resetZoomState();
  // v11 动态更新 slides 的方式：改 wrapper DOM 后调 update()
  const wrapper = sw.el.querySelector('.swiper-wrapper');
  if (wrapper) wrapper.innerHTML = view.map(slideHtml).join('');
  const idx = view.findIndex((v) => v.path === prevPath);
  sw.update();
  sw.slideTo(idx >= 0 ? idx : 0, 0);
  reapplyZoom();
  updateHud();
  if (!state.writable) syncWindow();
}

// 排序与 app.ts 共用一份 zh+numeric 比较（经 window 注入，避免循环依赖）
function sortCompareForViewer(): (a: { name: string }, b: { name: string }) => number {
  return (window as unknown as {
    __viewerSort: (a: { name: string }, b: { name: string }) => number;
  }).__viewerSort;
}

function ensureSwiper(): Swiper {
  // v11 不再自动创建 wrapper：容器必须预先包含 .swiper-wrapper
  const el = document.querySelector('#strip');
  if (el && !el.querySelector('.swiper-wrapper')) {
    el.innerHTML = '<div class="swiper-wrapper"></div>';
  }
  if (!swiper) swiper = new Swiper('#strip', {
    modules: [Zoom, Keyboard],
    zoom: {
      maxRatio: ZOOM_MAX,
      minRatio: ZOOM_MIN,
      toggle: false, // 双击由本文件 dblclick 接管（步进缩放，避免直接跳到 1000%）
    },
    keyboard: { enabled: false }, // 键盘由全局 keydown 统一驱动（见 viewerKeydown）
    simulateTouch: true, // 鼠标拖拽 = 滑动翻页（桌面端）
    grabCursor: true,
    watchSlidesProgress: true,
    on: {
      slideChange: () => {
        turning = true; // 过渡结束前 Swiper 会复位旧页缩放，其 zoomChange(1) 需吞掉
        updateHud();
        closeSavePop();
        cancelPress();
        if (!state.writable) syncWindow(); // 内存窗口跟随翻页重算
      },
      // 翻页时 Swiper 在 transitionEnd 才把缩放复位到 100%，晚于 slideChange，需再同步一次；
      // 复位完成后（microtask）按便签倍率恢复新页
      transitionEnd: () => {
        syncZoomUi();
        queueMicrotask(() => {
          turning = false;
          reapplyZoom();
        });
      },
      zoomChange: () => {
        if (!programmatic && !turning && swiper) stickyZoom = swiper.zoom.scale; // 双指捏合等真实手势
        syncZoomUi();
      },
    },
  });
  return swiper;
}

function updateHud(): void {
  if (!swiper) return;
  const n = view.length;
  $('page-indicator').textContent = n ? `${swiper.activeIndex + 1} / ${n}` : '0 / 0';
  syncZoomUi();
  for (const b of document.querySelectorAll<HTMLButtonElement>('#img-sort button')) {
    b.classList.toggle('active', b.dataset.sort === state.imageSort);
  }
}

/* ---------------- 翻页 / 缩放控制 ---------------- */

export function goto(i: number): void {
  if (!swiper || !view.length) return;
  swiper.slideTo(Math.max(0, Math.min(i, view.length - 1)), 0);
}

export function next(): void {
  swiper?.slideNext();
}
export function prev(): void {
  swiper?.slidePrev();
}

function zoomPercent(scale: number): string {
  return `${Math.round(scale * 100)}%`;
}

/** 同步缩放显示：工具栏百分比 + 弹层滑杆（若打开）。 */
function syncZoomUi(): void {
  if (!swiper) return;
  $('zoom-reset').textContent = zoomPercent(swiper.zoom.scale);
  if (zoomPopOpen) {
    $('zoom-pop-val').textContent = zoomPercent(swiper.zoom.scale);
    ($('zoom-slider') as HTMLInputElement).value = String(Math.round(scaleToSlider(swiper.zoom.scale)));
  }
}

/** 精确缩放到指定倍率（钳制在 10%–1000%）。 */
function zoomTo(scale: number): void {
  if (!swiper) return;
  const target = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, scale));
  if (Math.abs(target - 1) < 0.005) {
    zoomReset();
    return;
  }
  stickyZoom = target;
  // Swiper 的 zoom.in(数值) 直接定位到该倍率；平移量按比例延续，保持当前视点
  programmatic = true;
  try {
    swiper.zoom.in(target);
  } finally {
    programmatic = false;
  }
  syncZoomUi();
}

function zoomStep(delta: number): void {
  // 捏合手势会留下任意倍率，取整到百分点再叠加，保证步进结果总是整齐的数值
  const cur = Math.round((swiper?.zoom.scale ?? 1) * 100) / 100;
  zoomTo(cur + delta);
}

function zoomReset(): void {
  stickyZoom = 1;
  programmatic = true;
  try {
    swiper?.zoom.out();
  } finally {
    programmatic = false;
  }
  syncZoomUi();
}

/** 清掉缩放手势状态（apply 重建幻灯片前调用；空列表时 Swiper 的 out() 会崩，需跳过）。 */
function resetZoomState(): void {
  if (!swiper || !swiper.slides.length) return;
  programmatic = true;
  try {
    swiper.zoom.out();
  } finally {
    programmatic = false;
  }
}

/** 把便签倍率重新应用到当前页（翻页结束/同文件夹重建后调用；1 = 免操作）。 */
function reapplyZoom(): void {
  if (!swiper || stickyZoom === 1 || !swiper.slides.length) return;
  programmatic = true;
  try {
    swiper.zoom.in(stickyZoom);
  } finally {
    programmatic = false;
  }
  syncZoomUi();
}

/* ---------------- 缩放滑杆弹层 ---------------- */

let zoomPopOpen = false;

// 滑杆行程 [0,1000] → 倍率：中间 80% 行程覆盖常用的 50%–500%，
// 两端各 10% 平均分给极端区 10%–50% 与 500%–1000%
function sliderToScale(v: number): number {
  if (v <= 100) return 0.1 + (v / 100) * 0.4;
  if (v <= 900) return 0.5 + ((v - 100) / 800) * 4.5;
  return 5 + ((v - 900) / 100) * 5;
}

function scaleToSlider(scale: number): number {
  const p = scale * 100;
  if (p <= 50) return ((p - 10) / 40) * 100;
  if (p <= 500) return 100 + ((p - 50) / 450) * 800;
  return 900 + ((p - 500) / 500) * 100;
}

function openZoomPop(): void {
  if (!view.length) return;
  const pop = $('zoom-pop') as HTMLElement;
  const anchor = ($('zoom-reset') as HTMLElement).getBoundingClientRect();
  pop.hidden = false;
  const left = Math.min(window.innerWidth - pop.offsetWidth - 8,
    Math.max(8, anchor.left + anchor.width / 2 - pop.offsetWidth / 2));
  pop.style.left = `${left}px`;
  pop.style.top = `${anchor.bottom + 8}px`;
  zoomPopOpen = true;
  syncZoomUi();
}

function closeZoomPop(): void {
  if (!zoomPopOpen) return;
  zoomPopOpen = false;
  ($('zoom-pop') as HTMLElement).hidden = true;
}

/* ---------------- 长按保存（安卓端；桥不存在时整段逻辑不启用） ---------------- */

let pressTimer = 0;
let pressPos: { x: number; y: number } | null = null;
let savePop: HTMLElement | null = null;

function cancelPress(): void {
  if (pressTimer) {
    clearTimeout(pressTimer);
    pressTimer = 0;
  }
  pressPos = null;
}

/** 从被按住的 img 反推所在 slide 的服务端 URL（当前显示可能是 blob，保存必须用源地址）。 */
function saveUrlForSlide(img: Element): string | null {
  if (!swiper) return null;
  const slide = img.closest('.swiper-slide');
  if (!slide) return null;
  const idx = swiper.slides.indexOf(slide as HTMLElement);
  if (idx < 0 || !view[idx]) return null;
  return imgUrl(view[idx].path);
}

function closeSavePop(): void {
  savePop?.remove();
  savePop = null;
}

function showSavePop(img: Element, x: number, y: number): void {
  const url = saveUrlForSlide(img);
  if (!url) return;
  closeSavePop();
  savePop = h('div', { class: 'save-pop' },
    h('button', {
      class: 'save-pop-btn',
      onclick: (ev) => {
        ev.stopPropagation();
        closeSavePop();
        window.LotusAndroid?.saveImage(url);
      },
    }, '⬇ 保存到相册'),
  );
  document.body.append(savePop);
  const el = savePop;
  el.style.left = `${Math.min(window.innerWidth - el.offsetWidth - 8, Math.max(8, x - el.offsetWidth / 2))}px`;
  el.style.top = `${Math.max(8, y - el.offsetHeight - 20)}px`;
}

/* ---------------- 全屏 ---------------- */

export function isFullscreen(): boolean {
  return document.body.classList.contains('fs-active');
}

/** 全屏态则退出并返回 true。安卓返回手势据此消费一次返回（先退全屏、不翻页）；
 * WebView 未配 WebChromeClient 时 Fullscreen API 不生效，状态判定只认 fs-active 类。 */
export function exitFullscreenIfActive(): boolean {
  if (!isFullscreen()) return false;
  toggleFullscreen();
  return true;
}

export function toggleFullscreen(): void {
  if (isFullscreen()) {
    document.body.classList.remove('fs-active');
    document.exitFullscreen?.().catch(() => {});
  } else {
    closeZoomPop();
    document.body.classList.add('fs-active');
    document.documentElement.requestFullscreen?.().catch(() => {});
  }
}

/* ---------------- 事件绑定（main 里调用一次） ---------------- */

export function bindViewer(): void {
  const strip = $('strip') as HTMLElement;

  // 滚轮：普通滚轮=翻页；Ctrl+滚轮=缩放（每次 10 个百分点，比按钮更细腻）
  strip.addEventListener('wheel', (e) => {
    if (e.ctrlKey) {
      e.preventDefault();
      zoomStep(e.deltaY < 0 ? 0.1 : -0.1);
      return;
    }
    if (Math.abs(e.deltaX) > Math.abs(e.deltaY)) return;
    e.preventDefault();
    if (e.deltaY > 0 || e.deltaX > 0) next();
    else prev();
  }, { passive: false });

  // 双击 = 100% ↔ 200% 切换
  strip.addEventListener('dblclick', () => {
    if ((swiper?.zoom.scale ?? 1) > 1.005) zoomReset();
    else zoomTo(2);
  });

  // 边缘按钮
  $('img-prev').addEventListener('click', prev);
  $('img-next').addEventListener('click', next);

  // 跳页：点击页码 → 输入框 → 回车跳转（自动钳制）
  $('page-indicator').addEventListener('click', () => beginJump());
  $('page-input').addEventListener('keydown', (e) => {
    const ke = e as KeyboardEvent;
    ke.stopPropagation();
    if (ke.key === 'Escape') {
      ($('page-input') as HTMLInputElement).hidden = true;
      ($('page-indicator') as HTMLElement).hidden = false;
    } else if (ke.key === 'Enter') {
      const n = parseInt(($('page-input') as HTMLInputElement).value, 10);
      if (Number.isFinite(n)) goto(n - 1);
      ($('page-input') as HTMLInputElement).hidden = true;
      ($('page-indicator') as HTMLElement).hidden = false;
    }
  });
  $('page-input').addEventListener('blur', () => {
    ($('page-input') as HTMLInputElement).hidden = true;
    ($('page-indicator') as HTMLElement).hidden = false;
  });

  // 缩放：百分比按钮弹出滑杆；＋/－ 步进；松手取整
  $('zoom-reset').addEventListener('click', openZoomPop);
  $('zoom-in').addEventListener('click', () => zoomStep(ZOOM_STEP));
  $('zoom-out').addEventListener('click', () => zoomStep(-ZOOM_STEP));
  ($('zoom-slider') as HTMLInputElement).addEventListener('input', () => {
    zoomTo(sliderToScale(Number(($('zoom-slider') as HTMLInputElement).value)));
  });
  ($('zoom-slider') as HTMLInputElement).addEventListener('change', () => {
    // 松手时按当前倍率重新对齐（取整到百分点），滑杆回正到对应刻度
    zoomTo(Math.round((swiper?.zoom.scale ?? 1) * 100) / 100);
  });
  for (const b of document.querySelectorAll<HTMLButtonElement>('#zoom-pop .zoom-pop-marks button')) {
    b.addEventListener('click', () => zoomTo(Number(b.dataset.zoom)));
  }
  document.addEventListener('pointerdown', (e) => {
    if (!zoomPopOpen) return;
    const t = e.target as Node;
    if (!($('zoom-pop') as HTMLElement).contains(t) && !($('zoom-reset') as HTMLElement).contains(t)) {
      closeZoomPop();
    }
  });
  $('fs-toggle').addEventListener('click', toggleFullscreen);

  // 长按图片 → 保存气泡（安卓壳注入 LotusAndroid 桥时才启用）
  if (window.LotusAndroid?.saveImage) {
    strip.addEventListener('pointerdown', (e) => {
      const img = (e.target as Element).closest?.('img');
      if (!img) return;
      if (!e.isPrimary) { cancelPress(); return; } // 双指缩放期间不触发
      cancelPress();
      const imgEl = img;
      pressPos = { x: e.clientX, y: e.clientY };
      pressTimer = window.setTimeout(() => {
        pressTimer = 0;
        if (pressPos) showSavePop(imgEl, pressPos.x, pressPos.y);
      }, 550);
    });
    strip.addEventListener('pointermove', (e) => {
      // 手指移动（滑动翻页）取消长按
      if (pressTimer && pressPos &&
        Math.hypot(e.clientX - pressPos.x, e.clientY - pressPos.y) > 10) cancelPress();
    });
    strip.addEventListener('pointerup', cancelPress);
    strip.addEventListener('pointercancel', cancelPress);
    // 点气泡外部任意处关闭
    document.addEventListener('pointerdown', (e) => {
      if (savePop && !(e.target as Element).closest?.('.save-pop')) closeSavePop();
    });
  }
}

function beginJump(): void {
  if (!view.length) return;
  const input = $('page-input') as HTMLInputElement;
  input.hidden = false;
  input.value = String(currentIndex() + 1);
  input.focus();
  input.select();
  ($('page-indicator') as HTMLElement).hidden = true;
}

export function viewerKeydown(e: KeyboardEvent): boolean {
  if (state.mode !== 'images' || !view.length) return false;
  switch (e.key) {
    case 'Escape':
      if (zoomPopOpen) {
        closeZoomPop();
        return true;
      }
      return false;
    case 'ArrowLeft':
      prev();
      return true;
    case 'ArrowRight':
      next();
      return true;
    case '+':
    case '=':
      zoomStep(ZOOM_STEP);
      return true;
    case '-':
      zoomStep(-ZOOM_STEP);
      return true;
    case '0':
      zoomReset();
      return true;
    default:
      return false;
  }
}

export function setSortButtons(mode: 'name' | 'time-desc' | 'time-asc'): void {
  state.imageSort = mode;
  apply();
}
