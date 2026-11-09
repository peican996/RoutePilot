#!/usr/bin/env python3
"""检查 / 获取项目内嵌的第三方资源。

这些文件加起来约 71MB，不进版本库。没有它们 `go build` 会失败，
因为 main.go 里用了 go:embed。

用法：
    python scripts/fetch-assets.py                 # 只校验，缺什么提示什么
    python scripts/fetch-assets.py --download      # 按 manifest 的 url 下载（缺什么下什么）
    python scripts/fetch-assets.py --download --force   # 全部重下，忽略本地已有文件
    python scripts/fetch-assets.py --from-dir DIR  # 从已有目录复制（推荐）
    python scripts/fetch-assets.py --placeholders  # 生成空占位文件，仅用于 CI 编译

--from-dir 的典型场景：你本地已经有一份能跑起来的构建（assets/ 下文件齐全），
换机器时直接从那儿复制，省得重新下载上百兆。

--download 是给发布流程用的：从零还原一份可用于发行的资源，
每个文件都按 manifest 里的 sha256 校验，校验不过就失败（geo 数据例外，见下）。

geo 数据（geoip.metadb / geosite.dat）跟随上游的 `latest` 标签，每天都会变，
固定哈希没有意义，所以它们的 sha256_policy 是 warn：哈希不符只告警，
其余资源默认 strict。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
import zipfile
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

# GitHub 会拒绝没有 UA 的请求
UA = "routepilot-fetch-assets/1.0 (+https://github.com/peican996/nexuslink)"


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load_manifest() -> list[dict]:
    with MANIFEST.open(encoding="utf-8") as f:
        return json.load(f)["assets"]


def policy_of(entry: dict) -> str:
    return entry.get("sha256_policy", "strict")


def verify(entry: dict, actual: str, *, action: str) -> int:
    """按策略比对哈希，返回 1 表示算作失败。"""
    if actual == entry["sha256"]:
        print(f"[OK] {entry['path']}")
        return 0
    if policy_of(entry) == "warn":
        print(f"[哈希变化] {entry['path']} —— 已{action}，版本与清单记录不同（按策略放行）")
        print(f"           清单 {entry['sha256']}")
        print(f"           实际 {actual}")
        return 0
    print(f"[哈希不符] {entry['path']} —— 已{action}")
    print(f"           期望 {entry['sha256']}")
    print(f"           实际 {actual}")
    return 1


def report(entries: list[dict]) -> int:
    """校验现状，返回缺失或哈希不符（strict）的条目数。"""
    bad = 0
    for e in entries:
        p = ROOT / e["path"]
        if not p.exists():
            print(f"[缺失] {e['path']}  ({e['name']} {e['version']})")
            bad += 1
            continue
        bad += verify(e, sha256_of(p), action="校验")
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
        bad += verify(e, sha256_of(dst), action="复制")
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


def http_download(url: str, dst: Path) -> None:
    """把 url 下到 dst。

    优先用 curl：GitHub 的 release 资产会 302 到 objects.githubusercontent.com，
    在部分网络（含 GitHub 的 Windows runner）上 urllib 会直接卡到超时，
    curl 带 --retry 稳得多。没有 curl 再退回 urllib。
    """
    curl = shutil.which("curl")
    if curl:
        cmd = [
            curl, "-fL", "--retry", "2", "--retry-delay", "2",
            "--connect-timeout", "20", "--max-time", "900",
            "-o", str(dst), url,
        ]
        r = subprocess.run(cmd, capture_output=True, text=True)
        if r.returncode == 0 and dst.exists() and dst.stat().st_size > 0:
            return
        detail = (r.stderr or r.stdout or "").strip().splitlines()
        print(f"    [curl 失败 rc={r.returncode}] {detail[-1] if detail else ''}", file=sys.stderr)
    urllib_download(url, dst)


def urllib_download(url: str, dst: Path, attempts: int = 3) -> None:
    """纯标准库兜底，带重试。"""
    last: Exception | None = None
    for i in range(1, attempts + 1):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=120) as resp, dst.open("wb") as f:
                shutil.copyfileobj(resp, f, 1 << 20)
            if dst.stat().st_size == 0:
                raise IOError("下载结果为空")
            return
        except Exception as e:  # noqa: BLE001 —— 网络层什么都可能抛
            last = e
            print(f"    [重试 {i}/{attempts}] {type(e).__name__}: {e}", file=sys.stderr)
            if i < attempts:
                time.sleep(2 * i)
    raise RuntimeError(f"下载失败: {url}") from last


def fetch_one(entry: dict) -> int:
    url = entry["url"]
    member = entry.get("member")
    dst = ROOT / entry["path"]

    print(f"[下载] {entry['path']}")
    print(f"    {url}")

    dst.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td) / "payload"
        http_download(url, tmp)
        print(f"    收到 {tmp.stat().st_size / 1048576:.1f} MB")

        if member:
            with zipfile.ZipFile(tmp) as zf:
                try:
                    data = zf.read(member)
                except KeyError:
                    names = ", ".join(zf.namelist()[:10])
                    print(f"[错误] 压缩包里没有 {member}；实际内容前几项：{names}", file=sys.stderr)
                    return 1
            dst.write_bytes(data)
        else:
            shutil.copyfile(tmp, dst)

    print(f"    写出 {dst.relative_to(ROOT)}  ({dst.stat().st_size / 1048576:.1f} MB)")
    return verify(entry, sha256_of(dst), action="下载")


def download(entries: list[dict], force: bool) -> int:
    """按 manifest 下载缺失（或哈希不对）的资源。"""
    bad = 0
    for e in entries:
        dst = ROOT / e["path"]
        if "url" not in e:
            print(f"[跳过] {e['path']} 没有配置 url，请手动获取")
            bad += 1
            continue
        if dst.exists() and not force:
            if sha256_of(dst) == e["sha256"]:
                print(f"[已就绪] {e['path']}")
                continue
            if policy_of(e) == "warn":
                # 跟随 latest 的数据文件，本地已有一份就不折腾；要刷新用 --force
                print(f"[已存在] {e['path']} —— 哈希与清单记录不同，按策略沿用本地文件")
                continue
            print(f"[过期] {e['path']} —— 哈希与清单不符，重新下载")
        try:
            bad += fetch_one(e)
        except Exception as ex:  # noqa: BLE001
            print(f"[失败] {e['path']}: {ex}", file=sys.stderr)
            bad += 1
    return bad


def main() -> int:
    ap = argparse.ArgumentParser(description="检查/获取内嵌资源")
    ap.add_argument("--from-dir", type=Path, help="从已有目录复制资源")
    ap.add_argument("--download", action="store_true", help="按 manifest 的 url 下载")
    ap.add_argument("--force", action="store_true", help="配合 --download：忽略本地已有文件全部重下")
    ap.add_argument("--placeholders", action="store_true", help="生成空占位文件（CI 用）")
    args = ap.parse_args()

    entries = load_manifest()

    if args.placeholders:
        return placeholders(entries)

    if args.download:
        bad = download(entries, args.force)
        if bad == 0:
            # 每个文件在下载时已经校验过并打印了结论，不再重复一遍
            print("\n全部资源就绪。")
            return 0
    elif args.from_dir:
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
            "\n资源不完整。三种获取方式：\n"
            "  1) 直接下载：python scripts/fetch-assets.py --download\n"
            "  2) 从已有构建复制：python scripts/fetch-assets.py --from-dir <目录>\n"
            "  3) 按 assets/manifest.json 里的 url 手动下载，放到对应 path\n"
            "\n注意：上游若已发布新版本，sha256 会与清单不符，属正常现象——\n"
            "确认来源可信后同步更新 manifest 里的 version/url/sha256。",
            file=sys.stderr,
        )
        return 1

    print("\n全部资源就绪。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
