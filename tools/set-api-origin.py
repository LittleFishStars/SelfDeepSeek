#!/usr/bin/env python3
"""改写前端里的接口地址（后端域名）。

前端是上游 chat-demo 的编译产物，接口基址在构建期就写死进了 bundle：

    let m="https://api.example.com/chat",g="https://api.example.com/auth"

本脚本把「https://<任意域名>」后面紧跟 /chat 或 /auth 的位置整体换成你的地址，
所以自托管时不需要重新编译前端。login.html 里的 `var API = '...'` 同理会一起改。

用法：
    python3 tools/set-api-origin.py https://api.your.host      # 就地改写
    python3 tools/set-api-origin.py https://api.your.host -n   # 只看会改哪些文件
    python3 tools/set-api-origin.py --check                    # 检查各处是否一致
    python3 tools/set-api-origin.py https://api.your.host --revert   # 还原成 example.com

改完记得重新打客户端包（安卓 build.sh 也支持 --api-origin 在打包时自动调用本逻辑）。
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TARGETS = [
    ROOT / "client/android/app/assets",
    ROOT / "client/harmony/entry/src/main/resources/rawfile",
    ROOT / "server/chat-server/ops",          # 网页端登录页
]
TEXT_SUFFIX = {".js", ".html", ".htm", ".json", ".css", ".mjs"}
PLACEHOLDER = "https://api.example.com"

# 「https://host」后面紧跟 /chat 或 /auth 才算接口基址；其余（图片、文档链接等）不动
PATTERN = re.compile(r"https://[A-Za-z0-9.\-]+(?=/(?:chat|auth)\b)")


def iter_files(extra_dirs: list[Path]) -> list[Path]:
    out: list[Path] = []
    for base in TARGETS + extra_dirs:
        if not base.exists():
            continue
        for p in sorted(base.rglob("*")):
            if p.is_file() and (p.suffix in TEXT_SUFFIX or p.name == "login.html"):
                out.append(p)
    return out


def scan(paths: list[Path]) -> dict[Path, list[str]]:
    found: dict[Path, list[str]] = {}
    for p in paths:
        try:
            text = p.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError):
            continue
        hits = sorted(set(PATTERN.findall(text)))
        if hits:
            found[p] = hits
    return found


def rewrite(paths: list[Path], target: str, dry: bool) -> tuple[int, int, list[Path]]:
    changed = 0
    skipped = 0
    touched: list[Path] = []
    for p in paths:
        try:
            text = p.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError):
            continue
        new, n = PATTERN.subn(target, text)
        if n == 0:
            continue
        if new == text:
            skipped += 1
            continue
        changed += 1
        touched.append(p)
        if not dry:
            p.write_text(new, encoding="utf-8")
    return changed, skipped, touched


def main() -> int:
    ap = argparse.ArgumentParser(description="改写前端里的接口地址")
    ap.add_argument("origin", nargs="?", help="目标地址，如 https://api.your.host（不带 /chat）")
    ap.add_argument("-n", "--dry-run", action="store_true", help="只显示会改哪些文件")
    ap.add_argument("--check", action="store_true", help="检查各文件是否指向同一个地址")
    ap.add_argument("--revert", action="store_true", help="还原成占位地址 example.com")
    ap.add_argument("--also", action="append", default=[], metavar="DIR",
                    help="额外扫描的目录（可多次）")
    args = ap.parse_args()

    extra = [Path(d).resolve() for d in args.also]
    paths = iter_files(extra)
    if not paths:
        print("没找到可改写的文件（是不是没在仓库根目录执行？）", file=sys.stderr)
        return 1

    if args.check:
        found = scan(paths)
        if not found:
            print("没有找到任何接口地址 —— 前端可能不是本仓库的产物")
            return 1
        origins = sorted({h for hits in found.values() for h in hits})
        for p, hits in found.items():
            print(f"{p.relative_to(ROOT)}  →  {', '.join(hits)}")
        print()
        if len(origins) == 1:
            print(f"一致：{origins[0]}")
            return 0
        print(f"不一致：{', '.join(origins)}（跑一遍 set-api-origin.py 统一）")
        return 2

    target = PLACEHOLDER if args.revert else (args.origin or "")
    if not target:
        ap.error("缺少目标地址，或加 --check / --revert")
    if not re.fullmatch(r"https?://[A-Za-z0-9.\-]+(?::\d+)?", target):
        ap.error(f"地址格式不对：{target}（示例 https://api.your.host，不要带路径）")

    changed, skipped, touched = rewrite(paths, target, args.dry_run)
    verb = "将改写" if args.dry_run else "已改写"
    for p in touched:
        print(f"{verb} {p.relative_to(ROOT)}")
    print(f"\n{verb} {changed} 个文件 → {target}；{skipped} 个文件本来就是该地址")
    if changed and not args.dry_run:
        print("下一步：重新打包客户端（cd client/android && bash build.sh），网页端重新发布静态目录")
    return 0


if __name__ == "__main__":
    sys.exit(main())
