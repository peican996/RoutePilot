// 界面通用的格式化工具。从 App.tsx 抽出来，避免同一个函数在多处各写一份
// （之前 fmtBytes 在 App 和 TrafficChart 里各有一份，且对 0 的处理不一致）。

/** 字节数转人类可读。0 统一返回 "0 B"，保持各处口径一致。 */
export function fmtBytes(n: number): string {
  if (!n || n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

/** 内核版本号统一格式：mihomo 有的版本带 v、有的不带，避免出现 "vv1.19.33"。 */
export function fmtVersion(v?: string): string {
  if (!v) return "—";
  return `v${v.replace(/^v+/i, "")}`;
}

/** 代理模式的中文名。三处展示共用，避免各写一份还漏了直连。 */
export function modeLabel(mode?: string): string {
  switch (mode) {
    case "global":
      return "全局模式";
    case "direct":
      return "直连模式";
    case "rule":
      return "规则模式";
    default:
      return "—";
  }
}

/** 运行时长（秒）转中文描述。 */
export function fmtUptime(sec: number): string {
  if (sec <= 0) return "—";
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (h > 0) return `${h} 小时 ${m} 分`;
  if (m > 0) return `${m} 分 ${s} 秒`;
  return `${s} 秒`;
}

/** 时间戳转"多久以前"。 */
export function fmtAgo(ms: number): string {
  if (!ms) return "从未";
  const diff = Math.floor((Date.now() - ms) / 1000);
  if (diff < 60) return "刚刚";
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

/** 从订阅地址里取主机名，取不到就截断原串。 */
export function subHost(url: string): string {
  try {
    return new URL(url).hostname;
  } catch {
    return url.length > 30 ? `${url.slice(0, 30)}…` : url;
  }
}

/**
 * 订阅到期时间。0 表示不限期。
 *
 * 后端 `subscription-userinfo` 里的 expire 是 Unix **秒**，这里统一在入口
 * 归一成毫秒：小于 1e12 的值按秒处理。之前直接当毫秒用，所有带 expire
 * 头的订阅都显示成 1970 年已过期。
 */
export function fmtExpire(ts: number): { text: string; days: number; warn: boolean } {
  if (!ts) return { text: "永久", days: Infinity, warn: false };
  const ms = ts < 1e12 ? ts * 1000 : ts;
  const days = Math.ceil((ms - Date.now()) / 86400000);
  const d = new Date(ms);
  const text = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(
    d.getDate(),
  ).padStart(2, "0")}`;
  if (days < 0) return { text: `已过期（${text}）`, days, warn: true };
  return { text: `${text} 到期`, days, warn: days <= 7 };
}

/** 连接已持续的时长。接口给的是开始时间字符串。 */
export function fmtDuration(start: string): string {
  if (!start) return "—";
  const t = new Date(start).getTime();
  if (Number.isNaN(t)) return "—";
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

/** 内核生命周期状态的中文名。 */
export const STATE_TEXT: Record<string, string> = {
  running: "运行中",
  stopped: "已停止",
  starting: "启动中",
  error: "异常",
};
