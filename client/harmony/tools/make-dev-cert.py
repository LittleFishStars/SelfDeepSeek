#!/usr/bin/env python3
"""生成本地自签所需的证书链与 profile（OpenHarmony 官方测试材料）。

    python3 tools/make-dev-cert.py <sdk-lib-dir> <输出目录> <bundleName>

产物：
    app-cert-chain.pem   应用证书 + CA + 根证书（leaf 在前的 PEM 链）
    profile.json         调试 profile（bundle-name 已改成目标包名）

为什么这么绕：
  · OpenHarmony.p12 里 `openharmony application release` 那条存的是**自签**证书，
    不是由 CA 签发的真正应用证书 —— 直接拿它拼链，sign-app 会报
    "verify certificate chain failed"。
    真正的应用证书在 SDK 的 profile 模板里（development-certificate 字段），
    私钥仍在 p12 里，二者是一对。
  · p12 用 RC2-40-CBC 加密，OpenSSL 3 默认拒绝，必须带 -legacy 才能读出证书。
"""
import json
import pathlib
import re
import subprocess
import sys
import time

ROOT_CN = 'CN=OpenHarmony Application Root CA'
CA_CN = 'CN=OpenHarmony Application CA'


def subject_of(pem_text: str, tmp: pathlib.Path) -> str:
    tmp.write_text(pem_text)
    out = subprocess.run(['openssl', 'x509', '-in', str(tmp), '-noout', '-subject'],
                         capture_output=True, text=True).stdout
    return out


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__, file=sys.stderr)
        return 2
    lib = pathlib.Path(sys.argv[1])
    out = pathlib.Path(sys.argv[2])
    bundle = sys.argv[3]
    out.mkdir(parents=True, exist_ok=True)

    tpl_path = lib / 'UnsgnedDebugProfileTemplate.json'
    if not tpl_path.exists():
        print(f'找不到 profile 模板: {tpl_path}', file=sys.stderr)
        return 1
    tpl = json.loads(tpl_path.read_text())
    leaf = tpl['bundle-info']['development-certificate'].strip() + '\n'

    p12 = lib / 'OpenHarmony.p12'
    raw = subprocess.run(
        ['openssl', 'pkcs12', '-legacy', '-in', str(p12), '-nokeys',
         '-passin', 'pass:123456', '-nomacver'],
        capture_output=True, text=True)
    blocks = re.findall(
        r'-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----\n?', raw.stdout, re.S)
    if not blocks:
        print('从 OpenHarmony.p12 里读不到证书（openssl 需要 legacy provider）', file=sys.stderr)
        return 1

    probe = out / '.probe.pem'
    ca = root = None
    for block in blocks:
        subj = subject_of(block, probe)
        if CA_CN in subj and ca is None:
            ca = block
        elif ROOT_CN in subj and root is None:
            root = block
    probe.unlink(missing_ok=True)
    if ca is None or root is None:
        print('没在 OpenHarmony.p12 里找到应用 CA / 根证书', file=sys.stderr)
        return 1

    (out / 'app-cert-chain.pem').write_text(leaf + ca + root)

    now = int(time.time())
    tpl['validity'] = {'not-before': now - 86400, 'not-after': now + 365 * 86400}
    tpl['bundle-info']['bundle-name'] = bundle
    tpl['bundle-info']['development-certificate'] = leaf
    (out / 'profile.json').write_text(json.dumps(tpl, ensure_ascii=False, indent=2))
    print(f'证书链与 profile 已生成（bundle={bundle}）')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
