// 与 PROTOCOL.md §2 冻结契约一一对应的类型定义。

export interface Entry {
  name: string;
  path: string;
  mtime?: number;
  service?: boolean; // 本机服务的根（并入本机图库，直接读盘）
}

export interface Crumb {
  name: string;
  path: string;
}

export interface BrowseData {
  name: string;
  path: string;
  parent: string | null;
  isRoots?: boolean;
  breadcrumbs: Crumb[];
  folders: Entry[];
  images: Entry[];
  offline?: boolean;
}

export interface RootsResp {
  roots: Entry[];
}

export interface TimesResp {
  times: Entry[];
}

export interface Me {
  id: string;
  name: string;
  publicKey: string;
  secret?: string;
}

export interface UplinkStatus {
  addr: string;
  status: string;
}

export interface MemberInfo {
  id: string;
  name: string;
  addr: string;
  direct: boolean;
  local?: boolean; // 本机服务（同机 lotus-server）：与客户端同属一个信源
}

export interface CachedNode {
  id: string;
  name: string;
  files: number;
  lastSeen: number;
  local?: boolean;
}

export interface VisitedRoot {
  node: string;
  nodeName: string;
  root: string;
  rootName: string;
  files: number;
  online: boolean;
  local?: boolean;
}

export interface AppConfig {
  paths: string[];
  cacheDir: string;
  quicPort: number;
  name: string;
  fixedRoots?: boolean; // 移动端：根与缓存目录固定
  writable?: boolean; // 服务端判定：本客户端是否可管理配置（非本机 = 消费者模式）
}

export interface NetState {
  ok: boolean;
  me: Me;
  share: boolean;
  cacheServe: boolean;
  concurrency: number;
  uplinks: UplinkStatus[];
  members: MemberInfo[];
  cached: CachedNode[];
  visited?: VisitedRoot[];
  hub: boolean;
  joined: boolean;
  hostLoopback: boolean;
  libVersion?: number; // 本地图库版本（服务根同步后自动刷新根视图）
  lanDiscovery?: boolean; // 局域网自动发现并入网
  scanPorts?: number[]; // 局域网扫描附加端口（默认 3456 恒在）
}

export interface LanDiscoverItem {
  id: string;
  addr: string;
  name?: string;
  via?: string; // mdns / scan
  ok: boolean;
  error?: string;
}

export interface LanDiscoverResp {
  ok: boolean;
  results: LanDiscoverItem[];
}

export interface DiscoverItem {
  id: string;
  name: string;
  addr: string;
  ts: number;
  known: boolean;
}

export interface JoinResultItem {
  addr: string;
  ok: boolean;
  node?: string;
  error?: string;
  note?: string;
}

export interface JoinResp {
  ok: boolean;
  results: JoinResultItem[];
}

export interface PrefetchResp {
  ok: boolean;
  total: number;
  cached: number;
}

export interface Progress {
  total: number;
  done: number;
  failed: number;
}

export type SortMode = 'name' | 'time-desc' | 'time-asc';

// Windows 防火墙检测（见 internal/firewall；非 Windows supported=false）
export interface FirewallNeed {
  proto: string;
  port: number;
  desc: string;
  allowed: boolean;
  ruleName?: string; // 命中的放行规则名
  blockBy?: string; // 命中的拦截规则名（拦截优先于放行）
}

export interface FirewallStatus {
  supported: boolean;
  reason?: string;
  firewallOn: boolean;
  activeProfiles: string[];
  exe: string;
  needs: FirewallNeed[];
  allowed: boolean; // 防火墙关闭时恒为 true
}

// 浏览位置：本地或远程节点
export interface Loc {
  remote: boolean;
  node?: string; // remote 时为 PeerID
  nodeName?: string; // 显示名（来自成员表或已记住）
  path: string; // 空串 = 根视图
}
