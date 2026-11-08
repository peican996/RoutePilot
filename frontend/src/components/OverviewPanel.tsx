// 概览面板：内核运行状态、实时流量、24 小时曲线。

import TrafficChart from "../TrafficChart";
import type { Conn, Status, Traffic } from "../types";
import { STATE_TEXT, fmtBytes, fmtUptime, fmtVersion, modeLabel } from "../utils";

type Props = {
  status: Status;
  traffic: Traffic;
  conns: Conn[];
  activeNode: string;
  trafficHistory: Array<{ t: number; u: number; d: number }>;
};

export default function OverviewPanel({
  status,
  traffic,
  conns,
  activeNode,
  trafficHistory,
}: Props) {
  return (
    <>
  <div className="grid-2">
    <div className="card">
      <div className="card-head">
        <span className="card-title">内核状态</span>
        <span className="status-pill">
          <span className={`dot ${status.state}`} />
          {STATE_TEXT[status.state] ?? "未知"}
        </span>
      </div>
      <div className="stat-value">
        {fmtVersion(status.version)}
      </div>
      <div className="stat-sub">
        {status.pid > 0 ? `PID ${status.pid}` : "未运行"} · 已运行{" "}
        {fmtUptime(status.uptime)}
      </div>
      <div style={{ marginTop: 12 }}>
        <div className="kv">
          <span className="kv-k">混合端口</span>
          <span className="kv-v">{status.mixedPort || "—"}</span>
        </div>
        <div className="kv">
          <span className="kv-k">控制 API</span>
          <span className="kv-v">{status.apiPort || "—"}</span>
        </div>
        <div className="kv">
          <span className="kv-k">工作模式</span>
          <span
            className="kv-v"
            style={status.mode === "direct" ? { color: "var(--amber)" } : undefined}
          >
            {status.mode === "direct"
              ? "直连（代理未生效）"
              : modeLabel(status.mode)}
          </span>
        </div>
        <div className="kv">
          <span className="kv-k">当前节点</span>
          <span className="kv-v">{activeNode || "—"}</span>
        </div>
      </div>
    </div>

    <div className="card">
      <div className="card-head">
        <span className="card-title">实时流量</span>
        <span style={{ fontSize: 11.5, color: "var(--text-faint)" }}>
          {conns.length} 条连接
        </span>
      </div>
      <div style={{ display: "flex", gap: 28 }}>
        <div>
          <div className="stat-value" style={{ color: "var(--green)" }}>
            {fmtBytes(traffic.downRate)}
            <span className="stat-unit">/s</span>
          </div>
          <div className="stat-sub">下行 · 累计 {fmtBytes(traffic.downTotal)}</div>
        </div>
        <div>
          <div className="stat-value" style={{ color: "var(--accent)" }}>
            {fmtBytes(traffic.upRate)}
            <span className="stat-unit">/s</span>
          </div>
          <div className="stat-sub">上行 · 累计 {fmtBytes(traffic.upTotal)}</div>
        </div>
      </div>
      <div style={{ marginTop: 14 }}>
        <div className="kv">
          <span className="kv-k">系统代理</span>
          <span className="kv-v">
            {status.proxyOn ? status.proxyAddr : "未开启"}
          </span>
        </div>
      </div>
    </div>
  </div>

  <div className="card">
    <div className="card-head">
      <span className="card-title">24 小时流量</span>
      <span style={{ fontSize: 11.5, color: "var(--text-faint)" }}>
        每格 10 分钟
      </span>
    </div>
    <TrafficChart data={trafficHistory} />
  </div>

  <div className="section-gap" />

  <div className="card">
    <div className="card-head">
      <span className="card-title">按程序分流 · 即将解锁</span>
    </div>
    <div style={{ fontSize: 12.5, color: "var(--text-dim)", lineHeight: 1.75 }}>
      内核已开启进程识别（<span className="mono">find-process-mode: always</span>）。
      去「连接」页可以看到每条连接对应的程序。下一步会在此基础上做学习式分流：
      新程序首次联网时问一次，之后自动记住。
    </div>
  </div>
    </>
  );
}
