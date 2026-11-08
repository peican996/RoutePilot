import { Component, type ErrorInfo, type ReactNode } from "react";

type Props = { children: ReactNode };
type State = { error: Error | null };

/**
 * 顶层错误边界。
 *
 * 没有它的话，任何一个组件在渲染期抛错（比如 hook 顺序错乱），React 会
 * 卸载整棵树，用户看到的就是一个纯黑空窗口——既没有提示也拿不到线索。
 * 这里至少把错误内容显示出来，并给一个"重试"入口。
 */
export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("[nexuslink] 界面渲染失败", error, info.componentStack);
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;

    return (
      <div
        style={{
          padding: "32px 28px",
          fontFamily: "var(--sans, system-ui)",
          color: "var(--text, #e6e9ef)",
          lineHeight: 1.7,
        }}
      >
        <div style={{ fontSize: 15, fontWeight: 600, marginBottom: 10 }}>
          界面出现异常，已停止渲染以保护数据
        </div>
        <div style={{ fontSize: 12.5, color: "var(--text-dim, #9aa4b2)", marginBottom: 16 }}>
          把下面这段信息发给开发者即可定位问题。内核仍在后台运行，代理不受影响。
        </div>
        <pre
          style={{
            fontSize: 11.5,
            whiteSpace: "pre-wrap",
            wordBreak: "break-word",
            background: "rgba(255,255,255,0.04)",
            border: "1px solid var(--border-soft, #2a2f38)",
            borderRadius: 6,
            padding: "10px 12px",
            maxHeight: 240,
            overflow: "auto",
          }}
        >
          {error.message}
          {error.stack ? "\n\n" + error.stack.split("\n").slice(0, 8).join("\n") : ""}
        </pre>
        <button
          className="btn sm"
          style={{ marginTop: 14 }}
          onClick={() => this.setState({ error: null })}
        >
          重试
        </button>
      </div>
    );
  }
}
