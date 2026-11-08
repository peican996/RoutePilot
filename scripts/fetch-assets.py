#!/usr/bin/env python3
"""检查 / 获取项目内嵌的第三方资源。

这些文件加起来约 71MB，不进版本库。没有它们 `go build` 会失败，
因为 main.go 里用了 go:embed。

用法：
    python scripts/fetch-assets.py                 # 只校验，缺什么提示什么
    python scripts/fetch-assets.py --from-dir DIR  # 从已有目录复制（推荐）
    python scripts/fetch-assets.py --placeholders  # 生成空占位文件，仅用于 CI 编译

--from-dir 的典型场景：你本地已经有一份能跑起来的构建（assets/ 下文件齐全），
换机器时直接从那儿复制，省得重新下载上百兆。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import sys
from pathlib import Path

# Windows 上 stdout 默认是 cp1252，打印中文会抛 UnicodeEncodeError
# （CI 里就是死在这里）。强制切 UTF-8，坏字符替换而不是崩。
for _stream in (sys.stdout, sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

ROOT = Path(__file__).resolve().parent.parent
MANIFEST = ROOT / "assets" / "manifest.json"


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load_manifest() -> list[dict]:
    with MANIFEST.open(encoding="utf-8") as f:
        return json.load(f)["assets"]


def report(entries: list[dict]) -> int:
    """校验现状，返回缺失或哈希不符的条目数。"""
    bad = 0
    for e in entries:
        p = ROOT / e["path"]
        if not p.exists():
            print(f"[缺失] {e['path']}  ({e['name']} {e['version']})")
            bad += 1
            continue
        got = sha256_of(p)
        if got != e["sha256"]:
            print(f"[哈希不符] {e['path']}")
            print(f"           期望 {e['sha256']}")
            print(f"           实际 {got}")
            bad += 1
        else:
            print(f"[OK] {e['path']}")
    return bad


def copy_from(entries: list[dict], src: Path) -> int:
    """从已有目录复制。仍然按哈希校验，不信任来源。"""
    bad = 0
    for e in entries:
        rel = Path(e["path"])
        cand = src / rel.name
        if not cand.exists():
            # 允许 assets/geo/xxx 这种结构在源目录里是平铺的
            cand = src / rel
        if not cand.exists():
            print(f"[跳过] 源目录找不到 {rel.name}")
            bad += 1
            continue
        dst = ROOT / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(cand, dst)
        got = sha256_of(dst)
        if got != e["sha256"]:
            print(f"[哈希不符] {rel} —— 已复制但版本与清单不同")
            print(f"           期望 {e['sha256']}")
            print(f"           实际 {got}")
            bad += 1
        else:
            print(f"[OK] {rel}")
    return bad


def placeholders(entries: list[dict]) -> int:
    """生成空文件，让 go:embed 能编过。仅用于 CI 的类型检查与单元测试。"""
    for e in entries:
        p = ROOT / e["path"]
        if p.exists():
            continue
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(b"")
        print(f"[占位] {e['path']}")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description="检查/获取内嵌资源")
    ap.add_argument("--from-dir", type=Path, help="从已有目录复制资源")
    ap.add_argument("--placeholders", action="store_true", help="生成空占位文件（CI 用）")
    args = ap.parse_args()

    entries = load_manifest()

    if args.placeholders:
        return placeholders(entries)

    if args.from_dir:
        if not args.from_dir.is_dir():
            print(f"目录不存在: {args.from_dir}", file=sys.stderr)
            return 2
        bad = copy_from(entries, args.from_dir)
    else:
        bad = 0

    print("\n--- 校验 ---")
    bad = report(entries)
    if bad:
        print(
            "\n资源不完整。两种获取方式：\n"
            "  1) 从已有构建复制：python scripts/fetch-assets.py --from-dir <目录>\n"
            "  2) 按 assets/manifest.json 里的 source 手动下载，放到对应 path\n"
            "\n注意：上游若已发布新版本，sha256 会与清单不符，属正常现象——\n"
            "确认来源可信后同步更新 manifest 里的 version 与 sha256。",
            file=sys.stderr,
        )
        return 1

    print("\n全部资源就绪。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
