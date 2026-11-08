// 节点与订阅面板：代理模式切换、订阅卡片、节点列表。
// 面板私有的 UI 状态（过滤词、订阅输入框、测速排序）由外部持有，
// 因为 3 秒轮询会把新数据推下来，状态放在 App 里更不容易错位。

import type { Dispatch, SetStateAction } from "react";

import { svc } from "../api";
import { TOAST_NOTE_MS } from "../constants";
import type { NodeView, Status, SubInfo, SubItem } from "../types";
import { fmtAgo, fmtBytes, fmtExpire, subHost } from "../utils";

export const MODES = [
  { id: "rule", label: "规则" },
  { id: "global", label: "全局" },
] as const;

type Props = {
  status: Status;
  running: boolean;
  busy: boolean;
  sub: SubInfo;
  subItems: SubItem[];
  subBusy: boolean;
  subInput: string;
  editingSub: boolean;
  leafNodes: NodeView[];
  filteredNodes: NodeView[];
  mainGroup: NodeView | null;
  activeGroupName: string;
  activeNode: string;
  delays: Record<string, number>;
  favSet: Set<string>;
  nodeFilter: string;
  sortByDelay: boolean;
  testing: boolean;
  histTip: (name: string) => string;
  histTrend: (name: string) => string;
  setNodeFilter: (v: string) => void;
  setNodeMenu: (v: { x: number; y: number; name: string } | null) => void;
  setSortByDelay: Dispatch<SetStateAction<boolean>>;
  setSubInput: (v: string) => void;
  setEditingSub: Dispatch<SetStateAction<boolean>>;
  setFavSet: Dispatch<SetStateAction<Set<string>>>;
  setNote: (v: string) => void;
  setErr: (v: string) => void;
  schedNoteClear: (ms: number) => void;
  onRefreshSub: () => void;
  onSwitchSub: (url: string) => void;
  onRemoveSub: (url: string, name: string) => void;
  onSubscribe: () => void;
  onSetMode: (m: string) => void;
  onTestAll: () => void;
  run: (fn: () => Promise<void>) => Promise<unknown>;
  reportErr: (e: unknown) => void;
};

export default function NodesPanel({
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
}: Props) {
  return (
    <>
  <div
    style={{
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      marginBottom: 14,
      gap: 16,
    }}
  >
    <div style={{ minWidth: 0 }}>
      <div style={{ fontSize: 13, color: "var(--text)" }}>代理模式</div>
      <div
        style={{ fontSize: 11.5, color: "var(--text-faint)", marginTop: 2 }}
      >
        {status.mode === "global"
          ? "忽略全部规则和进程策略，所有流量走主策略组"
          : "按订阅规则和进程策略分流"}
      </div>
    </div>
    <div className="segmented">
      {MODES.map((m) => (
        <button
          key={m.id}
          className={status.mode === m.id ? "active" : ""}
          onClick={() => onSetMode(m.id)}
          disabled={!running || busy}
        >
          {m.label}
        </button>
      ))}
    </div>
  </div>

  <div className="card">
    <div className="card-head">
      <span className="card-title">订阅</span>
      {sub.url && (
        <div style={{ display: "flex", gap: 8 }}>
          <button className="btn sm" onClick={onRefreshSub} disabled={subBusy}>
            {subBusy ? "刷新中…" : "刷新"}
          </button>
          <button className="btn sm" onClick={() => setEditingSub((v) => !v)}>
            {editingSub ? "取消" : "更换"}
          </button>
        </div>
      )}
    </div>

    {sub.url && !editingSub ? (
      <div className="sub-list">
        {subItems.map((it: any, i: number) => {
          const used = (it.upload ?? 0) + (it.download ?? 0);
          const total = it.total ?? 0;
          const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
          return (
            <div
              key={it.url || i}
              className={`sub-item ${it.active ? "active" : ""}`}
              onClick={() => onSwitchSub(it.url)}
              title={it.url}
            >
              <div className="sub-item-head">
                <span className="sub-name">{it.name || subHost(it.url)}</span>
                <div style={{ display: "flex", gap: 6 }}>
                  <button
                    className="sub-del"
                    onClick={(e) => {
                      e.stopPropagation();
                      void (async () => {
                        try {
                          await svc.CopySubscriptionURL(it.url);
                          setNote("订阅链接已复制，可直接粘贴到手机客户端");
                        } catch (err) {
                          setErr(err instanceof Error ? err.message : String(err));
                        }
                        schedNoteClear(TOAST_NOTE_MS);
                      })();
                    }}
                    title="复制订阅链接（手机端客户端导入用）"
                  >
                    复制
                  </button>
                  {!it.active && subItems.length > 1 && (
                    <button
                      className="sub-del"
                      onClick={(e) => {
                        e.stopPropagation();
                        onRemoveSub(it.url, it.name || subHost(it.url));
                      }}
                      title="删除这个订阅"
                    >
                      删除
                    </button>
                  )}
                </div>
              </div>
              <div className="sub-line">
                {subHost(it.url)} · {fmtAgo(it.fetchedAt)}更新
              </div>

              {/* 流量：已用 / 总量 + 百分比。total 为 0 表示机场不限量。 */}
              <div className="sub-usage">
                <span>
                  已用 <b>{fmtBytes(used)}</b>
                  <span className="sub-usage-total">
                    {" / "}
                    {total > 0 ? fmtBytes(total) : "不限量"}
                  </span>
                </span>
                {total > 0 && (
                  <span
                    className={`sub-pct ${pct > 90 ? "danger" : pct > 70 ? "warn" : ""}`}
                  >
                    {pct < 1 && pct > 0 ? "<1" : pct.toFixed(0)}%
                  </span>
                )}
              </div>

              {total > 0 && (
                <div className="sub-bar" style={{ marginTop: 6 }}>
                  <div
                    className="sub-bar-fill"
                    style={{
                      width: `${pct}%`,
                      background:
                        pct > 90
                          ? "var(--red)"
                          : pct > 70
                          ? "var(--amber)"
                          : "var(--accent)",
                    }}
                  />
                </div>
              )}

              <div className="sub-usage sub-usage-foot">
                <span>
                  {total > 0
                    ? `剩余 ${fmtBytes(Math.max(0, total - used))}`
                    : "无限流量"}
                </span>
                {(() => {
                  const ex = fmtExpire(it.expire ?? 0);
                  return (
                    <span
                      className={ex.warn ? "sub-expire warn" : "sub-expire"}
                      title={ex.days === Infinity ? "长期有效" : `还剩 ${ex.days} 天`}
                    >
                      {ex.text}
                    </span>
                  );
                })()}
              </div>

              {/* 跳过证书校验是用户显式开启的危险开关，
                  开着就一直提示，点一下可以关掉。 */}
              {it.insecure && (
                <button
                  className="sub-insecure"
                  onClick={(e) => {
                    e.stopPropagation();
                    void run(async () => {
                      await svc.SetSubInsecure(it.url, false);
                      await svc.RefreshSubscription();
                      setNote("已恢复证书校验");
                    });
                  }}
                  title="当前跳过 HTTPS 证书校验，点击恢复严格校验"
                >
                  ⚠ 未校验证书 · 点击恢复
                </button>
              )}
            </div>
          );
        })}

        {String(sub.error || "").includes("证书") && (
          <button
            className="btn sm"
            onClick={() =>
              run(async () => {
                await svc.SetSubInsecure(sub.url, true);
                await svc.RefreshSubscription();
              })
            }
            style={{ alignSelf: "flex-start" }}
            title="仅在确认这是你自己的自建面板时使用"
          >
            忽略证书校验并重试
          </button>
        )}

        <button
          className="btn sm"
          onClick={() => setEditingSub(true)}
          style={{ alignSelf: "flex-start", marginTop: 2 }}
        >
          + 添加订阅
        </button>
      </div>
    ) : (
      <div style={{ display: "flex", gap: 8 }}>
        <input
          className="input"
          placeholder="https://…/api/v1/client/subscribe?token=…"
          value={subInput}
          onChange={(e) => setSubInput(e.target.value)}
          spellCheck={false}
        />
        <button className="btn primary" onClick={onSubscribe} disabled={subBusy}>
          {subBusy ? "拉取中…" : "保存并应用"}
        </button>
      </div>
    )}

    {sub.insecure && (
      <div style={{ marginTop: 10, fontSize: 11.5, color: "var(--amber)" }}>
        该订阅使用了自签名证书，已忽略校验连上。
      </div>
    )}
  </div>

  <div className="section-gap" />

  <div className="card" style={{ padding: 0 }}>
    <div className="card-head" style={{ padding: "14px 16px 0" }}>
      <span className="card-title">节点</span>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <input
          className="input"
          placeholder="过滤…"
          value={nodeFilter}
          onChange={(e) => setNodeFilter(e.target.value)}
          style={{ width: 130 }}
          spellCheck={false}
        />
        <span className="badge" title="当前生效的出口">
          {status.mode === "direct"
            ? "直连 · 未走代理"
            : status.mode === "global"
            ? `全局出口 ${activeNode || "未选择"}`
            : mainGroup
            ? `${mainGroup.name} · ${mainGroup.now}`
            : "未配置策略组"}
        </span>
        <button
          className={`btn sm ${sortByDelay ? "primary" : ""}`}
          onClick={() => setSortByDelay((v) => !v)}
          title={sortByDelay ? "当前按延迟排序，点击恢复默认顺序" : "按延迟从低到高排序"}
        >
          {sortByDelay ? "延迟 ↑" : "默认序"}
        </button>
        <button
          className="btn sm"
          onClick={onTestAll}
          disabled={testing || !running}
        >
          {testing ? "测速中…" : "测速"}
        </button>
      </div>
    </div>

    {leafNodes.length === 0 ? (
      <div className="empty" style={{ display: "grid", gap: 10, placeItems: "center" }}>
        <span>
          {sub.url ? "内核未运行，启动后显示节点" : "还没有添加订阅"}
        </span>
        {!sub.url && (
          <button className="btn sm primary" onClick={() => setEditingSub(true)}>
            + 添加订阅
          </button>
        )}
      </div>
    ) : filteredNodes.length === 0 ? (
      <div className="empty">没有匹配「{nodeFilter}」的节点</div>
    ) : (
      <div className="node-grid">
        {filteredNodes.map((n) => {
          const d = delays[n.name] !== undefined ? delays[n.name] : n.delay;
          return (
            <div
              key={n.name}
              className={`node-card ${n.name === activeNode ? "active" : ""}`}
              onClick={() =>
                activeGroupName &&
                run(() => svc.SelectNode(activeGroupName, n.name))
              }
              onContextMenu={(e) => {
                e.preventDefault();
                setNodeMenu({ x: e.clientX, y: e.clientY, name: n.name });
              }}
              title={n.name}
            >
              <div className="node-card-name">{n.name}</div>
              <div className="node-card-meta">
                <span className="node-tag">{n.type}</span>
                <span
                  className={`node-delay ${d > 0 && d < 200 ? "fast" : ""}`}
                  title={histTip(n.name)}
                >
                  {d > 0 ? `${d} ms` : d < 0 ? "不通" : "—"}
                  {histTrend(n.name)}
                </span>
                <button
                  className="fav-star"
                  title={favSet.has(n.name) ? "取消收藏" : "收藏"}
                  onClick={(e) => {
                    e.stopPropagation();
                    void svc
                      .ToggleFavorite(n.name)
                      .then((on) => {
                        setFavSet((prev) => {
                          const next = new Set(prev);
                          if (on) next.add(n.name);
                          else next.delete(n.name);
                          return next;
                        });
                      })
                      .catch(reportErr);
                  }}
                >
                  {favSet.has(n.name) ? "★" : "☆"}
                </button>
              </div>
            </div>
          );
        })}
      </div>
    )}
  </div>
    </>
  );
}
