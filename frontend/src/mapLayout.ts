// 标签避让。SVG 里的文字没有自动避让，东亚节点密集时标签会叠成
// 一团糊在一起，所以手动找一个不冲突的位置。

export type LabelBox = { x: number; y: number; w: number; h: number };

function overlaps(a: LabelBox, b: LabelBox) {
  return !(a.x + a.w < b.x || b.x + b.w < a.x || a.y + a.h < b.y || b.y + b.h < a.y);
}

// 候选顺序：上、下、右、左、更上。都不行就用上方，至少保证看得见。
// 返回值是 <text> 要用的 x/y（x 是中心，y 是 baseline）。
//
// scale 是"世界坐标单位 → 屏幕像素"的缩放系数。字号在 SVG 里是世界单位，
// 放大后会跟着变大，所以要用 scale 反向补偿，让屏幕上的字保持同样大小。
export function placeLabel(
  cx: number,
  cy: number,
  text: string,
  taken: LabelBox[],
  scale: number
) {
  const fontSize = 11 * scale;
  // 中文方块字比拉丁字母宽得多，按 0.62 估算会让含中文的标签避让失效
  // （实际渲染宽度超出预留盒子，标签互相压住）。CJK 按 1.0 算。
  let em = 0;
  for (const ch of text) {
    em += /[\u3000-\u303f\u3400-\u4dbf\u4e00-\u9fff\uff00-\uffef]/.test(ch) ? 1 : 0.62;
  }
  const w = em * fontSize;
  const h = fontSize * 1.25;
  const candidates = [
    { x: cx, y: cy - fontSize * 1.1 }, // 上
    { x: cx, y: cy + fontSize * 1.9 }, // 下
    { x: cx + fontSize * 1.3 + w / 2, y: cy + fontSize * 0.35 }, // 右
    { x: cx - fontSize * 1.3 - w / 2, y: cy + fontSize * 0.35 }, // 左
    { x: cx, y: cy - fontSize * 2.4 }, // 更上
  ];
  for (const c of candidates) {
    // 文字 baseline 大约在框底部，这里近似为 y - h + 3。
    const box = { x: c.x - w / 2, y: c.y - h + 3, w, h };
    if (!taken.some((t) => overlaps(box, t))) {
      taken.push(box);
      return c;
    }
  }
  const fallback = candidates[0];
  taken.push({ x: fallback.x - w / 2, y: fallback.y - h + 3, w, h });
  return fallback;
}
