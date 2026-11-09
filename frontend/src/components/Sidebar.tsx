import type { Tab } from "../types";
import { STATE_TEXT } from "../utils";

/** 导航图标。纯静态 SVG，和品牌 mark 同一套线条风格。 */
export function Icon({ name }: { name: Tab }) {
  const common = {
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.7,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
  };
  if (name === "overview") {
    return (
      <svg {...common}>
        <rect x="3" y="3" width="7" height="7" rx="1.6" />
        <rect x="14" y="3" width="7" height="7" rx="1.6" />
        <rect x="3" y="14" width="7" height="7" rx="1.6" />
        <rect x="14" y="14" width="7" height="7" rx="1.6" />
      </svg>
    );
  }
  if (name === "nodes") {
    return (
      <svg {...common}>
        <rect x="3" y="4" width="18" height="6" rx="2" />
        <rect x="3" y="14" width="18" height="6" rx="2" />
        <line x1="7" y1="7" x2="7.01" y2="7" />
        <line x1="7" y1="17" x2="7.01" y2="17" />
      </svg>
    );
  }
  if (name === "connections") {
    return (
      <svg {...common}>
        <path d="M9 17H7A5 5 0 0 1 7 7h2" />
        <path d="M15 7h2a5 5 0 0 1 0 10h-2" />
        <line x1="8" y1="12" x2="16" y2="12" />
      </svg>
    );
  }
  if (name === "logs") {
    return (
      <svg {...common}>
        <path d="M5 4h9l5 5v11a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z" />
        <path d="M14 4v5h5" />
        <line x1="8" y1="13" x2="15" y2="13" />
        <line x1="8" y1="17" x2="13" y2="17" />
      </svg>
    );
  }
  return (
    <svg {...common}>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-1.8-.3 1.6 1.6 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.6 1.6 0 0 0-1-1.5 1.6 1.6 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.6 1.6 0 0 0 .3-1.8 1.6 1.6 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.6 1.6 0 0 0 1.5-1 1.6 1.6 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.6 1.6 0 0 0 1.8.3H9a1.6 1.6 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 1 1.5 1.6 1.6 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0-.3 1.8V9a1.6 1.6 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1z" />
    </svg>
  );
}

const NAV: Array<[Tab, string]> = [
  ["overview", "概览"],
  ["nodes", "节点"],
  ["connections", "连接"],
  ["logs", "日志"],
  ["settings", "设置"],
];

type Props = {
  tab: Tab;
  onTabChange: (t: Tab) => void;
  connCount: number;
  nodeCount: number;
  tun: boolean;
  proxyOn: boolean;
  state: string;
  running: boolean;
  busy: boolean;
  onToggleProxy: () => void;
};

/** 左侧导航栏：品牌、五个导航项、底部代理开关与运行状态。 */
export default function Sidebar({
  tab,
  onTabChange,
  connCount,
  nodeCount,
  tun,
  proxyOn,
  state,
  running,
  busy,
  onToggleProxy,
}: Props) {
  return (
    <aside className="sidebar">
      <div className="brand">
        <div className="brand-mark">
          {/* 品牌位直接用应用图标，和 exe/托盘保持同一张脸 */}
          <img src="/appicon.png" alt="" width="32" height="32" />
        </div>
        <span className="brand-name">
          Route<span className="brand-accent">Pilot</span>
        </span>
      </div>

      <nav className="nav">
        {NAV.map(([id, label]) => (
          <div
            key={id}
            className={`nav-item ${tab === id ? "active" : ""}`}
            onClick={() => onTabChange(id)}
          >
            <Icon name={id} />
            <span>{label}</span>
            {id === "connections" && connCount > 0 && (
              <span className="nav-badge">{connCount}</span>
            )}
            {id === "nodes" && nodeCount > 0 && (
              <span className="nav-badge">{nodeCount}</span>
            )}
          </div>
        ))}
      </nav>

      <div className="sidebar-foot">
        {tun ? (
          // TUN 接管时系统代理开关没有意义（且被强制关闭），
          // 所以这里不放开关，只显示状态，点一下去设置页调整。
          <div
            className="proxy-row clickable"
            onClick={() => onTabChange("settings")}
            title="TUN 已在 IP 层接管全部流量，点击前往设置调整"
          >
            <span className="proxy-label" style={{ color: "var(--green)" }}>
              TUN 接管中
            </span>
            <span className="hint" style={{ marginLeft: 0, fontSize: 10.5 }}>
              详情
            </span>
          </div>
        ) : (
          <div className="proxy-row">
            <span className="proxy-label">系统代理</span>
            <button
              className={`switch ${proxyOn ? "on" : ""}`}
              onClick={onToggleProxy}
              disabled={!running || busy}
              aria-label="系统代理开关"
            />
          </div>
        )}
        <div className="proxy-row">
          <span className="proxy-label">{STATE_TEXT[state] ?? "未知"}</span>
          <span className={`dot ${state}`} />
        </div>
      </div>
    </aside>
  );
}
