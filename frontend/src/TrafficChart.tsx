// 流量历史图表。用纯 SVG 画，不引图表库 —— 一个面积图不需要那点依赖。
//
// 设计要点：
// - 上行青、下行蓝，堆叠显示，一眼看出总量和构成
// - 24 小时窗口，样本过多时按 10 分钟聚合，避免几百个点挤成一团
// - Y 轴自动缩放到最大值，留 15% 头部空间

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { CHART_TICK_MS } from "./constants";
import { fmtBytes } from "./utils";

type Sample = { t: number; u: number; d: number };

// 字节数 → 人类可读
function fmtHour(unix: number): string {
  const d = new Date(unix * 1000);
  return `${String(d.getHours()).padStart(2, "0")}:00`;
}

export default function TrafficChart({
  data,
  height = 170,
}: {
  data: Sample[];
  height?: number;
}) {
  // 时间窗要跟着墙上的钟走。不加这个 tick，图会冻结在首次渲染时的
  // 那个"最近 24 小时"，开久了右边缘就不动了。
  const [nowTick, setNowTick] = useState(() => Math.floor(Date.now() / 1000));
  useEffect(() => {
    const id = window.setInterval(() => setNowTick(Math.floor(Date.now() / 1000)), CHART_TICK_MS);
    return () => window.clearInterval(id);
  }, []);

  const { points, maxV, totalUp, totalDown, peakLabel } = useMemo(() => {
    // 只看最近 24 小时
    const now = nowTick;
    const from = now - 24 * 3600;
    const recent = data.filter((s) => s.t >= from);

    // 样本过多时按 10 分钟聚合
    const bucket = 10 * 60;
    const agg = new Map<number, { u: number; d: number }>();
    for (const s of recent) {
      const key = Math.floor(s.t / bucket) * bucket;
      const cur = agg.get(key) || { u: 0, d: 0 };
      cur.u += s.u;
      cur.d += s.d;
      agg.set(key, cur);
    }
    const pts = [...agg.entries()]
      .sort((a, b) => a[0] - b[0])
      .map(([t, v]) => ({ t, u: v.u, d: v.d }));

    const totals = pts.reduce(
      (acc, p) => ({ u: acc.u + p.u, d: acc.d + p.d }),
      { u: 0, d: 0 }
    );
    const peak = pts.reduce((m, p) => Math.max(m, p.u + p.d), 0);

    return {
      points: pts,
      maxV: Math.max(peak, 1),
      totalUp: totals.u,
      totalDown: totals.d,
      peakLabel: fmtBytes(peak),
    };
  }, [data, nowTick]);

  // viewBox 的宽高必须和实际渲染尺寸一致。
  // 之前固定 600 宽再配 preserveAspectRatio="none"，600 会被横向拉伸到
  // 容器实际宽度（常见 1.3~1.7 倍），刻度文字跟着变形。
  // 用 callback ref 而不是 useEffect 里读 ref.current：
  // 空态和正常态是两个不同的 div，节点会换，只在挂载时观察一次会盯着已经
  // 被卸载的旧节点，数据来了宽度永远测不到。
  const [W, setW] = useState(600);
  const roRef = useRef<ResizeObserver | null>(null);
  const setWrap = useCallback((el: HTMLDivElement | null) => {
    roRef.current?.disconnect();
    roRef.current = null;
    if (!el) return;
    const measure = () => setW(Math.max(320, Math.round(el.clientWidth || 600)));
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    roRef.current = ro;
  }, []);
  useEffect(() => () => roRef.current?.disconnect(), []);

  if (points.length === 0) {
    return (
      <div
        ref={setWrap}
        style={{
          height,
          display: "grid",
          placeItems: "center",
          color: "var(--text-faint)",
          fontSize: 12,
        }}
      >
        还没有流量记录，跑一会儿就会有数据
      </div>
    );
  }

  const H = height;
  const padL = 8;
  const padR = 8;
  const padB = 22;
  const padT = 10;
  const innerW = W - padL - padR;
  const innerH = H - padT - padB;

  const t0 = points[0].t;
  const t1 = points[points.length - 1].t || t0 + 1;
  const x = (t: number) => padL + ((t - t0) / (t1 - t0 || 1)) * innerW;
  const y = (v: number) => padT + innerH - (v / maxV) * innerH;

  // 堆叠面积：下行在下、上行叠在上面
  const totalPath = points.map((p) => p.u + p.d);
  const areaTop = points
    .map((p, i) => `${i === 0 ? "M" : "L"} ${x(p.t).toFixed(1)} ${y(totalPath[i]).toFixed(1)}`)
    .join(" ");
  const areaBottom = points
    .slice()
    .reverse()
    .map((p) => `L ${x(p.t).toFixed(1)} ${y(0).toFixed(1)}`)
    .join(" ");
  const area = `${areaTop} ${areaBottom} Z`;

  // 下行单独一层（更清晰）
  const downTop = points
    .map((p, i) => `${i === 0 ? "M" : "L"} ${x(p.t).toFixed(1)} ${y(p.d).toFixed(1)}`)
    .join(" ");
  const downArea = `${downTop} L ${x(t1).toFixed(1)} ${y(0).toFixed(1)} L ${x(t0).toFixed(1)} ${y(0).toFixed(1)} Z`;

  // 时间刻度：每 4 小时一个
  const ticks: number[] = [];
  const start = Math.ceil(t0 / (4 * 3600)) * 4 * 3600;
  for (let t = start; t <= t1; t += 4 * 3600) ticks.push(t);

  return (
    <div ref={setWrap}>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"        style={{ width: "100%", height, display: "block" }}
        role="img"
        aria-label="24 小时流量图"
      >
        <defs>
          <linearGradient id="tc-up" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--accent)" stopOpacity="0.55" />
            <stop offset="100%" stopColor="var(--accent)" stopOpacity="0.08" />
          </linearGradient>
          <linearGradient id="tc-down" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--violet)" stopOpacity="0.5" />
            <stop offset="100%" stopColor="var(--violet)" stopOpacity="0.06" />
          </linearGradient>
        </defs>

        {/* 网格线 */}
        {[0.25, 0.5, 0.75, 1].map((f) => (
          <line
            key={f}
            x1={padL}
            x2={W - padR}
            y1={y(maxV * f)}
            y2={y(maxV * f)}
            stroke="var(--border-soft)"
            strokeWidth={1}
          />
        ))}

        {/* 堆叠面积 */}
        <path d={area} fill="url(#tc-up)" />
        <path d={downArea} fill="url(#tc-down)" />

        {/* 顶部轮廓线。preserveAspectRatio="none" 会把坐标轴拉伸，
            stroke 用 non-scaling-stroke 才不会跟着变粗/变细。 */}
        <path
          d={areaTop}
          fill="none"
          stroke="var(--accent)"
          strokeWidth={1.5}
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />
        <path
          d={downTop}
          fill="none"
          stroke="var(--violet)"
          strokeWidth={1.2}
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />

        {/* 时间刻度 */}
        {ticks.map((t) => (
          <g key={t}>
            <line
              x1={x(t)}
              x2={x(t)}
              y1={padT}
              y2={padT + innerH}
              stroke="var(--border-soft)"
              strokeWidth={1}
              strokeDasharray="2 4"
            />
            <text
              x={x(t)}
              y={H - 6}
              textAnchor="middle"
              fill="var(--text-faint)"
              fontSize={10}
            >
              {fmtHour(t)}
            </text>
          </g>
        ))}
      </svg>

      {/* 图例与汇总 */}
      <div
        style={{
          display: "flex",
          gap: 16,
          marginTop: 8,
          fontSize: 11.5,
          color: "var(--text-dim)",
          flexWrap: "wrap",
        }}
      >
        <span>
          <span style={{ color: "var(--accent)" }}>●</span> 上行 {fmtBytes(totalUp)}
        </span>
        <span>
          <span style={{ color: "var(--violet)" }}>●</span> 下行 {fmtBytes(totalDown)}
        </span>
        <span>峰值 {peakLabel}/10min</span>
      </div>
    </div>
  );
}
