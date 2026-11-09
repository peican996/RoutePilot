// 设置面板：网络接管 / 数据与备份 / 系统信息三组。
// 面板私有的 UI 状态（输入框、规则展开态）留在这里；
// 共享数据（status/sub/coreInfo）和动作通过 props 传入。

import { useEffect, useState } from "react";

import { svc } from "../api";
import { TOAST_ERROR_MS, TOAST_NOTE_MS } from "../constants";
import type { NodeView, SiteRule, Status, SubInfo } from "../types";
import { fmtVersion } from "../utils";

type CoreInfo = { hasUpdate: boolean; latest: string } | null;

// 内置组不作为网站规则的可选出口。
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

type Props = {
  status: Status;
  running: boolean;
  busy: boolean;
  sub: SubInfo;
  nodes: NodeView[];
  autoStart: boolean;
  onToggleAutoStart: () => void;
  coreInfo: CoreInfo;
  coreChecking: boolean;
  coreUpdating: boolean;
  coreCustom: boolean;
  onCheckCore: () => void;
  onUpdateCore: () => void;
  onResetCore: () => void;
  onToggleProxy: () => void;
  onToggleTun: () => void;
  onElevate: () => void;
  onSetMode: (m: string) => void;
  onExportConfig: () => void;
  onImportConfig: () => void;
  trafficAlertMB: number;
  setTrafficAlertMB: (n: number) => void;
  homeLoc: { lon: number; lat: number };
  setHomeLoc: (h: { lon: number; lat: number }) => void;
  run: (fn: () => Promise<void>) => Promise<unknown>;
  setNote: (s: string) => void;
  setErr: (s: string) => void;
  schedNoteClear: (ms: number) => void;
  schedErrClear: (ms: number) => void;
};

type RuleRow = { type: string; payload: string; proxy: string };

export default function SettingsPanel({
  status,
  running,
  busy,
  sub,
  nodes,
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
}: Props) {
  // 流量告警阈值输入框受控，避免命令式读 DOM 拿到 NaN
  const [alertInput, setAlertInput] = useState("");
  useEffect(() => {
    setAlertInput(trafficAlertMB > 0 ? String(trafficAlertMB) : "");
  }, [trafficAlertMB]);

  const [homeEdit, setHomeEdit] = useState({ lon: "", lat: "" });
  useEffect(() => {
    setHomeEdit({ lon: String(homeLoc.lon), lat: String(homeLoc.lat) });
  }, [homeLoc]);

  // 分流规则是低频查看的功能，数据只在这个面板用，状态留在这里
  const [rules, setRules] = useState<RuleRow[]>([]);
  const [ruleFilter, setRuleFilter] = useState("");
  const [rulesOpen, setRulesOpen] = useState(false);
  const [rulesLoading, setRulesLoading] = useState(false);

  // 网站分流：智能开关 + 自定义域名规则
  const [smart, setSmart] = useState(false);
  const [siteRules, setSiteRules] = useState<SiteRule[]>([]);
  const [siteDomain, setSiteDomain] = useState("");
  const [siteType, setSiteType] = useState("suffix");
  const [siteTarget, setSiteTarget] = useState("direct");
  const [siteBusy, setSiteBusy] = useState(false);

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const [sm, list] = await Promise.all([svc.SmartRouting(), svc.SiteRules()]);
        if (!alive) return;
        setSmart(!!sm);
        setSiteRules(Array.isArray(list) ? list : []);
      } catch {
        // 内核/后端尚未就绪时保持默认值，不打断设置页
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  const groupChoices = nodes.filter((n) => n.group && !BUILTIN.has(n.name));
  const nodeChoices = nodes.filter((n) => !n.group && !BUILTIN.has(n.name));

  const sitePolicyLabel = (p: string) => {
    if (p === "direct") return "直连";
    if (p === "proxy") return "主策略组";
    if (p.startsWith("target:")) return p.slice("target:".length);
    return p;
  };

  const onToggleSmart = async () => {
    setSiteBusy(true);
    setErr("");
    try {
      const next = !smart;
      await svc.SetSmartRouting(next);
      setSmart(next);
      setNote(next ? "已开启国内外智能分流" : "已关闭国内外智能分流");
      schedNoteClear(TOAST_NOTE_MS);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      schedErrClear(TOAST_ERROR_MS);
    } finally {
      setSiteBusy(false);
    }
  };

  const onAddSiteRule = async () => {
    const domain = siteDomain.trim();
    if (!domain) return;
    setSiteBusy(true);
    setErr("");
    try {
      await svc.SetSiteRuleWithType(domain, siteType, siteTarget);
      setSiteRules(await svc.SiteRules());
      setSiteDomain("");
      setNote(
        `${domain} 已按「${siteType === "exact" ? "精确域名" : "域名后缀"} / ${sitePolicyLabel(siteTarget)}」分流`,
      );
      schedNoteClear(TOAST_NOTE_MS);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      schedErrClear(TOAST_ERROR_MS);
    } finally {
      setSiteBusy(false);
    }
  };

  const onDeleteSiteRule = async (rule: SiteRule) => {
    setSiteBusy(true);
    setErr("");
    try {
      await svc.DeleteSiteRuleWithType(rule.domain, rule.type);
      setSiteRules(await svc.SiteRules());
      setNote(`已删除 ${rule.domain} 的网站规则`);
      schedNoteClear(TOAST_NOTE_MS);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      schedErrClear(TOAST_ERROR_MS);
    } finally {
      setSiteBusy(false);
    }
  };

  const onShowRules = async () => {
    setRulesOpen(true);
    setRulesLoading(true);
    try {
      setRules(await svc.ListRules());
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setRulesLoading(false);
    }
  };

  return (
    <>
  <div className="settings-section">网络接管</div>
  <div className="card" style={{ marginBottom: 14 }}>
    <div className="card-head">
      <span className="card-title">流量接管</span>
    </div>

    <div className="kv">
      <span className="kv-k">
        系统代理
        <span className="hint">
          {status.tun
            ? "已被 TUN 取代，先关闭 TUN 才能使用"
            : "只对读系统代理设置的程序生效，免管理员"}
        </span>
      </span>
      <button
        className={`switch ${status.proxyOn ? "on" : ""}`}
        onClick={onToggleProxy}
        disabled={!running || busy || status.tun}
        aria-label="系统代理"
      />
    </div>

    <div className="kv">
      <span className="kv-k">
        TUN 模式
        <span className="hint">IP 层接管全部流量，需管理员权限</span>
      </span>
      <button
        className={`switch ${status.tun ? "on" : ""}`}
        onClick={onToggleTun}
        disabled={busy || !running || !status.admin}
        aria-label="TUN 模式"
      />
    </div>

    <div
      style={{
        marginTop: 12,
        fontSize: 11.5,
        color: "var(--text-faint)",
        lineHeight: 1.7,
      }}
    >
      两者互斥，开一个会自动关掉另一个——否则浏览器先连本地代理端口、
      再被 TUN 转发一次，流量会被代理两遍。
    </div>

    {!status.admin && (
      <div
        className="banner info"
        style={{ marginTop: 12, marginBottom: 0, alignItems: "center" }}
      >
        <span style={{ flex: 1 }}>
          TUN 要创建虚拟网卡，需要管理员权限。重启后本窗口会关闭并重新打开。
        </span>
        <button className="btn sm primary" onClick={onElevate}>
          以管理员重启
        </button>
      </div>
    )}
  </div>

  <div className="card" style={{ marginBottom: 14 }}>
    <div className="card-head">
      <span className="card-title">网站分流</span>
    </div>
    <div className="kv">
      <span className="kv-k">
        国内外智能分流
        <span className="hint">国内域名/IP 直连，其余继续走订阅规则</span>
      </span>
      <button
        className={`switch ${smart ? "on" : ""}`}
        onClick={onToggleSmart}
        disabled={siteBusy}
        aria-label="国内外智能分流"
      />
    </div>

    <div className="card-sub" style={{ marginTop: 10 }}>
      自定义网站规则优先于智能分流；按域名后缀匹配，例如 example.com 也会匹配 www.example.com。
      修改对新建连接生效，已建立的连接会在对应规则变更时断开。
    </div>

    <div style={{ display: "flex", gap: 8, marginTop: 10, flexWrap: "wrap" }}>
      <input
        className="input"
        placeholder="example.com"
        value={siteDomain}
        onChange={(e) => setSiteDomain(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") void onAddSiteRule();
        }}
        spellCheck={false}
        style={{ flex: 1, minWidth: 160 }}
      />
      <select
        className="input"
        value={siteType}
        onChange={(e) => setSiteType(e.target.value)}
        style={{ width: 108 }}
        title="精确域名只匹配该主机；后缀会同时匹配子域名"
      >
        <option value="suffix">域名后缀</option>
        <option value="exact">精确域名</option>
      </select>
      <select
        className="input"
        value={siteTarget}
        onChange={(e) => setSiteTarget(e.target.value)}
        style={{ width: 150 }}
      >
        <option value="direct">直连</option>
        <option value="proxy">主策略组</option>
        {groupChoices.length > 0 && (
          <optgroup label="策略组">
            {groupChoices.map((g) => (
              <option key={`sg-${g.name}`} value={`target:${g.name}`}>
                {g.name}
              </option>
            ))}
          </optgroup>
        )}
        {nodeChoices.length > 0 && (
          <optgroup label="节点">
            {nodeChoices.map((n) => (
              <option key={`sn-${n.name}`} value={`target:${n.name}`}>
                {n.name}
              </option>
            ))}
          </optgroup>
        )}
      </select>
      <button
        className="btn sm primary"
        onClick={() => void onAddSiteRule()}
        disabled={siteBusy || !siteDomain.trim()}
      >
        添加
      </button>
    </div>

    {siteRules.length > 0 && (
      <div style={{ marginTop: 10 }}>
        {siteRules.map((r) => (
          <div
            key={r.domain}
            style={{
              display: "flex",
              alignItems: "center",
              gap: 10,
              padding: "7px 0",
              borderTop: "1px solid var(--border)",
              fontSize: 12.5,
            }}
          >
            <span className="mono" style={{ flex: 1, color: "var(--text-dim)" }}>
              {r.domain}
            </span>
            <span style={{ color: "var(--text-faint)" }}>{sitePolicyLabel(r.policy)}</span>
            <span
              style={{
                fontSize: 10.5,
                color: "var(--text-faint)",
                border: "1px solid var(--border)",
                borderRadius: 4,
                padding: "1px 5px",
              }}
            >
              {r.type === "exact" ? "精确" : "后缀"}
            </span>
            <button
              className="btn sm"
              onClick={() => void onDeleteSiteRule(r)}
              disabled={siteBusy}
            >
              删除
            </button>
          </div>
        ))}
      </div>
    )}
  </div>

  <div className="card">
    <div className="card-head">
      <span className="card-title">流量告警</span>
    </div>
    <div className="card-sub">
      当日流量超过阈值时弹出系统通知。0 = 不告警。
    </div>
    <div style={{ display: "flex", gap: 8, marginTop: 12, alignItems: "center" }}>
      <input
        className="input"
        type="number"
        min={0}
        placeholder="阈值 (MB)"
        style={{ width: 140 }}
        value={alertInput}
        onChange={(e) => setAlertInput(e.target.value)}
      />
      <span style={{ fontSize: 12, color: "var(--text-faint)" }}>MB</span>
      <button
        className="btn sm"
        onClick={async () => {
          // 非法输入（空/负数/非数字）一律按 0 处理，避免 NaN 传到后端
          const n = parseInt(alertInput, 10);
          const mb = Number.isFinite(n) && n > 0 ? n : 0;
          try {
            await svc.SetTrafficAlertLimit(mb);
            setTrafficAlertMB(mb);
            setNote(mb > 0 ? `流量告警已设为 ${mb} MB` : "流量告警已关闭");
          } catch (e) {
            setErr(e instanceof Error ? e.message : String(e));
          }
          schedNoteClear(TOAST_NOTE_MS);
        }}
      >
        保存
      </button>
    </div>
  </div>

  <div className="settings-section">数据与备份</div>
  <div className="card">
    <div className="card-head">
      <span className="card-title">配置备份与规则</span>
    </div>
    <div className="card-sub">
      导出订阅、进程规则等配置；查看当前生效的分流规则。
    </div>
    <div style={{ display: "flex", gap: 8, marginTop: 12, flexWrap: "wrap" }}>
      <button className="btn" onClick={onExportConfig}>
        导出配置
      </button>
      <button className="btn" onClick={onImportConfig}>
        导入配置
      </button>
      <button className="btn" onClick={onShowRules}>
        {rulesOpen ? "刷新规则" : "查看规则"}
      </button>
    </div>

    {rulesOpen && (
      <div style={{ marginTop: 14 }}>
        <input
          className="input"
          placeholder="过滤规则（域名/关键词）…"
          value={ruleFilter}
          onChange={(e) => setRuleFilter(e.target.value)}
          style={{ width: "100%", marginBottom: 8 }}
          spellCheck={false}
        />
        {rulesLoading ? (
          <div className="empty">加载中…</div>
        ) : (
          <div
            style={{
              maxHeight: 280,
              overflowY: "auto",
              border: "1px solid var(--border-soft)",
              borderRadius: 8,
            }}
          >
            {rules
              .filter((r) => {
                const q = ruleFilter.trim().toLowerCase();
                if (!q) return true;
                return (
                  r.payload.toLowerCase().includes(q) ||
                  r.proxy.toLowerCase().includes(q) ||
                  r.type.toLowerCase().includes(q)
                );
              })
              .slice(0, 200)
              .map((r, i) => (
                <div
                  key={`${r.type}|${r.payload}|${i}`}
                  style={{
                    display: "flex",
                    gap: 10,
                    padding: "6px 10px",
                    fontSize: 11.5,
                    borderBottom: "1px solid var(--border-soft)",
                    alignItems: "center",
                  }}
                >
                  <span
                    style={{
                      color: "var(--accent)",
                      fontFamily: "var(--mono)",
                      minWidth: 74,
                    }}
                  >
                    {r.type}
                  </span>
                  <span
                    style={{
                      flex: 1,
                      overflow: "hidden",
                      textOverflow: "ellipsis",
                      whiteSpace: "nowrap",
                      fontFamily: "var(--mono)",
                    }}
                    title={r.payload}
                  >
                    {r.payload}
                  </span>
                  <span
                    style={{
                      color: "var(--text-faint)",
                      fontFamily: "var(--mono)",
                      minWidth: 60,
                      textAlign: "right",
                    }}
                  >
                    → {r.proxy}
                  </span>
                </div>
              ))}
            {rules.length > 200 && (
              <div style={{ padding: 8, fontSize: 11, color: "var(--text-faint)" }}>
                规则较多，只显示前 200 条（可用过滤缩小范围）
              </div>
            )}
          </div>
        )}
      </div>
    )}
  </div>

  <div className="settings-section">系统信息</div>
  <div className="card">
    <div className="card-head">
      <span className="card-title">运行信息</span>
      <button className="btn sm" onClick={() => run(() => svc.OpenDataDir())}>
        打开数据目录
      </button>
    </div>
    <div className="kv">
    <span className="kv-k">
      内核版本
      {coreCustom && <span className="hint">已自定义升级</span>}
    </span>
    <span
      className="kv-v"
      style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}
    >
      {fmtVersion(status.version)}
      {coreChecking ? (
        <span className="hint">检查中…</span>
      ) : coreInfo?.hasUpdate ? (
        <button
          className="btn sm primary"
          onClick={onUpdateCore}
          disabled={coreUpdating}
        >
          {coreUpdating ? "更新中…" : `升级到 ${coreInfo.latest}`}
        </button>
      ) : coreInfo ? (
        <button className="btn sm" onClick={onCheckCore}>
          已是最新
        </button>
      ) : (
        <button className="btn sm" onClick={onCheckCore}>
          检查更新
        </button>
      )}
      {coreCustom && (
        <button className="btn sm" onClick={onResetCore}>
          回退内置
        </button>
      )}
    </span>
  </div>
  <div className="kv">
    <span className="kv-k">混合端口</span>
    <span className="kv-v">{status.mixedPort || "—"}</span>
  </div>
  <div className="kv">
    <span className="kv-k">控制 API</span>
    <span className="kv-v">{status.apiPort || "—"}</span>
  </div>
  <div className="kv">
    <span className="kv-k">数据目录</span>
    <span className="kv-v" style={{ fontSize: 11.5 }}>
      {status.dataDir || "—"}
    </span>
  </div>
  <div className="kv">
    <span className="kv-k">
      地图本机位置
      <span className="hint">地图连线的起点，默认东莞</span>
    </span>
    <span style={{ display: "flex", gap: 6, alignItems: "center" }}>
      <input
        className="input"
        style={{ width: 80 }}
        value={homeEdit.lon}
        onChange={(e) => setHomeEdit((p) => ({ ...p, lon: e.target.value }))}
        placeholder="经度"
        spellCheck={false}
      />
      <input
        className="input"
        style={{ width: 80 }}
        value={homeEdit.lat}
        onChange={(e) => setHomeEdit((p) => ({ ...p, lat: e.target.value }))}
        placeholder="纬度"
        spellCheck={false}
      />
      <button
        className="btn sm"
        onClick={() => {
          const lon = parseFloat(homeEdit.lon);
          const lat = parseFloat(homeEdit.lat);
          if (!Number.isFinite(lon) || !Number.isFinite(lat)) {
            setErr("经纬度必须是数字");
            schedErrClear(TOAST_ERROR_MS);
            return;
          }
          void run(async () => {
            await svc.SetHomeLocation(lon, lat, "本机");
            setHomeLoc({ lon, lat });
            setNote("地图本机位置已更新");
            schedNoteClear(TOAST_NOTE_MS);
          });
        }}
      >
        保存
      </button>
    </span>
  </div>
  <div className="kv">
    <span className="kv-k">进程识别</span>
    <span className="kv-v">always（已开启）</span>
  </div>
  <div className="kv">
    <span className="kv-k">
      开机自启
      <span className="hint">登录后自动在托盘启动，不开窗口</span>
    </span>
    <button
      className={`switch ${autoStart ? "on" : ""}`}
      onClick={onToggleAutoStart}
      aria-label="开机自启"
    />
  </div>
  <div className="kv">
    <span className="kv-k">
      全部直连
      <span className="hint">绕过所有规则，排查时用</span>
    </span>
    <button
      className={`switch ${status.mode === "direct" ? "on" : ""}`}
      onClick={() => onSetMode(status.mode === "direct" ? "rule" : "direct")}
      disabled={!running || busy}
      aria-label="全部直连"
    />
  </div>
  <div className="kv">
    <span className="kv-k">订阅</span>
    <span className="kv-v" style={{ fontSize: 11.5 }}>
      {sub.url ? `${sub.nodes} 节点` : "未配置"}
    </span>
  </div>
  <div className="kv">
    <span className="kv-k">代理方式</span>
    <span className="kv-v" style={status.tun ? { color: "var(--green)" } : undefined}>
      {status.tun ? "TUN（全部程序）" : "系统代理"}
    </span>
  </div>
  <div className="kv">
    <span className="kv-k">权限</span>
    <span
      className="kv-v"
      style={status.admin ? { color: "var(--green)" } : { color: "var(--amber)" }}
    >
      {status.admin ? "管理员" : "普通用户"}
    </span>
  </div>
  </div>
    </>
  );
}
