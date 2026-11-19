#!/usr/bin/env python3
"""把阿里云邮件推送（DirectMail）发信域名所需的解析写进 Cloudflare。

用法（值从邮件推送控制台「发信域名 → 配置」页抄）：

  python3 set-mail-dns.py --domain example.com \
      --spf 'v=spf1 include:spf1.dm.aliyun.com -all' \
      --mx mx01.dm.aliyun.com \
      --ownership-name aliyundm --ownership-value xxxx \
      --dkim-name aliyun-cn-hangzhou._domainkey --dkim-value 'v=DKIM1; k=rsa; p=...' \
      [--dmarc 'v=DMARC1; p=none'] [--dry-run]

只改这些记录，其它解析不动。MX 会替换根域现有的邮件服务器。
"""
import argparse, json, os, re, sys, urllib.request, urllib.error

def cf_token():
    for path in ("/root/.acme.sh/account.conf", os.path.expanduser("~/.acme.sh/account.conf")):
        try:
            txt = open(path, encoding="utf-8", errors="ignore").read()
        except OSError:
            continue
        m = re.search(r"SAVED_CF_Token=([A-Za-z0-9_\-]+)", txt)
        if m:
            return m.group(1).strip("'\"")
    sys.exit("找不到 Cloudflare token（~/.acme.sh/account.conf 的 SAVED_CF_Token）")

def api(tok, method, path, body=None):
    req = urllib.request.Request(
        "https://api.cloudflare.com/client/v4" + path, method=method,
        data=json.dumps(body).encode() if body else None,
        headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    try:
        return json.load(urllib.request.urlopen(req, timeout=25))
    except urllib.error.HTTPError as e:
        return json.load(e)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--domain", required=True)
    ap.add_argument("--spf", required=True)
    ap.add_argument("--mx", required=True)
    ap.add_argument("--ownership-name", required=True)
    ap.add_argument("--ownership-value", required=True)
    ap.add_argument("--dkim-name")
    ap.add_argument("--dkim-value")
    ap.add_argument("--dmarc", default="v=DMARC1; p=none")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    tok = cf_token()
    zones = api(tok, "GET", "/zones?name=" + a.domain)
    if not zones.get("result"):
        sys.exit("Cloudflare 里找不到域名 " + a.domain)
    zid = zones["result"][0]["id"]

    recs = [
        {"type": "TXT", "name": "@", "content": a.spf, "ttl": 300},
        {"type": "MX", "name": "@", "content": a.mx, "priority": 10, "ttl": 300},
        {"type": "TXT", "name": a.ownership_name, "content": a.ownership_value, "ttl": 300},
    ]
    if a.dkim_name and a.dkim_value:
        recs.append({"type": "TXT", "name": a.dkim_name, "content": a.dkim_value, "ttl": 300})
    if a.dmarc:
        recs.append({"type": "TXT", "name": "_dmarc", "content": a.dmarc, "ttl": 300})

    for r in recs:
        fqdn = a.domain if r["name"] == "@" else r["name"] + "." + a.domain
        if a.dry_run:
            print("[dry]", r["type"], fqdn, "=>", r["content"][:60])
            continue
        ex = api(tok, "GET", f"/zones/{zid}/dns_records?name={fqdn}&type={r['type']}")
        if ex.get("result"):
            out = api(tok, "PUT", f"/zones/{zid}/dns_records/{ex['result'][0]['id']}", r)
            print("更新" if out.get("success") else "失败", r["type"], fqdn,
                  "" if out.get("success") else out.get("errors"))
        else:
            out = api(tok, "POST", f"/zones/{zid}/dns_records", r)
            print("新增" if out.get("success") else "失败", r["type"], fqdn,
                  "" if out.get("success") else out.get("errors"))

if __name__ == "__main__":
    main()
