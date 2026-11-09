import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { svc } from "./api";
import {
  GEOIP_CACHE_MAX,
  TOAST_ERROR_MS,
  TOAST_INFO_MS,
  TOAST_KERNEL_ERR_MS,
  TOAST_NOTE_MS,
  POLL_LIST_HIDDEN_MS,
  POLL_LIST_MS,
  POLL_MAIN_HIDDEN_MS,
  POLL_MAIN_MS,
} from "./constants";
import NodeMap, { type MapExit } from "./NodeMap";
import ConnectionsPanel from "./components/ConnectionsPanel";
import NodesPanel from "./components/NodesPanel";
import OverviewPanel from "./components/OverviewPanel";
import SettingsPanel from "./components/SettingsPanel";
import LogPanel from "./components/LogPanel";
import Sidebar from "./components/Sidebar";
import Topbar from "./components/Topbar";
import type { Conn, NodeView, Status, SubInfo, SubItem, Tab, Traffic } from "./types";
import { procKeyLabel } from "./utils";

// UI 里最多保留的日志行数。后端缓冲 400 行，这里留点余量避免来回抖动。
const LOG_UI_MAX = 500;

// 走 Windows 通知中心（WebView2 的 Notification API）。
// 收起窗口时也能看到，比界面里的 toast 靠谱。
function notify(title: string, body?: string) {
  try {
    if (!("Notification" in window)) return;
    const show = () => {
      try {
        new Notification(title, { body, silent: true });
      } catch {
        /* WebView2 下偶尔抛错，忽略 */
      }
    };
    if (Notification.permission === "granted") {
      show();
    } else if (Notification.permission !== "denied") {
      Notification.requestPermission().then((p) => {
        if (p === "granted") show();
      });
    }
  } catch {
    /* 完全不支持就静默 */
  }
}


const EMPTY_STATUS: Status = {
  state: "stopped",
  pid: 0,
  uptime: 0,
  error: "",
  version: "",
  proxyOn: false,
  proxyAddr: "",
  mixedPort: 0,
  apiPort: 0,
  mode: "rule",
  bootErr: "",
  dataDir: "",
  tun: false,
  admin: false,
};

const EMPTY_SUB: SubInfo = {
  url: "",
  name: "",
  nodes: 0,
  groups: 0,
  rules: 0,
  fetchedAt: 0,
  insecure: false,
  upload: 0,
  download: 0,
  total: 0,
  expire: 0,
  webPage: "",
  updateHrs: 0,
  error: "",
};



// 顶栏只放"规则 / 全局"。直连不放这里——它会让所有流量绕过代理，
// 一旦误点就是"我明明开了代理却上不了网"，排查成本极高。
// 直连挪到设置页，并且明确标注用途。

// mihomo 的 chains 形如 "香港 01 → 节点选择 → Copilot"：节点在最前，
// 后面是逐层经过的策略组。进程指定节点后链尾会挂 nx-pp- 隐藏组。
// 取第一段才是真正出口。
function exitNodeOf(c: Conn): string {
  const parts = c.chains.split("→").map((s) => s.trim()).filter(Boolean);
  return parts.length > 0 ? parts[0] : "";
}

// 进程指定出口的隐藏组前缀，和后端 configgen.ProcessGroupPrefix 保持一致。
const PROC_TARGET_PREFIX = "nx-pp-";

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

export default function App() {
  const [status, setStatus] = useState<Status>(EMPTY_STATUS);
  const [traffic, setTraffic] = useState<Traffic>({
    upTotal: 0,
    downTotal: 0,
    upRate: 0,
    downRate: 0,
  });
  const [conns, setConns] = useState<Conn[]>([]);
  const [logs, setLogs] = useState<string[]>([]);
  // 已消费到的日志序号，用于增量拉取
  const logSeqRef = useRef(0);
  // 代数计数。清空日志时 +1，用来丢弃清空之前发出的在途请求结果——
  // 否则晚到的旧响应会把旧日志拼回来，还会把游标写回大序号。
  const logGenRef = useRef(0);
  // 后端提示"更早的日志已被滚出缓冲区"
  const [logTruncated, setLogTruncated] = useState(false);
  const [nodes, setNodes] = useState<NodeView[]>([]);
  const [sub, setSub] = useState<SubInfo>(EMPTY_SUB);
  const [procRules, setProcRules] = useState<Record<string, string>>({});
  const [delays, setDelays] = useState<Record<string, number>>({});
  const [favSet, setFavSet] = useState<Set<string>>(new Set());

  // 连接持续时间：从 start 时间戳算
  const [trafficAlertMB, setTrafficAlertMB] = useState(0);
  // 地图上"本机"的位置，默认东莞，可在设置里改
  const [homeLoc, setHomeLoc] = useState({ lon: 113.75, lat: 23.02 });
  const [testing, setTesting] = useState(false);
  const [testingNode, setTestingNode] = useState<string | null>(null);
  const [sortByDelay, setSortByDelay] = useState(false);
  const [autoStart, setAutoStart] = useState(false);
  const [nodeFilter, setNodeFilter] = useState("");
  // 节点右键菜单
  const [nodeMenu, setNodeMenu] = useState<{
    x: number;
    y: number;
    name: string;
  } | null>(null);
  // 点菜单外就关掉
  useEffect(() => {
    if (!nodeMenu) return;
    const onDown = (e: MouseEvent) => {
      const el = e.target as Element | null;
      if (el && el.closest(".nm-picker")) return;
      setNodeMenu(null);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [nodeMenu]);
  // 连接目标的地理位置（IP → {lon,lat,country}），给地图画第二段路径用。
  const [targetLocs, setTargetLocs] = useState<Record<string, any>>({});
  const queriedIps = useRef<Set<string>>(new Set());
  const [subInput, setSubInput] = useState("");
  const [subBusy, setSubBusy] = useState(false);
  const [editingSub, setEditingSub] = useState(false);
  const [subs, setSubs] = useState<{ items: SubItem[]; active: number }>({ items: [], active: 0 });
  const [tab, setTab] = useState<Tab>("overview");
  const [err, setErr] = useState("");
  const [note, setNote] = useState("");

  // 提示条自动消失的定时器统一管理：新的提示要取消上一个定时器，
  // 否则旧定时器会把新消息提前清掉（表现为"提示一闪就没"）。
  const noteTimer = useRef<number | null>(null);
  const errTimer = useRef<number | null>(null);
  const schedNoteClear = useCallback((ms: number) => {
    if (noteTimer.current) window.clearTimeout(noteTimer.current);
    noteTimer.current = window.setTimeout(() => setNote(""), ms);
  }, []);
  const schedErrClear = useCallback((ms: number) => {
    if (errTimer.current) window.clearTimeout(errTimer.current);
    errTimer.current = window.setTimeout(() => setErr(""), ms);
  }, []);
  // 卸载时清理，避免定时器在组件消失后还去 setState
  useEffect(
    () => () => {
      if (noteTimer.current) window.clearTimeout(noteTimer.current);
      if (errTimer.current) window.clearTimeout(errTimer.current);
    },
    [],
  );

  // 用户主动操作失败要看得见；后台静默读取失败只记 console，
  // 否则内核启动的瞬间会弹一串无关提示。
  const reportErr = useCallback(
    (e: unknown) => {
      setErr(e instanceof Error ? e.message : String(e));
      schedErrClear(TOAST_ERROR_MS);
    },
    [schedErrClear],
  );
  const logErr = useCallback((e: unknown) => {
    console.warn("[routepilot]", e);
  }, []);
  // 用计数表达并发操作。单个布尔值时，第一个结束的操作就会解锁界面，
  // 另一个还在跑却已经可以再点。
  const [busyCount, setBusyCount] = useState(0);
  const busy = busyCount > 0;
  const setBusy = useCallback((on: boolean) => {
    setBusyCount((n) => (on ? n + 1 : Math.max(0, n - 1)));
  }, []);


  // 窗口收到托盘 / 被最小化时降频轮询。
  // 后台常驻是这类软件的常态，1.2 秒一轮的全量 IPC 纯属白耗 CPU 和电。
  const [hidden, setHidden] = useState(false);
  useEffect(() => {
    const sync = () => setHidden(document.visibilityState === "hidden");
    sync();
    document.addEventListener("visibilitychange", sync);
    return () => document.removeEventListener("visibilitychange", sync);
  }, []);

  useEffect(() => {
    let alive = true;
    // 请求序号：慢响应晚到会覆盖新数据，只采纳最新一轮的结果。
    let seq = 0;
    const tick = async () => {
      const mine = ++seq;
      const gen = logGenRef.current;
      const [s, t, c, l] = await Promise.allSettled([
        svc.Status(),
        svc.Traffic(),
        svc.Connections(),
        // 增量拉日志：只取上次序号之后的新行，不再每轮重复传整个缓冲
        svc.LogsSince(logSeqRef.current),
      ]);
      if (!alive || mine !== seq) return;
      // 空值防御：接口返回 null 时保持上一帧，避免读 status.xxx 直接白屏
      if (s.status === "fulfilled" && s.value) applyPolledStatus(s.value);
      if (t.status === "fulfilled" && t.value) setTraffic(t.value);
      if (c.status === "fulfilled") setConns(Array.isArray(c.value) ? c.value : []);
      if (l.status === "fulfilled" && l.value && gen === logGenRef.current) {
        const { lines, seq: nextSeq, truncated } = l.value;
        setLogTruncated(Boolean(truncated));
        if (nextSeq < logSeqRef.current) {
          // 序号回退说明缓冲区被清空重置了，整段替换
          logSeqRef.current = nextSeq;
          setLogs(Array.isArray(lines) ? lines : []);
        } else if (lines && lines.length > 0) {
          logSeqRef.current = nextSeq;
          setLogs((prev) => {
            const next = prev.concat(lines);
            return next.length > LOG_UI_MAX ? next.slice(next.length - LOG_UI_MAX) : next;
          });
        }
      }
    };
    void tick();
    const id = window.setInterval(tick, hidden ? POLL_MAIN_HIDDEN_MS : POLL_MAIN_MS);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, [hidden]);

  // 节点列表与订阅信息变化不频繁，用更低的频率单独轮询。
  useEffect(() => {
    let alive = true;
    let seq = 0;
    const tick = async () => {
      const mine = ++seq;
      const [n, s, r, l] = await Promise.allSettled([
        svc.Nodes(),
        svc.Subscription(),
        svc.ProcessRules(),
        svc.ListSubscriptions(),
      ]);
      if (!alive || mine !== seq) return;
      if (n.status === "fulfilled") setNodes(Array.isArray(n.value) ? n.value : []);
      if (s.status === "fulfilled" && s.value) setSub(s.value);
      if (r.status === "fulfilled") applyPolledProcRules(r.value ?? {});
      if (l.status === "fulfilled" && l.value) setSubs(l.value);
    };
    void tick();
    const id = window.setInterval(tick, hidden ? POLL_LIST_HIDDEN_MS : POLL_LIST_MS);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, [hidden]);


  // 开机自启是个低频设置，读一次就够。
  useEffect(() => {
    svc.AutoStart().then(setAutoStart).catch(logErr);
    svc
      .HomeLocation()
      .then((h) => setHomeLoc({ lon: h.lon, lat: h.lat }))
      .catch(logErr);
  }, [logErr]);

  // 订阅后台自动更新后给个提示，否则用户不知道数据是新的。
  // On 会返回反注册函数，必须清理——否则 StrictMode/热更新下会重复注册，
  // 同一条通知弹两次。
  useEffect(() => {
    const offSub = Events.On("sub-auto-updated", (payload: any) => {
      const name = payload?.data;
      setNote(`订阅${name ? `「${name}」` : ""}已自动更新`);
      schedNoteClear(TOAST_INFO_MS);
      notify("订阅已自动更新", name ? `「${name}」拉取到新配置` : "配置已刷新");
    });

    const offCrash = Events.On("kernel-crashed", (payload: any) => {
      const msg = typeof payload?.data === "string" ? payload.data : "内核异常退出";
      setErr(msg);
      schedErrClear(TOAST_KERNEL_ERR_MS);
      notify("内核异常", msg);
    });

    const offNet = Events.On("network-degraded", (payload: any) => {
      const msg = typeof payload?.data === "string" ? payload.data : "代理链路检测失败";
      setErr(msg);
      schedErrClear(TOAST_KERNEL_ERR_MS);
      notify("网络告警", msg);
    });

    return () => {
      offSub();
      offCrash();
      offNet();
    };
  }, []);

  // 连接里出现新的目标 IP 就查一次 GeoIP。已查过的不再查，避免重复调用。
  useEffect(() => {
    // 长跑时这个集合只增不减，给个上限，超了就整体清空重查（很少触发）
    if (queriedIps.current.size > GEOIP_CACHE_MAX) queriedIps.current.clear();

    const fresh = conns
      .map((c) => c.destIP)
      .filter((ip): ip is string => !!ip && !queriedIps.current.has(ip));
    if (fresh.length === 0) return;
    for (const ip of fresh) queriedIps.current.add(ip);

    let alive = true;
    void svc
      .ResolveTargets(fresh)
      .then((r) => {
        if (!alive) {
          // 这批结果被丢弃了，必须撤销标记——否则这些 IP 永远不会再查，
          // 地图上的目标点会永久缺失。
          for (const ip of fresh) queriedIps.current.delete(ip);
          return;
        }
        if (r) setTargetLocs((prev) => ({ ...prev, ...r }));
      })
      .catch(() => {
        // 查询失败同样撤销标记，下一轮重试
        for (const ip of fresh) queriedIps.current.delete(ip);
      });
    return () => {
      alive = false;
    };
  }, [conns]);

  const run = useCallback(async (fn: () => Promise<void>) => {
    setBusy(true);
    setErr("");
    try {
      await fn();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }, []);

  const onToggleKernel = () =>
    run(async () => {
      if (status.state === "running") {
        await svc.StopKernel();
      } else {
        await svc.StartKernel();
      }
    });

  const onToggleProxy = () => run(() => svc.SetSystemProxy(!status.proxyOn));

  // 模式切换要立刻有反馈：先本地改高亮，失败再回滚。
  // 如果等 1.2 秒的轮询才更新，用户会以为点了没反应。
  const onSetMode = async (m: string) => {
    if (m === status.mode) return;
    const prev = status.mode;
    modeGuardRef.current = { value: m, until: Date.now() + 4000 };
    setStatus((p) => ({ ...p, mode: m }));
    setBusy(true);
    setErr("");
    try {
      await svc.SetMode(m);
    } catch (e) {
      modeGuardRef.current = null;
      setStatus((p) => ({ ...p, mode: prev }));
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const onToggleTun = () => run(() => svc.SetTUN(!status.tun));

  // 提权重启后当前进程会退出，这次调用大概率直接断开，所以不当错误处理。
  const onElevate = () => {
    svc.RelaunchElevated().catch(reportErr);
  };

  const onSubscribe = async () => {
    const url = subInput.trim();
    if (!url) {
      setErr("请先填写订阅地址");
      return;
    }
    setSubBusy(true);
    setErr("");
    try {
      await svc.Subscribe(url);
      setSubInput("");
      setEditingSub(false);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSubBusy(false);
    }
  };

  const onRefreshSub = async () => {
    setSubBusy(true);
    setErr("");
    try {
      await svc.RefreshSubscription();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSubBusy(false);
    }
  };

  // 订阅操作用 URL 定位，不用下标——列表每 3 秒刷新一次，
  // 点击瞬间若顺序变了，下标会指到另一个订阅上。
  const onSwitchSub = async (url: string) => {
    setBusy(true);
    setErr("");
    try {
      await svc.SwitchSubscriptionByURL(url);
      setEditingSub(false);
      setSubInput("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const onRemoveSub = async (url: string, name: string) => {
    // 删除后订阅 URL 要重新填，误点代价不小。
    if (!window.confirm(`确定删除订阅「${name}」？\n删除后需要重新添加订阅地址。`)) {
      return;
    }
    setBusy(true);
    setErr("");
    try {
      await svc.RemoveSubscriptionByURL(url);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  // 机场的主力策略组。只看 Selector——URLTest/Fallback 是自动选路，
  // 往里塞节点不会生效。成员最多的那个通常就是"节点选择"。
  const mainGroup = useMemo(() => {
    const candidates = nodes.filter(
      (n) => n.type === "Selector" && !BUILTIN.has(n.name) && n.count > 1
    );
    if (candidates.length === 0) {
      return nodes.find((n) => n.group && !BUILTIN.has(n.name)) ?? null;
    }
    return [...candidates].sort((a, b) => b.count - a.count)[0];
  }, [nodes]);

  // 机场习惯用"节点"来展示套餐信息（剩余流量、到期时间），
  // 它们其实指向同一个服务器，留在列表里只会碍眼。
  const leafNodes = useMemo(() => {
    const noise = ["剩余流量", "套餐到期", "过期时间", "过滤掉", "官网", "客服"];
    return nodes.filter(
      (n) => !n.group && !BUILTIN.has(n.name) && !noise.some((p) => n.name.startsWith(p))
    );
  }, [nodes]);

  // 全局模式下流量走的是 GLOBAL 组，点节点应该改 GLOBAL 的出口，
  // 而不是去改机场的策略组（那会污染规则模式下的选择）。
  const globalGroup = useMemo(() => nodes.find((n) => n.name === "GLOBAL") ?? null, [nodes]);

  // 地图只画叶子节点（跳过"剩余流量"这类噪音项）。
  const nodeNames = useMemo(() => leafNodes.map((n) => n.name), [leafNodes]);

  // 节点详情（协议、延迟），地图悬停时显示。
  const nodeMeta = useMemo(() => {
    const m: Record<string, { type: string; delay: number }> = {};
    for (const n of leafNodes) {
      m[n.name] = { type: n.type, delay: delays[n.name] ?? n.delay };
    }
    return m;
  }, [leafNodes, delays]);

  // 刚测出来的延迟优先于内核里存的历史值。
  const sortedNodes = useMemo(() => {
    const list = [...leafNodes];
    // 收藏的置顶，然后按延迟排
    return list.sort((a, b) => {
      const fa = favSet.has(a.name) ? 0 : 1;
      const fb = favSet.has(b.name) ? 0 : 1;
      if (fa !== fb) return fa - fb;
      if (!sortByDelay) return 0;
      const da = delays[a.name] !== undefined ? delays[a.name] : a.delay;
      const db = delays[b.name] !== undefined ? delays[b.name] : b.delay;
      return (da > 0 ? da : 1e9) - (db > 0 ? db : 1e9);
    });
  }, [leafNodes, sortByDelay, delays, favSet]);

  // 节点多的时候找起来费劲，和进程区一样加个过滤。
  const filteredNodes = useMemo(() => {
    const q = nodeFilter.trim().toLowerCase();
    if (!q) return sortedNodes;
    return sortedNodes.filter((n) => n.name.toLowerCase().includes(q));
  }, [sortedNodes, nodeFilter]);

  const activeGroupName =
    status.mode === "global" ? "GLOBAL" : (mainGroup?.name ?? "");

  const activeNode =
    status.mode === "global" ? (globalGroup?.now ?? "") : (mainGroup?.now ?? "");

  // 主策略组的 now 可能指向另一个组（机场常见的分层结构），
  // 地图需要的是最终落地的节点。解析不到就退回空，避免把组名当节点画。
  const mainExitNode = useMemo(() => {
    if (!activeNode) return "";
    const leafSet = new Set(leafNodes.map((n) => n.name));
    if (leafSet.has(activeNode)) return activeNode;
    const groupNow = new Map<string, string>();
    for (const n of nodes) {
      if (n.group && n.now) groupNow.set(n.name, n.now);
    }
    let cur = activeNode;
    for (let i = 0; i < 4; i++) {
      const next = groupNow.get(cur);
      if (!next || next === cur) return "";
      if (leafSet.has(next)) return next;
      cur = next;
    }
    return "";
  }, [activeNode, leafNodes, nodes]);

  // 地图按"真实出口"分组绘制。主策略组只代表未命中进程规则的流量，
  // 进程指定节点后，实际出口可能同时有好几个。
  const mapExits = useMemo<MapExit[]>(() => {
    const leafSet = new Set(leafNodes.map((n) => n.name));
    const groupNow = new Map<string, string>();
    for (const n of nodes) {
      if (n.group && n.now) groupNow.set(n.name, n.now);
    }

    const byName = new Map<string, MapExit>();
    const ensure = (name: string): MapExit => {
      let e = byName.get(name);
      if (!e) {
        e = { name, main: name === mainExitNode, procs: [], conns: 0, targets: [] };
        byName.set(name, e);
      }
      return e;
    };

    // 主出口即使暂时没有连接也要画出来。
    if (mainExitNode) ensure(mainExitNode);

    // 策略组可能嵌套（组里再选组），一路跟到真正的叶子节点。
    const resolveGroupNode = (name: string): string => {
      let cur = name;
      for (let i = 0; i < 4; i++) {
        if (leafSet.has(cur)) return cur;
        const next = groupNow.get(cur);
        if (!next || next === cur) return "";
        cur = next;
      }
      return "";
    };

    // 已配置的进程出口先标出来：没有流量时也应该在地图上看到它。
    for (const [proc, policy] of Object.entries(procRules)) {
      if (!policy.startsWith("target:")) continue;
      const target = policy.slice("target:".length);
      const node = leafSet.has(target) ? target : resolveGroupNode(target);
      if (!node || !leafSet.has(node)) continue;
      const e = ensure(node);
      const label = procKeyLabel(proc);
      if (!e.procs.includes(label)) e.procs.push(label);
    }

    // 活动连接的真实出口。每个出口最多保留 3 个目标点，
    // 多出口时否则连线会把地图铺满。
    for (const c of conns) {
      const name = exitNodeOf(c);
      if (!name || !leafSet.has(name)) continue;
      const e = ensure(name);
      e.conns += 1;
      // 链上出现 nx-pp- 说明这条连接命中了"进程指定出口"。
      // 即使进程规则还没同步过来，也能靠连接本身识别出来。
      if (
        c.chains.includes(PROC_TARGET_PREFIX) &&
        c.process &&
        c.process !== "—" &&
        !e.procs.includes(c.process)
      ) {
        e.procs.push(c.process);
      }
      const loc = c.destIP ? targetLocs[c.destIP] : undefined;
      if (!loc) continue;
      const key = `${loc.lon},${loc.lat}`;
      if (e.targets.some((t) => `${t.lon},${t.lat}` === key)) continue;
      if (e.targets.length >= 3) continue;
      e.targets.push({
        lon: loc.lon,
        lat: loc.lat,
        country: loc.country,
        ip: c.destIP,
        host: c.host || c.dest || "",
      });
    }

    // 只画主出口和进程指定出口。机场业务组（Netflix/ChatGPT 等）
    // 自己选到别的节点时不算进程分流，避免把它们误标成"进程出口"。
    const list = [...byName.values()].filter((e) => e.main || e.procs.length > 0);
    list.sort(
      (a, b) =>
        Number(b.main) - Number(a.main) ||
        b.conns - a.conns ||
        a.name.localeCompare(b.name),
    );
    return list.slice(0, 4);
  }, [mainExitNode, conns, leafNodes, nodes, procRules, targetLocs]);


  // 订阅流量。机场把用量放在 subscription-userinfo 响应头里，
  // 已用 = 上传 + 下载，和面板显示的口径一致。
  // 订阅列表：多订阅数据没就绪时退化成只显示当前那个。
  // 退化分支也要拼成完整 SubItem，字段缺一个（比如 insecure）就会让
  // "恢复证书校验"按钮消失。
  const subItems: SubItem[] =
    subs.items.length > 0
      ? subs.items
      : sub.url
        ? [
            {
              url: sub.url,
              name: sub.name,
              fetchedAt: sub.fetchedAt,
              upload: sub.upload,
              download: sub.download,
              total: sub.total,
              expire: sub.expire,
              insecure: sub.insecure,
              active: true,
            },
          ]
        : [];

  // 从当前连接里提取进程，并把"已设规则但暂时没连接"的进程也带出来，
  // 否则用户改完规则后进程一空闲就从列表里消失了。
  // 连接按流量排序。默认沿用后端顺序，点表头才排。
  const onSetProc = (proc: string, policy: string) => {
    const oldPolicy = procRules[proc] ?? "follow";
    procGuardRef.current.set(proc, { value: policy, until: Date.now() + 4000 });
    // 先本地更新，避免等 3 秒轮询才看到变化
    setProcRules((prev) => {
      const next = { ...prev };
      if (policy === "follow") delete next[proc];
      else next[proc] = policy;
      return next;
    });
    setErr("");
    void (async () => {
      try {
        const closed = await svc.SetProcessRule(proc, policy);
        if (closed > 0) {
          setNote(`${procKeyLabel(proc)} 的 ${closed} 条旧连接已断开，重连即按新规则走`);
          schedNoteClear(TOAST_NOTE_MS);
        }
      } catch (e) {
        // 失败回滚，不能让界面显示"已改"而实际没生效
        setProcRules((prev) => {
          const next = { ...prev };
          if (oldPolicy === "follow") delete next[proc];
          else next[proc] = oldPolicy;
          return next;
        });
        setErr(e instanceof Error ? e.message : String(e));
      }
    })();
  };

  const onCloseConn = (id: string) => run(() => svc.CloseConnection(id));
  const onCloseAll = () => {
    if (conns.length === 0) return;
    if (!window.confirm(`确定断开全部 ${conns.length} 条连接？\n程序会自动重新发起连接。`)) {
      return;
    }
    return run(() => svc.CloseAllConnections());
  };

  const onToggleAutoStart = async () => {
    const next = !autoStart;
    try {
      await svc.SetAutoStart(next);
      setAutoStart(next);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  // --- 内核版本更新 ---
  const [coreInfo, setCoreInfo] = useState<{
    current: string;
    latest: string;
    hasUpdate: boolean;
    url: string;
    assetName: string;
  } | null>(null);
  const [coreChecking, setCoreChecking] = useState(false);
  const [coreUpdating, setCoreUpdating] = useState(false);
  const [coreCustom, setCoreCustom] = useState(false);

  useEffect(() => {
    svc.TrafficAlertLimit().then((v) => setTrafficAlertMB(Math.round(v / 1024 / 1024))).catch(logErr);
    svc.FavoriteNodes().then((list) => setFavSet(new Set(list))).catch(logErr);
  }, []);

  // 流量历史（分钟级），用于概览页的 24 小时图表。
  const [trafficHistory, setTrafficHistory] = useState<
    Array<{ t: number; u: number; d: number }>
  >([]);

  useEffect(() => {
    const load = () =>
      svc
        .TrafficHistory()
        .then(setTrafficHistory)
        .catch(logErr);
    load();
    // 每分钟采样一次，图表也跟着每分钟刷新就够
    const timer = window.setInterval(load, 60_000);
    return () => window.clearInterval(timer);
  }, []);



  // 延迟趋势：和上一次测速比，给出 ↑ ↓ →
  const histTrend = (name: string) => {
    const list = delayHist[name] || [];
    if (list.length < 2) return "";
    const last = list[list.length - 1].d;
    const prev = list[list.length - 2].d;
    if (last <= 0 || prev <= 0) return "";
    const diff = last - prev;
    if (Math.abs(diff) < 15) return " →";
    return diff > 0 ? " ↑" : " ↓";
  };

  // 悬停显示最近几次测速
  const histTip = (name: string) => {
    const list = (delayHist[name] || []).slice(-5);
    if (list.length === 0) return "暂无测速历史";
    return list
      .map(
        (s) =>
          `${new Date(s.t * 1000).toLocaleTimeString()}  ${
            s.d > 0 ? s.d + " ms" : "不通"
          }`
      )
      .join("\n");
  };

  // --- 节点延迟历史 ---
  const [delayHist, setDelayHist] = useState<
    Record<string, Array<{ t: number; d: number }>>
  >({});
  useEffect(() => {
    const load = () =>
      svc
        .NodeDelayHistory()
        .then(setDelayHist)
        .catch(logErr);
    load();
    const timer = window.setInterval(load, 60_000);
    return () => window.clearInterval(timer);
  }, []);

  // --- 全局快捷键 ---
  // Ctrl+Shift+P 切换系统代理，Ctrl+Shift+T 全量测速
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!e.ctrlKey || !e.shiftKey) return;
      // 长按会连发 keydown，不去重的话会快速反复翻转系统代理
      if (e.repeat) return;
      // 焦点在输入框/可编辑区域时不拦截——否则在订阅框里打字
      // 会顺手把系统代理切了，或者触发全量测速。
      const t = e.target as HTMLElement | null;
      if (t) {
        const tag = t.tagName;
        if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || t.isContentEditable) {
          return;
        }
      }
      const k = e.key.toLowerCase();
      if (k === "p") {
        e.preventDefault();
        if (!busy) void run(() => svc.SetSystemProxy(!status.proxyOn));
      } else if (k === "t") {
        e.preventDefault();
        if (!testing) void onTestAll();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [busy, testing, status.proxyOn]);

  const onCheckCore = async () => {
    setCoreChecking(true);
    setErr("");
    try {
      const info = await svc.CheckCoreUpdate();
      setCoreInfo(info);
      setCoreCustom(await svc.CoreCustomized());
    } catch (e) {
      // 网络问题很常见（GitHub 在国内不稳），提示清楚而不是抛异常
      setErr(
        `检查更新失败：${e instanceof Error ? e.message : String(e)}（可尝试开启代理后重试）`
      );
    } finally {
      setCoreChecking(false);
    }
  };

  const onUpdateCore = async () => {
    if (!coreInfo?.url) return;
    if (
      !window.confirm(
        `确认升级内核到 ${coreInfo.latest}？\n升级过程中代理会短暂中断。`
      )
    ) {
      return;
    }
    setCoreUpdating(true);
    setErr("");
    try {
      await svc.UpdateCore(coreInfo.url);
      setNote(`内核已更新到 ${coreInfo.latest}`);
      schedNoteClear(TOAST_INFO_MS);
      notify("内核已更新", `RoutePilot 已升级到 ${coreInfo.latest}`);
      const info = await svc.CheckCoreUpdate();
      setCoreInfo(info);
      setCoreCustom(await svc.CoreCustomized());
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setCoreUpdating(false);
    }
  };

  const onResetCore = async () => {
    if (!window.confirm("回退到内置内核？\n自定义版本会被替换。")) return;
    setErr("");
    try {
      await svc.ResetCore();
      setCoreCustom(false);
      setNote("已回退到内置内核");
      schedNoteClear(TOAST_NOTE_MS);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  // --- 配置备份 ---
  const onExportConfig = async () => {
    try {
      const json = await svc.ExportConfig();
      const blob = new Blob([json], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `routepilot-config-${new Date().toISOString().slice(0, 10)}.json`;
      a.click();
      URL.revokeObjectURL(url);
      setNote("配置已导出");
      schedNoteClear(TOAST_NOTE_MS);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  const onImportConfig = () => {
    const input = document.createElement("input");
    input.type = "file";
    input.accept = ".json,application/json";
    input.onchange = async () => {
      const file = input.files?.[0];
      if (!file) return;
      try {
        const text = await file.text();
        await svc.ImportConfig(text);
        setNote("配置已导入，订阅与进程规则已覆盖");
        schedNoteClear(TOAST_NOTE_MS);
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e));
      }
    };
    input.click();
  };

  // 批量测速。结果按延迟升序排一次，用户点完直接看到最快的几个。
  const onTestAll = async () => {
    // 右键菜单不受按钮 disabled 约束，可能重复触发；
    // 两个并发测速会互相覆盖 delays，先完成那个还会提前把 testing 复位。
    if (testing) return;
    setTesting(true);
    setErr("");
    try {
      const r = await svc.TestAllNodes();
      setDelays(r ?? {});
      setSortByDelay(true);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setTesting(false);
    }
  };

  // 单节点测速。地图浮层和右键菜单共用，测试中显示转圈避免重复点击。
  const onTestOne = async (name: string) => {
    if (testingNode) return;
    setTestingNode(name);
    try {
      const d = await svc.TestNode(name);
      setDelays((prev) => ({ ...prev, [name]: d }));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setTestingNode(null);
    }
  };

  // 日志导出/清空。导出走 Go 侧取全量文本，再交给浏览器下载。
  const onExportLogs = useCallback(() => {
    void (async () => {
      try {
        const text = await svc.ExportLogs();
        const blob = new Blob([text], { type: "text/plain" });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = `routepilot-logs-${new Date().toISOString().slice(0, 10)}.txt`;
        a.click();
        URL.revokeObjectURL(url);
        setNote("日志已导出");
        schedNoteClear(TOAST_NOTE_MS);
      } catch (e) {
        reportErr(e);
      }
    })();
  }, [reportErr, schedNoteClear]);

  const onClearLogs = useCallback(() => {
    void run(async () => {
      // 先递增代数，让所有在途响应作废，再清空
      logGenRef.current += 1;
      await svc.ClearLogs();
      // 后端序号一并归零，否则下一轮增量会拿到空数组还以为没更新
      logSeqRef.current = 0;
      setLogTruncated(false);
      setLogs([]);
    });
  }, [run]);

  // 乐观写入守卫。
  // 本地先改、再等后端落库，中间那一轮在途轮询带回来的是改动前的数据，
  // 直接 setStatus 会把界面打回去，用户看到"切了又弹回来"。
  // 守卫在「后端确认」或「超时」时自动解除。
  const modeGuardRef = useRef<{ value: string; until: number } | null>(null);
  const procGuardRef = useRef<Map<string, { value: string; until: number }>>(new Map());

  const applyPolledStatus = (next: Status) => {
    const g = modeGuardRef.current;
    let mode = next.mode;
    if (g) {
      if (Date.now() > g.until || next.mode === g.value) modeGuardRef.current = null;
      else mode = g.value;
    }
    setStatus({ ...next, mode });
  };

  const applyPolledProcRules = (next: Record<string, string>) => {
    const g = procGuardRef.current;
    if (g.size === 0) {
      setProcRules(next);
      return;
    }
    const merged = { ...next };
    const now = Date.now();
    for (const [proc, v] of g) {
      if (now > v.until || next[proc] === v.value) {
        g.delete(proc);
        continue;
      }
      if (v.value === "follow") delete merged[proc];
      else merged[proc] = v.value;
    }
    setProcRules(merged);
  };

  const running = status.state === "running";
  // 节点面板的依赖
  const nodesProps = {
    status,
    running,
    busy,
    sub,
    subItems,
    subBusy,
    subInput,
    editingSub,
    leafNodes,
    filteredNodes,
    mainGroup,
    activeGroupName,
    activeNode,
    delays,
    favSet,
    nodeFilter,
    sortByDelay,
    testing,
    histTip,
    histTrend,
    setNodeFilter,
    setNodeMenu,
    setSortByDelay,
    setSubInput,
    setEditingSub,
    setFavSet,
    setNote,
    setErr,
    schedNoteClear,
    onRefreshSub,
    onSwitchSub,
    onRemoveSub,
    onSubscribe,
    onSetMode,
    onTestAll,
    run,
    reportErr,
  };

  // 概览面板的依赖
  const overviewProps = { status, traffic, conns, activeNode, trafficHistory };

  // 设置面板的全部依赖集中在这里，避免 JSX 里散落一长串 props
  const settingsProps = {
    status,
    running,
    busy,
    sub,
    autoStart,
    onToggleAutoStart,
    coreInfo,
    coreChecking,
    coreUpdating,
    coreCustom,
    onCheckCore,
    onUpdateCore,
    onResetCore,
    onToggleProxy,
    onToggleTun,
    onElevate,
    onSetMode,
    onExportConfig,
    onImportConfig,
    trafficAlertMB,
    setTrafficAlertMB,
    homeLoc,
    setHomeLoc,
    run,
    setNote,
    setErr,
    schedNoteClear,
    schedErrClear,
  };

  const banner = err || status.bootErr || (running ? "" : status.error);

  return (
    <div className="app">
            <Sidebar
        tab={tab}
        onTabChange={setTab}
        connCount={conns.length}
        nodeCount={leafNodes.length}
        tun={status.tun}
        proxyOn={status.proxyOn}
        state={status.state}
        running={running}
        busy={busy}
        onToggleProxy={onToggleProxy}
      />

      <main className="main">
        <Topbar
          tab={tab}
          mode={status.mode}
          running={running}
          busy={busy}
          onToggleKernel={onToggleKernel}
        />

        <div className="page">
          {banner && (
            <div className="banner error">
              <span>{banner}</span>
            </div>
          )}

          {note && <div className="banner info">{note}</div>}

          {/* 地图始终挂载，只用 display 控制显隐。
              切 tab 时如果卸载了组件，缩放级别和平移位置会全部归零。 */}
          <div style={{ display: tab === "overview" ? "block" : "none" }}>
            <NodeMap
              names={nodeNames}
              active={mainExitNode}
              exits={mapExits}
              delays={delays}
              nodeMeta={nodeMeta}
              onRefresh={onTestAll}
              refreshing={testing}
              onTestNode={onTestOne}
              testingNode={testingNode}
              home={homeLoc}
              onPick={(name) =>
                activeGroupName && run(() => svc.SelectNode(activeGroupName, name))
              }
            />
          </div>

          {tab === "overview" && <OverviewPanel {...overviewProps} />}

          {tab === "nodes" && <NodesPanel {...nodesProps} />}

          {tab === "connections" && (
            <ConnectionsPanel
              conns={conns}
              nodes={nodes}
              procRules={procRules}
              running={running}
              busy={busy}
              onSetProc={onSetProc}
              onCloseConn={onCloseConn}
              onCloseAll={onCloseAll}
            />
          )}

          {tab === "logs" && (
            <LogPanel
              logs={logs}
              truncated={logTruncated}
              onExport={onExportLogs}
              onClear={onClearLogs}
            />
          )}

          {tab === "settings" && (
            <SettingsPanel {...settingsProps} />
          )}
        </div>
      </main>

      {/* 节点右键菜单 */}
      {nodeMenu && (
        <div
          className="nm-picker"
          style={{
            left: Math.max(0, Math.min(nodeMenu.x, window.innerWidth - 210)),
            top: Math.max(0, Math.min(nodeMenu.y, window.innerHeight - 190)),
          }}
        >
          <div className="nm-picker-head">
            <span title={nodeMenu.name}>{nodeMenu.name.slice(0, 18)}</span>
            <button className="nm-picker-close" onClick={() => setNodeMenu(null)}>
              ×
            </button>
          </div>
          <div className="nm-picker-list">
            <button
              className="nm-picker-item"
              onClick={() => {
                if (activeGroupName)
                  void run(() => svc.SelectNode(activeGroupName, nodeMenu.name));
                setNodeMenu(null);
              }}
            >
              设为当前出口
            </button>
            <button
              className="nm-picker-item"
              onClick={() => {
                void onTestAll();
                setNodeMenu(null);
              }}
            >
              全量测速
            </button>
            <button
              className="nm-picker-item"
              onClick={() => {
                const name = nodeMenu.name;
                setNodeMenu(null);
                void (async () => {
                  try {
                    const on = await svc.ToggleFavorite(name);
                    setFavSet((prev) => {
                      const next = new Set(prev);
                      if (on) next.add(name);
                      else next.delete(name);
                      return next;
                    });
                  } catch (e) {
                    reportErr(e);
                  }
                })();
              }}
            >
              {favSet.has(nodeMenu.name) ? "★ 取消收藏" : "☆ 收藏此节点"}
            </button>
            <button
              className="nm-picker-item"
              onClick={async () => {
                const name = nodeMenu.name;
                setNodeMenu(null);
                try {
                  const d = await svc.TestNode(name);
                  setDelays((prev) => ({ ...prev, [name]: d }));
                  setNote(d > 0 ? `${name} 延迟 ${d} ms` : `${name} 测速失败`);
                  schedNoteClear(TOAST_NOTE_MS);
                } catch (e) {
                  setErr(e instanceof Error ? e.message : String(e));
                }
              }}
            >
              测速此节点
            </button>
            <button
              className="nm-picker-item"
              onClick={() => {
                void navigator.clipboard?.writeText(nodeMenu.name);
                setNote("节点名已复制");
                schedNoteClear(TOAST_NOTE_MS);
                setNodeMenu(null);
              }}
            >
              复制节点名
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
