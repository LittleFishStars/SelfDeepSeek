#!/usr/bin/env python3
"""生成 / 更新应用内更新清单 latest.json。

客户端的自动更新只比 versionCode，所以清单里的版本号必须与 APK 的实际
versionCode 一致；size 与 sha256 也必须对应同一个包，否则下载后校验失败。

用法：
    # 首次生成（安卓）
    python3 tools/make-manifest.py --platform android \\
        --artifact client/android/dist/sds-1.0.0.apk \\
        --base-url https://download.example.com/deepseek \\
        --notes "首个开源版"

    # 追加鸿蒙产物（会合并进已有清单，不覆盖另一个平台）
    python3 tools/make-manifest.py --platform harmony \\
        --artifact client/harmony/dist/deepseek-harmony-1.0.0.hap \\
        --base-url https://download.example.com/deepseek \\
        --manifest latest.json --notes "首个开源版"

清单结构：
    {
      "version": "1.0.0", "versionCode": 1, "notes": "…",
      "platforms": {
        "android": {"versionCode": 1, "url": "…", "size": 12345, "sha256": "…"},
        "harmony":  {...}
      }
    }

用法要求：产物文件名里带上版本号（build.sh 已按此命名），清单里只写 URL 路径，
不写绝对路径，方便整目录搬迁。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
VERSION_FILE = ROOT / "client/android/version.txt"


def read_version() -> tuple[str, int]:
    name, code = None, None
    for line in VERSION_FILE.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line.startswith("VERSION_NAME="):
            name = line.split("=", 1)[1].strip()
        elif line.startswith("VERSION_CODE="):
            code = int(line.split("=", 1)[1].strip())
    if not name or code is None:
        raise SystemExit(f"{VERSION_FILE} 里缺少 VERSION_NAME / VERSION_CODE")
    return name, code


def guess_platform(artifact: Path) -> str:
    suffix = artifact.suffix.lower()
    if suffix == ".apk":
        return "android"
    if suffix == ".hap":
        return "harmony"
    raise SystemExit(f"看不出版本类型：{artifact.name}（支持 .apk / .hap）")


def main() -> int:
    ap = argparse.ArgumentParser(description="生成应用更新清单")
    ap.add_argument("--artifact", required=True, help="APK / HAP 路径")
    ap.add_argument("--platform", choices=["android", "harmony"], help="不填则按扩展名推断")
    ap.add_argument("--base-url", required=True, help="下载站根地址，如 https://download.example.com/deepseek")
    ap.add_argument("--manifest", default="latest.json", help="清单输出路径（默认当前目录 latest.json）")
    ap.add_argument("--notes", default="", help="更新说明，一句话")
    ap.add_argument("--version", help="覆盖版本名（默认读 client/android/version.txt）")
    ap.add_argument("--version-code", type=int, help="覆盖 versionCode（默认同上）")
    args = ap.parse_args()

    art = Path(args.artifact).resolve()
    if not art.is_file():
        raise SystemExit(f"产物不存在：{art}")

    name, code = read_version()
    if args.version:
        name = args.version
    if args.version_code is not None:
        code = args.version_code
    platform = args.platform or guess_platform(art)

    if name not in art.name:
        print(f"提醒：产物文件名 {art.name} 里没有版本号 {name}，确认不是旧包", file=sys.stderr)

    data = art.read_bytes()
    sha = hashlib.sha256(data).hexdigest()

    manifest_path = Path(args.manifest)
    if manifest_path.exists():
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as e:
            raise SystemExit(f"{manifest_path} 不是合法 JSON：{e}")
    else:
        manifest = {}

    # 顶层只有「最新的那个版本」，同一次发版两平台应一起递增
    manifest["version"] = name
    manifest["versionCode"] = code
    if args.notes:
        manifest["notes"] = args.notes
    platforms = manifest.setdefault("platforms", {})
    platforms[platform] = {
        "versionCode": code,
        "version": name,
        "url": f"{args.base_url.rstrip('/')}/{name}/{platform}/{art.name}",
        "size": len(data),
        "sha256": sha,
    }

    manifest_path.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )

    print(f"已写入 {manifest_path}：{platform} {name}({code}) "
          f"{len(data)} 字节 sha256={sha[:16]}…")
    print(f"  → {platforms[platform]['url']}")
    print("清单要在产物上传之后再原子替换到下载站：先写临时文件再 mv，避免客户端读到半个文件。")
    return 0


if __name__ == "__main__":
    sys.exit(main())