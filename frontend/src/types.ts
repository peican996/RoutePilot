// 界面用到的视图模型类型。
// 后端已经通过 bindings/routepilot 导出了同构的模型，但历史代码里手写了一份
// 且做了双重断言。这里集中放一份，方便逐步替换成 bindings 的官方类型。

export type Status = {
  state: string;
  pid: number;
  uptime: number;
  error: string;
  version: string;
  proxyOn: boolean;
  proxyAddr: string;
  mixedPort: number;
  apiPort: number;
  mode: string;
  bootErr: string;
  dataDir: string;
  tun: boolean;
  admin: boolean;
};

export type Conn = {
  id: string;
  host: string;
  dest: string;
  destIP: string;
  process: string;
  processPath: string;
  chains: string;
  rule: string;
  upload: number;
  download: number;
  start: string;
  network: string;
};

export type Traffic = {
  upTotal: number;
  downTotal: number;
  upRate: number;
  downRate: number;
};

export type NodeView = {
  name: string;
  type: string;
  delay: number;
  group: boolean;
  now: string;
  count: number;
};

export type SubInfo = {
  url: string;
  name: string;
  nodes: number;
  groups: number;
  rules: number;
  fetchedAt: number;
  insecure: boolean;
  upload: number;
  download: number;
  total: number;
  expire: number;
  webPage: string;
  updateHrs: number;
  error: string;
};

export type Tab = "overview" | "nodes" | "connections" | "logs" | "settings";

/** ListSubscriptions 返回的订阅条目。字段与后端 map 一一对应。 */
export type SubItem = {
  url: string;
  name: string;
  fetchedAt: number;
  upload: number;
  download: number;
  total: number;
  expire: number;
  insecure: boolean;
  active: boolean;
};
