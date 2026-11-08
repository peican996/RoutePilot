// 界面里反复出现的数值集中在这里。
// 散落在 JSX 和 effect 里的裸数字，改一个忘一个是常见的事故源。

/** 轮询间隔（毫秒）。窗口可见时用快档，收到托盘后降到慢档省 CPU。 */
export const POLL_MAIN_MS = 1200;
export const POLL_MAIN_HIDDEN_MS = 10000;
export const POLL_LIST_MS = 3000;
export const POLL_LIST_HIDDEN_MS = 30000;

/** 日志面板多久跟一次时钟 / 图表刷新间隔。 */
export const CHART_TICK_MS = 30_000;

/** 提示条默认展示时长。错误比操作反馈停留更久，方便看清。 */
export const TOAST_NOTE_MS = 3000;
export const TOAST_INFO_MS = 5000;
export const TOAST_ERROR_MS = 6000;
export const TOAST_KERNEL_ERR_MS = 8000;

/** 连接表一次渲染多少行；点「显示更多」每次追加的数量。 */
export const CONN_PAGE_SIZE = 200;
/** 进程策略列表最多列多少个程序。 */
export const PROC_LIST_MAX = 15;
/** 地图上同一城市内合并节点的半径（世界坐标像素）。 */
export const MAP_MERGE_RADIUS = 15;
/** 地图缩放按钮的步进倍率。 */
export const MAP_ZOOM_STEP = 1.3;
/** GeoIP 结果缓存上限，超过就整体清空重查。 */
export const GEOIP_CACHE_MAX = 2000;
/** 日志面板判定"已回到底部"的容差（像素）。 */
export const LOG_FOLLOW_TOLERANCE = 40;
