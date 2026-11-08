import type { Tab } from "../types";
import { modeLabel } from "../utils";

const TITLES: Record<Tab, string> = {
  overview: "概览",
  nodes: "节点与订阅",
  connections: "活动连接",
  logs: "内核日志",
  settings: "设置",
};

type Props = {
  tab: Tab;
  mode: string;
  running: boolean;
  busy: boolean;
  onToggleKernel: () => void;
};

/** 顶部栏：当前页标题、代理模式标签、内核启停按钮。 */
export default function Topbar({ tab, mode, running, busy, onToggleKernel }: Props) {
  return (
    <header className="topbar">
      <span className="topbar-title">{TITLES[tab]}</span>
      <div className="topbar-right">
        <span
          className={`status-pill mode-${mode || "rule"}`}
          title="代理模式，在「设置」页切换"
        >
          <span className={`dot ${running ? "running" : ""}`} />
          {modeLabel(mode)}
        </span>
        <button
          className={`btn ${running ? "danger" : "primary"}`}
          onClick={onToggleKernel}
          disabled={busy}
        >
          {running ? "停止内核" : "启动内核"}
        </button>
      </div>
    </header>
  );
}
