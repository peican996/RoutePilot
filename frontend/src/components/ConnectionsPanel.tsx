import { useMemo, useState } from "react";

import type { Conn, NodeView } from "../types";
import { CONN_PAGE_SIZE, PROC_LIST_MAX } from "../constants";
import { fmtBytes, fmtDuration, procKeyIsNameOnly, procKeyLabel, procKeyPath } from "../utils";

type Policy = "proxy" | "direct" | "follow";
type SortKey = "" | "up" | "down";

type Props = {
  conns: Conn[];
  nodes: NodeView[];
  procRules: Record<string, string>;
  running: boolean;
  busy: boolean;
  onSetProc: (proc: string, policy: string) => void;
  onCloseConn: (id: string) => void;
  onCloseAll: () => void;
};

const POLICIES: Array<[Policy, string]> = [
  ["proxy", "代理"],
  ["direct", "直连"],
  ["follow", "跟随规则"],
];

// 内置组直接选没有意义（DIRECT 已由「直连」覆盖），列表里过滤掉。
const BUILTIN = new Set([
  "GLOBAL",
  "DIRECT",
  "REJECT",
  "REJECT-DROP",
  "COMPATIBLE",
  "PASS",
  "PASS-RULE",
  "PROXY",
]);

const TARGET_PREFIX = "target:";

/** 连接面板：进程策略 + 活动连接表。 */
export default function ConnectionsPanel({
  conns,
  nodes,
  procRules,
  running,
  busy,
  onSetProc,
  onCloseConn,
  onCloseAll,
}: Props) {
  const [procFilter, setProcFilter] = useState("");
  const [connSort, setConnSort] = useState<{ key: SortKey; desc: boolean }>({
    key: "",
    desc: true,
  });
  const [groupByProc, setGroupByProc] = useState(false);
  const [connLimit, setConnLimit] = useState(CONN_PAGE_SIZE);

  // 按进程身份聚合连接数。身份优先是完整路径，同名不同目录不会合并。
  // 已有规则但当前没连接的进程也要列出来，否则改不回"跟随规则"。
  const procs = useMemo(() => {
    const m = new Map<string, number>();
    for (const c of conns) {
      const key = c.processKey ?? "";
      if (!key) continue;
      m.set(key, (m.get(key) ?? 0) + 1);
    }
    for (const k of Object.keys(procRules)) {
      if (!m.has(k)) m.set(k, 0);
    }
    return [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  }, [conns, procRules]);

  // 连接里带有原始大小写的 processPath；键是小写规范化值，
  // 用这份映射让规则列表也能展示真实路径。
  const procPaths = useMemo(() => {
    const m = new Map<string, string>();
    for (const c of conns) {
      if (c.processKey && c.processPath && !m.has(c.processKey)) {
        m.set(c.processKey, c.processPath);
      }
    }
    return m;
  }, [conns]);

  const filteredProcs = useMemo(() => {
    const q = procFilter.trim().toLowerCase();
    if (!q) return procs;
    return procs.filter(
      ([key]) => key.toLowerCase().includes(q) || procKeyLabel(key).toLowerCase().includes(q),
    );
  }, [procs, procFilter]);

  // 进程「指定节点」下拉的可选目标：策略组排前面，节点排后面。
  const groupChoices = useMemo(
    () => nodes.filter((n) => n.group && !BUILTIN.has(n.name)),
    [nodes],
  );
  const nodeChoices = useMemo(
    () => nodes.filter((n) => !n.group && !BUILTIN.has(n.name)),
    [nodes],
  );

  // 默认沿用后端顺序，点表头才按流量排
  const sortedConns = useMemo(() => {
    if (!connSort.key) return conns;
    const list = [...conns];
    list.sort((a, b) => {
      if (connSort.key === "up")
        return connSort.desc ? b.upload - a.upload : a.upload - b.upload;
      return connSort.desc ? b.download - a.download : a.download - b.download;
    });
    return list;
  }, [conns, connSort]);

  const groupedConns = useMemo(() => {
    if (!groupByProc) return sortedConns;
    return [...sortedConns].sort((a, b) => a.process.localeCompare(b.process));
  }, [sortedConns, groupByProc]);

  // 连接多时只渲染前 N 条：每 1.2 秒刷新一次，DOM 节点过多会明显掉帧
  const visibleConns = useMemo(
    () => groupedConns.slice(0, connLimit),
    [groupedConns, connLimit],
  );

  const toggleConnSort = (key: "up" | "down") => {
    setConnSort((s) => (s.key === key ? { key, desc: !s.desc } : { key, desc: true }));
  };

  return (
    <>
      <div className="card" style={{ marginBottom: 14 }}>
        <div className="card-head">
          <span className="card-title">进程策略</span>
          <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
            <input
              className="input"
              placeholder="过滤…"
              value={procFilter}
              onChange={(e) => setProcFilter(e.target.value)}
              style={{ width: 130 }}
              spellCheck={false}
            />
            <span className="badge">
              {filteredProcs.length > PROC_LIST_MAX
                ? `显示前 ${PROC_LIST_MAX} 条 / 共 ${procs.length} 个程序`
                : `共 ${procs.length} 个程序`}{" "}
              · 改完即刻生效
            </span>
          </div>
        </div>
        {procs.length === 0 ? (
          <div style={{ fontSize: 12.5, color: "var(--text-faint)", lineHeight: 1.7 }}>
            还没有捕获到程序流量。开启系统代理或 TUN 之后，
            所有联网的程序都会出现在这里。
          </div>
        ) : filteredProcs.length === 0 ? (
          <div style={{ fontSize: 12.5, color: "var(--text-faint)", lineHeight: 1.7 }}>
            没有匹配「{procFilter}」的程序。
          </div>
        ) : (
          <div className="proc-list">
            {filteredProcs.slice(0, PROC_LIST_MAX).map(([key, count]) => {
              const value = procRules[key] ?? "follow";
              const isTarget = value.startsWith(TARGET_PREFIX);
              const target = isTarget ? value.slice(TARGET_PREFIX.length) : "";
              const targetValid = isTarget && nodes.some((n) => n.name === target);
              const activePolicy = isTarget ? "" : value;
              const fullPath = procPaths.get(key) || procKeyPath(key);
              const label = fullPath ? procKeyLabel(`path:${fullPath}`) : procKeyLabel(key);
              return (
                <div className="proc-row" key={key}>
                  <span className="proc-name" title={fullPath || label}>
                    {label}
                    {procKeyIsNameOnly(key) && (
                      <span
                        style={{ marginLeft: 6, fontSize: 10.5, color: "var(--text-faint)" }}
                        title="无法获取完整路径，同名程序会共享这条规则"
                      >
                        仅名称
                      </span>
                    )}
                  </span>
                  <span className="proc-count">{count > 0 ? `${count} 连接` : "未运行"}</span>
                  <div className="segmented sm">
                    {POLICIES.map(([p, pLabel]) => (
                      <button
                        key={p}
                        className={activePolicy === p ? "active" : ""}
                        onClick={() => onSetProc(key, p)}
                        disabled={busy}
                      >
                        {pLabel}
                      </button>
                    ))}
                  </div>
                  <select
                    className="input"
                    style={{ width: 118, flexShrink: 0 }}
                    value={isTarget ? value : ""}
                    onChange={(e) => {
                      const next = e.target.value;
                      if (next) onSetProc(key, next);
                    }}
                    disabled={busy}
                    title="指定该程序使用的节点或策略组"
                  >
                    <option value="">指定节点…</option>
                    {isTarget && <option value="follow">取消指定（跟随规则）</option>}
                    {isTarget && !targetValid && (
                      <option value={value}>{`已失效：${target}`}</option>
                    )}
                    {groupChoices.length > 0 && (
                      <optgroup label="策略组">
                        {groupChoices.map((g) => (
                          <option key={`g-${g.name}`} value={`${TARGET_PREFIX}${g.name}`}>
                            {g.name}
                          </option>
                        ))}
                      </optgroup>
                    )}
                    {nodeChoices.length > 0 && (
                      <optgroup label="节点">
                        {nodeChoices.map((n) => (
                          <option key={`n-${n.name}`} value={`${TARGET_PREFIX}${n.name}`}>
                            {n.name}
                          </option>
                        ))}
                      </optgroup>
                    )}
                  </select>
                </div>
              );
            })}
          </div>
        )}
      </div>

      <div
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          marginBottom: 10,
        }}
      >
        <span style={{ fontSize: 12.5, color: "var(--text-faint)" }}>
          {conns.length} 条活动连接
        </span>
        <button className="btn sm" onClick={onCloseAll} disabled={conns.length === 0 || busy}>
          全部断开
        </button>
      </div>

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th
                style={{ width: 130, cursor: "pointer", userSelect: "none" }}
                onClick={() => setGroupByProc((v) => !v)}
                title="点击按进程分组/还原"
              >
                程序 {groupByProc ? "◆" : ""}
              </th>
              <th>目标</th>
              <th style={{ width: 120 }}>规则</th>
              <th style={{ width: 190 }}>链路</th>
              <th
                style={{ width: 84, textAlign: "right", cursor: "pointer", userSelect: "none" }}
                onClick={() => toggleConnSort("up")}
                title="按上行流量排序"
              >
                上行 {connSort.key === "up" ? (connSort.desc ? "↓" : "↑") : ""}
              </th>
              <th
                style={{ width: 84, textAlign: "right", cursor: "pointer", userSelect: "none" }}
                onClick={() => toggleConnSort("down")}
                title="按下行流量排序"
              >
                下行 {connSort.key === "down" ? (connSort.desc ? "↓" : "↑") : ""}
              </th>
              <th style={{ width: 68, textAlign: "right" }}>时长</th>
              <th style={{ width: 56 }}></th>
            </tr>
          </thead>
          <tbody>
            {conns.length === 0 && (
              <tr>
                <td colSpan={8} className="empty">
                  {running ? "当前没有活动连接" : "内核未运行"}
                </td>
              </tr>
            )}
            {visibleConns.map((c) => (
              <tr key={c.id}>
                <td className="proc" title={c.processPath || c.processKey || c.process}>
                  {c.processKey ? procKeyLabel(c.processKey) : c.process}
                </td>
                <td title={c.host}>
                  <span className="mono" style={{ color: "var(--text-dim)" }}>
                    {c.host}
                  </span>
                </td>
                <td style={{ color: "var(--text-faint)", fontSize: 11.5 }}>{c.rule || "—"}</td>
                <td style={{ color: "var(--text-faint)", fontSize: 11.5 }}>{c.chains || "—"}</td>
                <td className="num">{fmtBytes(c.upload)}</td>
                <td className="num">{fmtBytes(c.download)}</td>
                <td className="num" style={{ color: "var(--text-faint)", fontSize: 11.5 }}>
                  {fmtDuration(c.start)}
                </td>
                <td>
                  <button
                    className="row-action"
                    onClick={() => onCloseConn(c.id)}
                    disabled={busy}
                    title="断开这条连接"
                  >
                    断开
                  </button>
                </td>
              </tr>
            ))}
            {groupedConns.length > visibleConns.length && (
              <tr>
                <td colSpan={8} style={{ textAlign: "center", padding: "10px 0" }}>
                  <button className="btn sm" onClick={() => setConnLimit((n) => n + CONN_PAGE_SIZE)}>
                    显示更多（还有 {groupedConns.length - visibleConns.length} 条）
                  </button>
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}
