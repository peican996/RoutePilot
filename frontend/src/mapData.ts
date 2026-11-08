// 地图的静态数据与坐标工具。从 NodeMap 拆出来，
// 让主组件只管渲染和交互，几百行的查找表不该和 DOM 逻辑混在一起。

// ---------------------------------------------------------------------------
// 国家/地区 → 经纬度。机场节点名千奇百怪，这里只覆盖常见关键词。
// ---------------------------------------------------------------------------

export const PLACES: Array<{
  keys: string[];
  lon: number;
  lat: number;
  label: string;
}> = [
  // 具体城市必须排在国家前面。
  // 「美国LA-优化3」同时含「美国」和「LA」，如果「美国」排在前面，
  // 所有美国节点都会被映射到同一个坐标，地图上永远是一个点。
  { keys: ["洛杉矶", "LA"], lon: -118.2, lat: 34, label: "洛杉矶" },
  { keys: ["西雅图", "SEA"], lon: -122.3, lat: 47.6, label: "西雅图" },
  { keys: ["圣何塞", "SJC"], lon: -121.9, lat: 37.3, label: "圣何塞" },
  { keys: ["达拉斯", "DAL"], lon: -96.8, lat: 32.8, label: "达拉斯" },
  { keys: ["纽约", "NY"], lon: -74, lat: 40.7, label: "纽约" },
  { keys: ["东京", "大阪"], lon: 139.7, lat: 35.7, label: "东京" },
  { keys: ["香港"], lon: 114.2, lat: 22.3, label: "香港" },
  { keys: ["新加坡", "狮城"], lon: 103.8, lat: 1.3, label: "新加坡" },
  { keys: ["首尔"], lon: 127, lat: 37.5, label: "首尔" },
  { keys: ["伦敦"], lon: -0.1, lat: 51.5, label: "伦敦" },
  { keys: ["莫斯科"], lon: 37.6, lat: 55.7, label: "莫斯科" },
  { keys: ["悉尼", "墨尔本"], lon: 151.2, lat: -33.9, label: "悉尼" },

  // 国家/地区兜底
  { keys: ["日本", "JP"], lon: 138, lat: 36, label: "日本" },
  { keys: ["台湾", "TW"], lon: 121, lat: 23.7, label: "台湾" },
  { keys: ["韩国", "KR"], lon: 127.5, lat: 36.5, label: "韩国" },
  { keys: ["美国", "USA", "US"], lon: -98, lat: 39, label: "美国" },
  { keys: ["英国", "UK", "GB"], lon: -2, lat: 54, label: "英国" },
  { keys: ["德国", "DE"], lon: 10.5, lat: 51, label: "德国" },
  { keys: ["法国", "FR"], lon: 2.3, lat: 46.8, label: "法国" },
  { keys: ["荷兰", "NL"], lon: 5.5, lat: 52.2, label: "荷兰" },
  { keys: ["俄罗斯", "RU"], lon: 60, lat: 58, label: "俄罗斯" },
  { keys: ["印度", "IN"], lon: 79, lat: 22, label: "印度" },
  { keys: ["泰国", "曼谷", "TH"], lon: 100.5, lat: 15, label: "泰国" },
  { keys: ["越南", "VN"], lon: 107, lat: 14, label: "越南" },
  { keys: ["马来西亚", "MY"], lon: 102, lat: 4, label: "马来西亚" },
  { keys: ["印尼", "ID"], lon: 113, lat: -3, label: "印尼" },
  { keys: ["菲律宾", "PH"], lon: 122, lat: 13, label: "菲律宾" },
  { keys: ["澳大利亚", "AU"], lon: 134, lat: -25, label: "澳大利亚" },
  { keys: ["加拿大", "CA"], lon: -105, lat: 55, label: "加拿大" },
  { keys: ["巴西", "BR"], lon: -50, lat: -12, label: "巴西" },
  { keys: ["土耳其", "TR"], lon: 33, lat: 39, label: "土耳其" },
  { keys: ["阿联酋", "迪拜", "AE"], lon: 54, lat: 24, label: "阿联酋" },
  { keys: ["阿根廷", "AR"], lon: -64, lat: -34, label: "阿根廷" },
];

// 取**最长**的那个匹配项。
//
// 「美国LA-优化3」既能匹配「美国」也能匹配「LA」，按顺序取第一个
// 会让所有美国节点挤到同一个坐标。取最长匹配能优先命中更具体的城市。
//
// 纯字母短 key 必须整词匹配：原来用 includes 会让
//   GERMANY → NY/MY（纽约）、RUSSIA/AUSTRALIA → US（美国）、
//   CHINA → IN（印度）、UKRAINE → KR（韩国）、POLAND/IRELAND → LA（洛杉矶）
// 整张地图的落点都是错的。中文 key 不存在词边界问题，继续用包含匹配。
const ASCII_KEY = /^[A-Za-z]+$/;

export function resolvePlace(nodeName: string) {
  const tokens = nodeName
    .toUpperCase()
    .split(/[^A-Z]+/)
    .filter(Boolean);

  let best: (typeof PLACES)[number] | null = null;
  let bestLen = 0;
  for (const p of PLACES) {
    for (const k of p.keys) {
      const hit = ASCII_KEY.test(k) ? tokens.includes(k.toUpperCase()) : nodeName.includes(k);
      if (hit && k.length > bestLen) {
        best = p;
        bestLen = k.length;
      }
    }
  }
  return best ? { lon: best.lon, lat: best.lat, label: best.label } : null;
}

// 本地位置（东莞）。
export const HOME = { lon: 113.75, lat: 23.02, label: "本机" };

// 等距圆柱投影，和 make-world-path.mjs 的生成脚本保持一致。
export function project(lon: number, lat: number, W: number, H: number) {
  return {
    x: ((lon + 180) / 360) * W,
    y: ((90 - lat) / 180) * H,
  };
}

// --- 延迟显示 -----------------------------------------------------------
//
// 注意区分两种"没有数字"的情况：
//   undefined / 0 = 压根没测过
//   < 0           = 测了，但超时/不通

export function delayColor(d: number | undefined): string {
  if (d === undefined || d === 0) return "var(--text-dim)";
  if (d < 0) return "var(--red)";
  if (d < 200) return "var(--green)";
  if (d < 500) return "var(--amber)";
  return "var(--red)";
}

export function delayText(d: number | undefined): string {
  if (d === undefined || d === 0) return "未测速";
  if (d < 0) return "不通";
  return `${d} ms`;
}
