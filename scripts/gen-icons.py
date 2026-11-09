#!/usr/bin/env python3
"""从一张方形 App 图标源图生成全套 Windows 图标。

产物（覆盖式）：
    assets/appicon.png          512 RGB   —— wails3 构建资产
    build/appicon.png           512 RGBA  —— 托盘默认 + 窗口/关于
    build/appicon-green.png     512 RGBA  —— 托盘：运行中
    build/appicon-amber.png     512 RGBA  —— 托盘：异常
    build/appicon-red.png       512 RGBA  —— 托盘：内核未运行
    build/appicon.ico           多尺寸    —— goversioninfo（exe 图标）
    build/windows/icon.ico      多尺寸    —— NSIS 安装包图标

状态环沿用原版规格：右下角圆心 (406,406)、外半径 64px、环宽 4px、
内部白 α40 填充。改这里会同时改三个状态图标，保持一致。

用法：
    python scripts/gen-icons.py <源图>
"""

from __future__ import annotations

import sys
from pathlib import Path

from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parent.parent

RING = {
    "green": (52, 229, 163, 255),
    "amber": (255, 180, 84, 255),
    "red": (255, 84, 112, 255),
}
RING_CENTER = (406, 406)
RING_OUTER = 64
RING_WIDTH = 4
ICO_SIZES = [16, 24, 32, 48, 64, 128, 256]


def crop_square(src: Image.Image) -> Image.Image:
    """源图是白底圆角方图，裁到内容包围盒并取正方形。

    白边不裁掉的话，缩小到 16px 时整个图标只剩一个白块。
    """
    rgb = src.convert("RGB")
    w, h = rgb.size
    # 找非白内容的包围盒；JPEG 有压缩噪声，容差放宽到 250
    mask = Image.new("L", rgb.size, 0)
    px, mp = rgb.load(), mask.load()
    for y in range(h):
        for x in range(w):
            r, g, b = px[x, y]
            if r < 250 or g < 250 or b < 250:
                mp[x, y] = 255
    bbox = mask.getbbox()
    if not bbox:
        raise SystemExit("源图全白，不像图标")
    l, t, r, b = bbox
    side = max(r - l, b - t)
    # 以包围盒中心取正方形，必要时向内收缩（源图四周留白够多）
    cx, cy = (l + r) // 2, (t + b) // 2
    half = side // 2
    box = (cx - half, cy - half, cx - half + side, cy - half + side)
    return rgb.crop(box)


def rounded_alpha(img: Image.Image, radius_frac: float = 0.225) -> Image.Image:
    """圆角透明遮罩，接近 iOS/Win11 圆角方图观感。"""
    img = img.convert("RGBA")
    w, h = img.size
    radius = int(min(w, h) * radius_frac)
    mask = Image.new("L", (w, h), 0)
    ImageDraw.Draw(mask).rounded_rectangle([0, 0, w - 1, h - 1], radius=radius, fill=255)
    img.putalpha(mask)
    return img


def with_status_ring(base: Image.Image, color: tuple[int, int, int, int]) -> Image.Image:
    out = base.copy()
    layer = Image.new("RGBA", out.size, (0, 0, 0, 0))
    d = ImageDraw.Draw(layer)
    cx, cy = RING_CENTER
    # 内部白 α40 填充
    d.ellipse(
        [cx - RING_OUTER, cy - RING_OUTER, cx + RING_OUTER, cy + RING_OUTER],
        fill=(255, 255, 255, 40),
    )
    # 彩色环
    d.ellipse(
        [
            cx - RING_OUTER + RING_WIDTH // 2,
            cy - RING_OUTER + RING_WIDTH // 2,
            cx + RING_OUTER - RING_WIDTH // 2,
            cy + RING_OUTER - RING_WIDTH // 2,
        ],
        outline=color,
        width=RING_WIDTH,
    )
    return Image.alpha_composite(out, layer)


def save_ico(img: Image.Image, path: Path) -> None:
    # PIL 写 ico：以 256 为主图，附带小尺寸重采样
    img.save(path, format="ICO", sizes=[(s, s) for s in ICO_SIZES])


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        return 2
    src_path = Path(sys.argv[1])
    if not src_path.exists():
        print(f"源图不存在: {src_path}", file=sys.stderr)
        return 1

    src = Image.open(src_path)
    print(f"源图 {src.size} {src.mode}")
    square = crop_square(src)
    print(f"裁剪为 {square.size}")

    # 512 基准图（圆角透明）
    base = rounded_alpha(square.resize((512, 512), Image.LANCZOS))

    # assets/appicon.png 保持 RGB（wails3 资产约定），不带透明
    square.resize((512, 512), Image.LANCZOS).save(ROOT / "assets" / "appicon.png")

    build = ROOT / "build"
    base.save(build / "appicon.png")
    for name, color in RING.items():
        with_status_ring(base, color).save(build / f"appicon-{name}.png")

    save_ico(base, build / "appicon.ico")
    save_ico(base, build / "windows" / "icon.ico")

    for p in [
        "assets/appicon.png",
        "build/appicon.png",
        "build/appicon-green.png",
        "build/appicon-amber.png",
        "build/appicon-red.png",
        "build/appicon.ico",
        "build/windows/icon.ico",
    ]:
        f = ROOT / p
        print(f"  {p}  {f.stat().st_size // 1024} KB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
