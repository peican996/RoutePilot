import { useEffect, useMemo, useRef, useState } from "react";

import { LOG_FOLLOW_TOLERANCE } from "../constants";

type Props = {
  logs: string[];
  /** 后端告知：更早的日志已滚出缓冲区 */
  truncated?: boolean;
  onExport: () => void;
  onClear: () => void;
};

type Level = "all" | "err" | "warn";

const LEVELS: Array<[Level, string]> = [
  ["all", "全部"],
  ["err", "错误"],
  ["warn", "警告"],
];

/** 内核日志面板：搜索、级别过滤、导出/清空，以及"接近底部才自动跟随"。 */
export default function LogPanel({ logs, truncated, onExport, onClear }: Props) {
  const [filter, setFilter] = useState("");
  const [level, setLevel] = useState<Level>("all");

  const viewRef = useRef<HTMLDivElement | null>(null);
  // 用户手动往上翻看历史时停止自动滚底，回到底部附近再恢复跟随
  const followRef = useRef(true);

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return logs.filter((line) => {
      const low = line.toLowerCase();
      if (level === "err" && !(low.includes("error") || low.includes("fatal"))) return false;
      if (level === "warn" && !low.includes("warn")) return false;
      if (q && !low.includes(q)) return false;
      return true;
    });
  }, [logs, filter, level]);

  useEffect(() => {
    const el = viewRef.current;
    if (el && followRef.current) el.scrollTop = el.scrollHeight;
  }, [filtered]);

  return (
    <>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 8,
          marginBottom: 10,
          flexWrap: "wrap",
        }}
      >
        <input
          className="input"
          placeholder="搜索日志…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          style={{ flex: 1, minWidth: 160 }}
          spellCheck={false}
        />
        <div className="seg">
          {LEVELS.map(([k, label]) => (
            <button
              key={k}
              className={`seg-item ${level === k ? "on" : ""}`}
              onClick={() => setLevel(k)}
            >
              {label}
            </button>
          ))}
        </div>
        <span className="hint" style={{ whiteSpace: "nowrap" }}>
          {filtered.length}/{logs.length}
        </span>
        <button className="btn sm" onClick={onExport}>
          导出
        </button>
        <button className="btn sm" onClick={onClear}>
          清空
        </button>
      </div>

      <div
        className="log-view"
        ref={viewRef}
        onScroll={(e) => {
          const el = e.currentTarget;
          followRef.current =
            el.scrollHeight - el.scrollTop - el.clientHeight < LOG_FOLLOW_TOLERANCE;
        }}
      >
        {truncated && (
          <span className="log-line" style={{ opacity: 0.65 }}>
            ⋯ 更早的日志已被滚出缓冲区
          </span>
        )}
        {logs.length === 0 && <span className="log-line">等待内核输出…</span>}
        {logs.length > 0 && filtered.length === 0 && (
          <span className="log-line">没有匹配的日志</span>
        )}
        {filtered.map((line, i) => {
          const lower = line.toLowerCase();
          const cls =
            lower.includes("error") || lower.includes("fatal")
              ? "err"
              : lower.includes("warn")
              ? "warn"
              : "";
          return (
            <span key={`${i}:${line}`} className={`log-line ${cls}`}>
              {line}
            </span>
          );
        })}
      </div>
    </>
  );
}
