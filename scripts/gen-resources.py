#!/usr/bin/env python3
"""生成 Windows 可执行文件的资源（图标 / 版本信息 / manifest）。

产物是 rsrc_windows_amd64.syso，放在仓库根目录，Go 链接时会自动识别。
没有这个文件，exe 在资源管理器里没有图标、没有版本信息，
main.go 里按资源 ID 1 设置窗口图标的逻辑也会静默失败。

用法：
    python scripts/gen-resources.py            # 使用默认版本 1.6.0
    python scripts/gen-resources.py 1.2.3      # 发布流程传入实际版本
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TOOL = "github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.7.0"
DEFAULT_VERSION = "1.6.0"
OUTPUT = ROOT / "rsrc_windows_amd64.syso"


def main() -> int:
    version = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_VERSION
    if not re.fullmatch(r"[0-9]+(?:\.[0-9]+){1,3}", version):
        print(f"版本号格式不正确: {version}（应形如 1.2.3）")
        return 1

    cmd = [
        "go",
        "run",
        TOOL,
        "-icon",
        str(ROOT / "build" / "appicon.ico"),
        # 不嵌入 manifest：goversioninfo 生成的 RT_MANIFEST 在本机测试时
        # 会让 test/exe 报 side-by-side configuration incorrect。
        # 图标和版本信息已足够；DPI 等由 Wails/运行时处理。
        "-internal-name",
        "NexusLink",
        "-original-name",
        "NexusLink.exe",
        "-product-name",
        "NexusLink",
        "-description",
        "NexusLink",
        "-company",
        "NexusLink Project",
        "-copyright",
        "(c) 2026, NexusLink Project",
        "-comment",
        "按程序分流的 Windows 代理控制台",
        "-file-version",
        version,
        "-product-version",
        version,
        "-propagate-ver-strings",
        "-o",
        str(OUTPUT),
        str(ROOT / "build" / "windows" / "info.json"),
    ]

    print("==> 生成 Windows 资源")
    try:
        subprocess.run(cmd, cwd=ROOT, check=True)
    except FileNotFoundError:
        print("找不到 go，无法生成 Windows 资源")
        return 1
    except subprocess.CalledProcessError as e:
        print(f"生成 Windows 资源失败: {e}")
        return 1

    if not OUTPUT.exists():
        print(f"资源文件未生成: {OUTPUT}")
        return 1
    print(f"    {OUTPUT.name}  ({OUTPUT.stat().st_size / 1024:.0f} KB)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
