#!/usr/bin/env python3
"""NexusLink 发布打包脚本。

产出便携版 zip：解压即用，配置存在用户目录（不污染系统）。

用法：
    python package.py            # 用默认版本 1.0.0
    python package.py 1.2.0     # 指定版本号

流程：前端构建 → Go 编译（带图标）→ 打包 zip
"""

import os
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

ROOT = Path(__file__).parent.resolve()
DIST = ROOT / "dist-release"


def run(cmd: list[str], cwd: Path | None = None) -> None:
    print(f"  $ {' '.join(cmd)}")
    subprocess.run(cmd, cwd=cwd or ROOT, check=True, shell=(os.name == "nt"))


def main() -> int:
    version = sys.argv[1] if len(sys.argv) > 1 else "1.0.0"
    print(f"==> 打包 NexusLink v{version}")

    # 1. 前端
    print("\n[1/4] 构建前端")
    run(["npm", "run", "build"], cwd=ROOT / "frontend")

    # 2. Go 编译（rsrc 已把图标嵌进 rsrc_windows_amd64.syso）
    print("\n[2/4] 编译 Go")
    exe_name = "NexusLink.exe"
    run(
        [
            "go",
            "build",
            "-ldflags",
            "-s -w -H windowsgui",
            "-o",
            f"bin/{exe_name}",
            ".",
        ]
    )

    # 3. 准备发布目录
    print("\n[3/4] 组装发布内容")
    if DIST.exists():
        shutil.rmtree(DIST)
    DIST.mkdir(parents=True)

    shutil.copy2(ROOT / "bin" / exe_name, DIST / exe_name)

    # 便携版说明
    readme = f"""NexusLink v{version} — 便携版
================================

用法
----
1. 双击 NexusLink.exe 启动
2. 首次运行会释放 mihomo 内核到数据目录
3. 托盘图标右键可退出

配置位置
--------
订阅、规则、流量历史等存在：
    %APPDATA%\\NexusLink\\
（即 C:\\Users\\<用户名>\\AppData\\Roaming\\NexusLink）
旧版本（ProxDeck）的数据会在首次启动时自动迁移过来。
卸载时删掉这个目录即可完全清除。

模式说明
--------
- 规则模式（推荐）：按订阅规则分流，国内直连、国外走代理
- 全局模式：全部流量走代理
- 直连模式：全部直连（等于关闭代理）

注意事项
--------
- TUN 模式需要管理员权限（右键 → 以管理员身份运行）
- 首次使用请先添加订阅
"""
    (DIST / "使用说明.txt").write_text(readme, encoding="utf-8")

    # 4. 压成 zip
    print("\n[4/4] 压缩")
    zip_name = f"NexusLink-v{version}-win64-portable.zip"
    # 发布包统一放 release/，根目录只留源码
    release_dir = ROOT / "release"
    release_dir.mkdir(exist_ok=True)
    zip_path = release_dir / zip_name
    if zip_path.exists():
        zip_path.unlink()
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
        for p in sorted(DIST.rglob("*")):
            if p.is_file():
                zf.write(p, p.relative_to(DIST))

    size_mb = zip_path.stat().st_size / 1024 / 1024
    print(f"\n==> 完成: {zip_path.name}  ({size_mb:.1f} MB)")
    print(f"    位置: {zip_path}")

    # 5. 可选：NSIS 安装包
    nsis_script = ROOT / "nexuslink-installer.nsi"
    if nsis_script.exists():
        try:
            subprocess.run(
                ["makensis", "/V2", str(nsis_script)],
                cwd=ROOT,
                check=True,
                shell=(os.name == "nt"),
                capture_output=True,
            )
            setup = ROOT / f"NexusLink-{version}-setup.exe"
            if setup.exists():
                mb = setup.stat().st_size / 1024 / 1024
                print(f"    安装包: {setup.name}  ({mb:.1f} MB)")
        except FileNotFoundError:
            print("    [跳过] 未安装 makensis，不生成安装包")
        except subprocess.CalledProcessError as e:
            print(f"    [跳过] NSIS 打包失败: {e}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
