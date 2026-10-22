// DOM 辅助：h() 创建元素，$() 取既有元素。保持零依赖。

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs?: Record<string, string | boolean | ((e: Event) => void)>,
  ...children: (Node | string | null | undefined)[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === undefined || v === null) continue;
      if (typeof v === 'function') {
        el.addEventListener(k === 'onclick' ? 'click' : k, v as EventListener);
      } else if (typeof v === 'boolean') {
        if (v) el.setAttribute(k, '');
      } else if (k === 'class') {
        el.className = v;
      } else {
        el.setAttribute(k, v);
      }
    }
  }
  for (const c of children) {
    if (c === null || c === undefined) continue;
    el.append(typeof c === 'string' ? document.createTextNode(c) : c);
  }
  return el;
}

export function $(id: string): HTMLElement {
  const el = document.getElementById(id);
  if (!el) throw new Error(`缺少元素 #${id}`);
  return el;
}

export function clear(el: HTMLElement): void {
  while (el.firstChild) el.removeChild(el.firstChild);
}

// 中文拼音 + 自然数排序（WebView2/Chromium 的 Intl 与原版一致）
export function zhCollate(a: string, b: string): number {
  return a.localeCompare(b, 'zh', { numeric: true, sensitivity: 'base' });
}

// 模糊筛选：关键字的字符按顺序出现即命中（忽略大小写与空白）
export function fuzzyMatch(name: string, q: string): boolean {
  let i = 0;
  for (const ch of name) {
    if (ch === q[i]) i++;
    if (i === q.length) return true;
  }
  return i === q.length;
}

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

export function fmtTime(ms: number): string {
  if (!ms) return '';
  const d = new Date(ms);
  const p = (x: number) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
