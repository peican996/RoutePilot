import { useEffect, useMemo, useRef, useState } from "react";
import { WORLD_PATH, WORLD_W, WORLD_H } from "./worldPath";
import {
  resolvePlace,
  HOME,
  project as projectRaw,
  delayColor,
  delayText,
} from "./mapData";
import { MAP_MERGE_RADIUS, MAP_ZOOM_STEP } from "./constants";
import { placeLabel, type LabelBox } from "./mapLayout";

// 把地图数据模块的通用投影绑定到本组件使用的画布尺寸。
const project = (lon: number, lat: number) => projectRaw(lon, lat, WORLD_W, WORLD_H);



export type Target = {
  lon: number;
  lat: number;
  country: string;
  ip: string;
  host: string;
};

// 一个实际出口：主策略组出口，或某个进程指定到的节点。
// 地图按出口分组画线，而不是只画"当前节点"一条。
export type MapExit = {
  name: string;
  main: boolean;
  /** 指定到这个出口的进程名，没有流量的配置也会带上。 */
  procs: string[];
  /** 当前走这个出口的连接数。 */
  conns: number;
  /** 这些连接的目标落点，已经去重并限量。 */
  targets: Target[];
};

type Props = {
  names: string[];
  active: string;
  exits?: MapExit[];
  onPick?: (name: string) => void;
  /** 节点名 → 延迟（毫秒）。用来给节点着色：绿/黄/红。 */
  delays?: Record<string, number>;
  /** 节点名 → 附加信息，悬停时显示。 */
  nodeMeta?: Record<string, { type: string; delay: number }>;
  /** 点地图上的测速按钮。 */
  onRefresh?: () => void;
  /** 正在测速中。 */
  refreshing?: boolean;
  /** 单点测某个节点的延迟。 */
  onTestNode?: (name: string) => void;
  /** 当前正在单测的节点名，用于显示转圈。 */
  testingNode?: string | null;
  /** 地图上"本机"的位置。默认东莞，可在设置里改。 */
  home?: { lon: number; lat: number };
};

// 延迟 → 颜色。和节点列表里的口径一致：<200 绿、<500 黄、其余红。
//
// 注意区分两种"没有数字"的情况：
//   undefined = 压根没测过
//   <= 0      = 测了，但超时/不通

export default function NodeMap({
  names,
  active,
  exits,
  onPick,
  delays,
  nodeMeta,
  onRefresh,
  refreshing = false,
  onTestNode,
  testingNode,
  home = HOME,
}: Props) {
  // 悬停浮层。目标点和节点共用一个，只是行内容不同。
  const [tip, setTip] = useState<{
    x: number;
    y: number;
    rows: Array<{ cls: string; text: string }>;
  } | null>(null);

  // --- 地图缩放 / 平移 ---------------------------------------------------
  const svgRef = useRef<SVGSVGElement>(null);
  // 视口：x/y 是左上角在世界坐标里的位置，w/h 是视口大小。
  // w 越小 = 放得越大。
  const DEFAULT_VIEW = { x: 0, y: 20, w: WORLD_W, h: 282 };
  const [view, setView] = useState(DEFAULT_VIEW);
  const dragRef = useRef<{
    x: number;
    y: number;
    vx: number;
    vy: number;
    sx: number;
    sy: number;
    moved: boolean;
  } | null>(null);

  // 合并点被点击时展开的选点菜单。
  const [picker, setPicker] = useState<{ x: number; y: number; items: string[] } | null>(
    null
  );

  // 点菜单以外的区域就收起。点在菜单或节点上不算——
  // 否则点节点切换单选菜单时，mousedown 会先把菜单关掉。
  useEffect(() => {
    if (!picker) return;
    const onDown = (e: MouseEvent) => {
      const el = e.target as Element | null;
      if (el && (el.closest(".nm-picker") || el.closest(".nm-node"))) return;
      setPicker(null);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [picker]);

  // 滚轮缩放。React 的 onWheel 是 passive 的、preventDefault 无效，
  // 必须用原生监听器才能阻止页面跟着滚动。
  useEffect(() => {
    const el = svgRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      const rect = el.getBoundingClientRect();
      const mx = (e.clientX - rect.left) / rect.width;
      const my = (e.clientY - rect.top) / rect.height;
      setView((v) => {
        const factor = e.deltaY < 0 ? 1.18 : 1 / 1.18;
        const minW = 70; // 放到最大时的视口宽度
        const nw = Math.min(WORLD_W, Math.max(minW, v.w / factor));
        const nh = (nw / WORLD_W) * 282;
        // 以鼠标位置为锚点缩放，这样缩放中心跟着指针走
        return {
          x: v.x + (v.w - nw) * mx,
          y: v.y + (v.h - nh) * my,
          w: nw,
          h: nh,
        };
      });
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, []);

  // 合并阈值随缩放级别变化。
  //
  // 全球视图下 15 个 SVG 单位的点会合并成一个；放大之后阈值等比缩小，
  // 原本合并的点会自然分开——这正是"放大后能看到被隐藏的点"的关键。
  const mergeRadius = useMemo(() => {
    return MAP_MERGE_RADIUS * (view.w / WORLD_W);
  }, [view.w]);

  // 解析节点位置。
  //
  // 东亚节点天然密集（香港/台湾/日本/新加坡在地图上就一小块），
  // 视觉距离太近的合并成一个点，但保留成员列表，点击时能展开选择。
  const placed = useMemo(() => {
    type Placed = {
      x: number;
      y: number;
      label: string;
      name: string;
      names: string[];
      count: number;
    };
    const raw: Placed[] = [];
    for (const n of names) {
      const p = resolvePlace(n);
      if (!p) continue;
      raw.push({
        ...project(p.lon, p.lat),
        label: p.label,
        name: n,
        names: [n],
        count: 1,
      });
    }
    const merged: Placed[] = [];
    for (const item of raw) {
      const near = merged.find(
        (m) => Math.hypot(m.x - item.x, m.y - item.y) < mergeRadius
      );
      if (near) {
        near.count += 1;
        near.names.push(item.name);
        continue;
      }
      merged.push(item);
    }
    return merged;
  }, [names, mergeRadius]);

  const activePlace = active ? resolvePlace(active) : null;
  const activeXY = activePlace ? project(activePlace.lon, activePlace.lat) : null;
  const homeXY = project(home.lon, home.lat);

  // 把出口解析成地图坐标。解析不到位置的（DIRECT、已下线的节点）直接跳过。
  const exitPlaces = useMemo(() => {
    const out: Array<{
      exit: MapExit;
      x: number;
      y: number;
      label: string;
    }> = [];
    for (const e of exits ?? []) {
      const p = resolvePlace(e.name);
      if (!p) continue;
      const xy = project(p.lon, p.lat);
      out.push({ exit: e, ...xy, label: p.label });
    }
    return out;
  }, [exits]);

  const exitByName = useMemo(() => {
    const m = new Map<string, MapExit>();
    for (const e of exits ?? []) m.set(e.name, e);
    return m;
  }, [exits]);

  // 拖动平移。用 pointer events，鼠标和触屏都能拖。
  // 拖完 3px 以内算点击，超过算拖动——否则用户想点节点会误触发平移。
  const justDraggedRef = useRef(false);
  // 拖拽中改变光标。用 state 而不是渲染期读 dragRef：ref 变化不触发重渲染，
  // 逻辑上也不该在 render 里读可变 ref。
  const [dragging, setDragging] = useState(false);
  const onPointerDown = (e: React.PointerEvent) => {
    const el = svgRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    dragRef.current = {
      x: e.clientX,
      y: e.clientY,
      vx: view.x,
      vy: view.y,
      sx: view.w / rect.width,
      sy: view.h / rect.height,
      moved: false,
    };
    setDragging(true);
  };
  const onPointerMove = (e: React.PointerEvent) => {
    const d = dragRef.current;
    if (!d) return;
    const dx = e.clientX - d.x;
    const dy = e.clientY - d.y;
    if (Math.abs(dx) > 3 || Math.abs(dy) > 3) d.moved = true;
    setView((v) => ({ ...v, x: d.vx - dx * d.sx, y: d.vy - dy * d.sy }));
  };
  const onPointerUp = () => {
    justDraggedRef.current = dragRef.current?.moved ?? false;
    dragRef.current = null;
    setDragging(false);
  };
  const resetView = () => {
    setView(DEFAULT_VIEW);
    setPicker(null);
  };
  const zoomBy = (factor: number) => {
    setView((v) => {
      const nw = Math.min(WORLD_W, Math.max(70, v.w / factor));
      const nh = (nw / WORLD_W) * 282;
      // 缩放中心取视口中心
      return {
        x: v.x + (v.w - nw) / 2,
        y: v.y + (v.h - nh) / 2,
        w: nw,
        h: nh,
      };
    });
  };

  // 尺寸补偿系数。
  //
  // SVG 里的半径、线宽、字号都是"世界坐标单位"，放大后视觉上会跟着变大
  // ——所以放大时点会变成一个大球、线会变成一条宽带。
  // k 是"世界单位 → 屏幕像素"的缩放比，所有视觉尺寸乘上它，
  // 屏幕上的大小就恒定了，跟放大倍数无关。
  const k = view.w / WORLD_W;
  const labels = useMemo(() => {
    const taken: LabelBox[] = [];
    const out: Array<{ key: string; x: number; y: number; text: string; cls: string }> = [];

    out.push({
      key: "home",
      ...placeLabel(homeXY.x, homeXY.y, "本机", taken, k),
      text: "本机",
      cls: "nm-label home",
    });

    if (activeXY && activePlace) {
      out.push({
        key: "active",
        ...placeLabel(activeXY.x, activeXY.y, activePlace.label, taken, k),
        text: activePlace.label,
        cls: "nm-label",
      });
    }

    // 进程指定出口单独标出来，没有流量时也能看出"这里还有一个出口"。
    for (const ep of exitPlaces) {
      if (ep.exit.main) continue;
      out.push({
        key: `exit-${ep.exit.name}`,
        ...placeLabel(ep.x, ep.y, ep.label, taken, k),
        text: ep.label,
        cls: "nm-label proc",
      });
    }

    // 目标点只标一次。多出口可能连到同一个国家/城市，重复标注没有意义。
    const seen = new Set<string>();
    const drawn: Array<{ lon: number; lat: number; country: string }> = [];
    for (const ep of exitPlaces) {
      for (const t of ep.exit.targets) {
        const key = `${t.lon},${t.lat}`;
        if (seen.has(key)) continue;
        seen.add(key);
        drawn.push(t);
      }
    }
    for (const t of drawn.slice(0, 6)) {
      const tp = project(t.lon, t.lat);
      out.push({
        key: `t-${t.lon},${t.lat}`,
        ...placeLabel(tp.x, tp.y, t.country, taken, k),
        text: t.country,
        cls: "nm-label small",
      });
    }

    return out;
  }, [homeXY, activeXY, activePlace, exitPlaces, k]);

  return (
    <div className="nodemap-wrap">
      <svg
        ref={svgRef}
        className="nodemap"
        viewBox={`${view.x} ${view.y} ${view.w} ${view.h}`}
        preserveAspectRatio="xMidYMid slice"
        role="img"
        aria-label="节点分布地图"
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerLeave={onPointerUp}
        style={{ cursor: dragging ? "grabbing" : "grab", touchAction: "none" }}
      >
        <defs>
          <radialGradient id="nm-glow" cx="50%" cy="50%" r="50%">
            <stop offset="0%" stopColor="var(--accent)" stopOpacity="0.85" />
            <stop offset="100%" stopColor="var(--accent)" stopOpacity="0" />
          </radialGradient>
          <radialGradient id="nm-glow-proc" cx="50%" cy="50%" r="50%">
            <stop offset="0%" stopColor="var(--amber)" stopOpacity="0.8" />
            <stop offset="100%" stopColor="var(--amber)" stopOpacity="0" />
          </radialGradient>
          <linearGradient id="nm-line" x1="0" y1="0" x2="1" y2="0">
            <stop offset="0%" stopColor="var(--accent)" stopOpacity="0.1" />
            <stop offset="100%" stopColor="var(--accent)" stopOpacity="0.8" />
          </linearGradient>
          <linearGradient id="nm-landfill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="#2a3a54" />
            <stop offset="100%" stopColor="#1d2739" />
          </linearGradient>
        </defs>

        {/* 真实世界陆地轮廓（Natural Earth 110m，已抽稀） */}
        <path className="nm-land" d={WORLD_PATH} />

        {/* 本机 → 各出口，以及出口 → 目标。
            主出口用强调色；进程指定出口用琥珀色，没有流量时线条变淡。 */}
        {exitPlaces.map(({ exit, x, y }) => {
          const proc = !exit.main;
          const path = `M ${homeXY.x} ${homeXY.y} Q ${(homeXY.x + x) / 2} ${
            Math.min(homeXY.y, y) - 40
          } ${x} ${y}`;
          const dur = proc ? 3.4 : exit.conns > 8 ? 1.7 : 2.9;
          // 连接越多光点越密。最多 4 颗，再多就显得满屏乱飞了。
          const count = proc
            ? exit.conns > 0
              ? 2
              : 0
            : exit.conns > 12
              ? 4
              : exit.conns > 4
                ? 3
                : exit.conns > 0
                  ? 2
                  : 0;
          return (
            <g key={exit.name}>
              <path
                className={proc ? "nm-line proc" : "nm-line"}
                d={path}
                fill="none"
                stroke={proc ? "var(--amber)" : "url(#nm-line)"}
                strokeWidth={(proc ? 1.2 : 1.6) * k}
                strokeDasharray={
                  proc && exit.conns === 0 ? `${2 * k} ${4 * k}` : undefined
                }
                opacity={proc && exit.conns === 0 ? 0.4 : 1}
              />
              {count > 0 &&
                Array.from({ length: count }, (_, i) => (
                  <circle
                    key={i}
                    className={proc ? "nm-particle proc" : "nm-particle"}
                    r={(proc ? 2 : 2.6) * k}
                    fill={proc ? "var(--amber)" : "var(--accent)"}
                  >
                    <animateMotion
                      dur={`${dur}s`}
                      begin={`${((i * dur) / count).toFixed(2)}s`}
                      repeatCount="indefinite"
                      path={path}
                    />
                  </circle>
                ))}

              {/* 出口 → 连接目标（第二段）。虚线弱化处理，
                  免得和主路径抢视觉焦点。 */}
              {exit.targets.map((t) => {
                const tp = project(t.lon, t.lat);
                const mx = (x + tp.x) / 2;
                const my = (y + tp.y) / 2 - 16;
                return (
                  <g key={`${exit.name}-${t.lon},${t.lat}`}>
                    <path
                      className="nm-line second"
                      d={`M ${x} ${y} Q ${mx} ${my} ${tp.x} ${tp.y}`}
                      fill="none"
                      stroke={proc ? "var(--amber)" : "var(--text-faint)"}
                      strokeWidth={1 * k}
                      strokeDasharray={`${3 * k} ${6 * k}`}
                      opacity={proc ? 0.4 : 0.55}
                    />
                    {/* 命中区比可见点大，否则鼠标很难停上去 */}
                    <circle
                      cx={tp.x}
                      cy={tp.y}
                      r={9 * k}
                      fill="transparent"
                      className="nm-hit"
                      onMouseEnter={(e) =>
                        setTip({
                          x: e.clientX,
                          y: e.clientY,
                          rows: [
                            { cls: "country", text: t.country },
                            { cls: "ip", text: t.ip },
                            { cls: "host", text: `出口 ${exit.name}` },
                            ...(t.host ? [{ cls: "host", text: t.host }] : []),
                          ],
                        })
                      }
                      onMouseMove={(e) =>
                        setTip((cur) => (cur ? { ...cur, x: e.clientX, y: e.clientY } : cur))
                      }
                      onMouseLeave={() => setTip(null)}
                    />
                    <circle
                      cx={tp.x}
                      cy={tp.y}
                      r={2.2 * k}
                      className="nm-target"
                      style={proc ? { fill: "var(--amber)", opacity: 0.8 } : undefined}
                    />
                  </g>
                );
              })}
            </g>
          );
        })}

        {/* 其他节点 */}
        {placed.map((p) => {
          // 激活节点可能被合并进簇里（p.x/y 是簇中心），必须按名字判断，
          // 否则当前出口在合并后高亮就丢了。
          const isActive = active ? p.names.includes(active) : false;
          // 合并簇里只要有一个进程指定出口，就整簇标成琥珀色。
          const procExitsHere = p.names
            .map((n) => exitByName.get(n))
            .filter((e): e is MapExit => Boolean(e && !e.main));
          const isProc = !isActive && procExitsHere.length > 0;
          const d = delays?.[p.name];
          const meta = nodeMeta?.[p.name];
          const rows: Array<{ cls: string; text: string }> = [
            { cls: "country", text: p.label },
            { cls: "ip", text: p.name },
          ];
          if (meta) {
            rows.push({
              cls: "host",
              text: `${meta.type} · ${delayText(meta.delay)}`,
            });
          }
          if (isProc) {
            const procs = [...new Set(procExitsHere.flatMap((e) => e.procs))];
            const count = procExitsHere.reduce((n, e) => n + e.conns, 0);
            if (procs.length > 0) {
              rows.push({
                cls: "host",
                text: `指定：${procs.slice(0, 3).join("、")}${procs.length > 3 ? "…" : ""}`,
              });
            }
            rows.push({
              cls: "host",
              text: count > 0 ? `${count} 条连接走此出口` : "已指定，暂无流量",
            });
          }
          if (p.count > 1) {
            rows.push({ cls: "host", text: `附近共 ${p.count} 个节点` });
          }
          return (
            <g
              key={p.name}
              className={
                isActive ? "nm-node active" : isProc ? "nm-node proc" : "nm-node"
              }
              onClick={(e) => {
                if (justDraggedRef.current) {
                  justDraggedRef.current = false;
                  return;
                }
                // 单节点直接切换；合并点展开菜单让用户挑
                if (p.count === 1) {
                  onPick?.(p.name);
                } else {
                  setPicker({ x: e.clientX, y: e.clientY, items: p.names });
                }
              }}
              onMouseEnter={(e) => setTip({ x: e.clientX, y: e.clientY, rows })}
              onMouseMove={(e) =>
                setTip((cur) => (cur ? { ...cur, x: e.clientX, y: e.clientY } : cur))
              }
              onMouseLeave={() => setTip(null)}
              style={{ cursor: onPick ? "pointer" : "default" }}
            >
              <circle
                cx={p.x}
                cy={p.y}
                r={(isActive ? 12 : isProc ? 9 : 6) * k}
                fill={isProc ? "url(#nm-glow-proc)" : "url(#nm-glow)"}
              />
              <circle
                cx={p.x}
                cy={p.y}
                r={(isActive ? 3.6 : isProc ? 3 : 2.4) * k}
                className="nm-dot"
                style={
                  isActive
                    ? undefined
                    : isProc
                      ? { fill: "var(--amber)" }
                      : { fill: delayColor(d) }
                }
              />
            </g>
          );
        })}

        {/* 本机 */}
        <g className="nm-home">
          <circle cx={homeXY.x} cy={homeXY.y} r={9 * k} fill="url(#nm-glow)" />
          <circle cx={homeXY.x} cy={homeXY.y} r={3 * k} className="nm-dot home" />
        </g>

        {/* 标签统一放最上层，位置已经避让过。字号随缩放反向补偿，
            这样放大后字不会跟着变巨大。 */}
        {labels.map((l) => (
          <text
            key={l.key}
            x={l.x}
            y={l.y}
            className={l.cls}
            style={{
              fontSize: (l.cls.includes("small") ? 9 : 11) * k,
              // 描边跟随缩放，放大后不会显得描边变粗/变细
              strokeWidth: (l.cls.includes("small") ? 2 : 2.5) * k,
            }}
          >
            {l.text}
          </text>
        ))}
      </svg>

      <div className="nm-legend">
        <span>
          <i className="nm-swatch home" /> 本机
        </span>
        <span>
          <i className="nm-swatch fast" /> 快
          <i className="nm-swatch mid" /> 中
          <i className="nm-swatch slow" /> 慢
        </span>
        <span>
          <i className="nm-swatch line" /> 主出口
          <i className="nm-swatch proc" /> 进程出口
        </span>
      </div>

      {tip && (
        <div
          className="nm-tip"
          style={{
            // 靠近右/下边缘时翻转方向，免得浮层被窗口裁掉一半；
            // 翻转后仍可能为负（贴左/上边缘），再钳一次下限。
            left: Math.max(
              0,
              tip.x + 14 + 300 > window.innerWidth ? tip.x - 314 : tip.x + 14,
            ),
            top: Math.max(
              0,
              tip.y + 14 + 110 > window.innerHeight ? tip.y - 124 : tip.y + 14,
            ),
          }}
        >
          {tip.rows.map((r, i) => (
            <div key={i} className={`nm-tip-row ${r.cls}`}>
              {r.text}
            </div>
          ))}
        </div>
      )}

      {/* 测速按钮。测完地图上的点会按延迟变色。 */}
      <div className="nm-tools">
        <button
          className="nm-refresh"
          onClick={() => onRefresh?.()}
          disabled={refreshing}
          title="测所有节点的延迟，测完地图上的点会按延迟变色"
        >
          {refreshing ? "测速中…" : "⟳ 测速"}
        </button>
      </div>

      {/* 缩放控件。放在右下角，不挡地图主体。 */}
      <div className="nm-zoom">
        <button className="nm-zoom-btn" onClick={() => zoomBy(MAP_ZOOM_STEP)} title="放大">
          +
        </button>
        <button className="nm-zoom-btn" onClick={() => zoomBy(1 / MAP_ZOOM_STEP)} title="缩小">
          −
        </button>
        <button className="nm-zoom-btn" onClick={resetView} title="回到全球视图">
          ⟳
        </button>
      </div>

      {/* 合并点的选点菜单 */}
      {picker && (
        <div
          className="nm-picker"
          style={{
            // 窄窗口下 min 会算出负数，菜单会被挤到屏幕外，必须再钳一次下限
            left: Math.max(0, Math.min(picker.x + 12, window.innerWidth - 230)),
            top: Math.max(0, Math.min(picker.y + 12, window.innerHeight - 220)),
          }}
        >
          <div className="nm-picker-head">
            <span>这个位置有 {picker.items.length} 个节点</span>
            <button className="nm-picker-close" onClick={() => setPicker(null)} title="关闭">
              ×
            </button>
          </div>
          <div className="nm-picker-list">
            {picker.items.map((n) => {
              const d = delays?.[n];
              const testing = testingNode === n;
              return (
                <div key={n} className="nm-picker-item">
                  <button
                    className="nm-picker-pick"
                    onClick={() => {
                      onPick?.(n);
                      setPicker(null);
                    }}
                    title={`切换到 ${n}`}
                  >
                    <span className="nm-picker-name">{n}</span>
                    <span className="nm-picker-delay" style={{ color: delayColor(d) }}>
                      {delayText(d)}
                    </span>
                  </button>
                  <button
                    className="nm-picker-test"
                    disabled={testing || !onTestNode}
                    onClick={(e) => {
                      e.stopPropagation();
                      onTestNode?.(n);
                    }}
                    title={testing ? "测速中…" : "只测这个节点"}
                    aria-label={`测速 ${n}`}
                  >
                    {testing ? "…" : "⟳"}
                  </button>
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}
