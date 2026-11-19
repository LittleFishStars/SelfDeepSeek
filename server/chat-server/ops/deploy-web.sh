#!/usr/bin/env bash
# 发布 chat-demo 静态站点到 https://chat.example.com/ （纯静态，接口一律在 https://api.example.com/）
set -euo pipefail
HOST="${HOST:?用法：HOST=root@你的服务器 bash deploy.sh}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${1:?用法: bash ops/deploy-web.sh <chat-demo dist 目录（含 login.html）>}"
[ -f "$SRC/index.html" ] || { echo "不是 dist 目录: $SRC"; exit 1; }
[ -f "$SRC/login.html" ] || { echo "缺少 login.html（用 app/android/app/assets 作为源）"; exit 1; }

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
# 图标（favicon / apple-touch / manifest）单独放 ops/icons，跟着静态站一起发布。
# 不能塞进 $SRC：那是前端构建产物，sync-web.sh 每次会清空重拷。
tar -czf "$TMP/web.tgz" -C "$SRC" . -C "$ROOT/ops/icons" .

echo "==> 上传到 /opt/chatweb/www"
ssh "$HOST" 'rm -rf /opt/chatweb/www && mkdir -p /opt/chatweb/www'
ssh "$HOST" 'tar -xzf - -C /opt/chatweb/www && chmod -R a+rX /opt/chatweb/www' < "$TMP/web.tgz"

echo "==> 写 nginx 配置（幂等）"
ssh "$HOST" 'cat > /tmp/setup_root_web.py' <<'PYCFG'
import re

path = "/etc/nginx/sites-available/deepseek"
lines = open(path, encoding="utf-8").read().split("\n")

targets = (
    "location = / {", "location = /login.html {", "location = /index.html {",
    "location = /icon.png {", "location /static/ {",
    "location /chat/app/ {", "location = /chat/app {",
    # 分享页也由本脚本重建：不列入 targets 的话，手动加过的 /share/ 会和
    # block 里新写的那份重复，nginx 直接报 duplicate location
    "location /share/ {",
)

# 图标段不能靠精确行匹配：它们既有正则 location 也有 exact location，
# 只清理 targets 会漏掉、每次部署都叠一份，最后 nginx 报 duplicate location。
ICON_PAT = re.compile(
    r"^\s*location\s+(?:=\s*|~\s*\^?)?/?[^{]*"
    r"(favicon|apple-touch-icon|icon-\d+|site\.webmanifest)", re.I)

out = []
i = 0
while i < len(lines):
    if lines[i].strip() in targets or ICON_PAT.match(lines[i]):
        if out and out[-1].strip().startswith("#"):
            out.pop()
        depth = 0
        while i < len(lines):
            depth += lines[i].count("{") - lines[i].count("}")
            i += 1
            if depth <= 0:
                break
        continue
    out.append(lines[i])
    i += 1

s = re.sub(r"\n{3,}", "\n\n", "\n".join(out))
block = """    # 网页端：静态前端挂在根路径，接口在 https://api.example.com/
    location = / {
        root /opt/chatweb/www;
        try_files /login.html =404;
    }
    location = /login.html {
        root /opt/chatweb/www;
    }
    location = /index.html {
        root /opt/chatweb/www;
    }
    location = /icon.png {
        root /opt/chatweb/www;
    }
    location /static/ {
        root /opt/chatweb/www;
        expires 7d;
        add_header Cache-Control "public";
    }
    # 会话分享页：静态 HTML 在 /opt/chatweb/share（www 之外，前端发布会 rm -rf www），
    # 链接不带 .html，CSS/JS 全站共用 /share/assets/share.{css,js}
    location /share/ {
        root /opt/chatweb;
        try_files $uri $uri.html =404;
        add_header X-Content-Type-Options nosniff;
        add_header Referrer-Policy no-referrer;
    }
    location /chat/app/ {
        return 301 /;
    }
    # 图标：真实文件直出（原先 favicon.ico 301 到 icon.png，部分客户端不认）
    location ~ ^/(favicon\.ico|favicon-\d+x\d+\.png|apple-touch-icon\.png|icon-\d+\.png)$ {
        root /opt/chatweb/www;
        expires 7d;
        add_header Cache-Control "public";
    }
    # 清单要 application/manifest+json（不能塞进上面的 regex 里用 types 改，
    # 那会覆盖继承的 mime.types，把 png/ico 也变成 manifest 类型）
    location = /site.webmanifest {
        root /opt/chatweb/www;
        default_type application/manifest+json;
        expires 7d;
        add_header Cache-Control "public";
    }

"""
# 插入位置：静态段（含兜底 location /）之前。
# 早先用 "    # chat-server API" 当锚点，那个 /api/ 反代撤掉后锚点就不存在了，
# 脚本会抛 ValueError。改为按兜底块定位，且已存在静态段时直接跳过（幂等）。
# 只插 443 的块：先定位 listen 443，再取它之后的第一个 location / {。
# 不能直接用第一个 location / {，那是 80 端口 301 重定向块里的，之前就插错过；
# 也不能依赖注释文字，清理逻辑会把紧邻的注释一起吃掉。
tls = s.find("listen 443")
if tls < 0:
    raise SystemExit("没找到 listen 443，请人工检查 " + path)
i = s.find("    location / {", tls)
if i < 0:
    raise SystemExit("443 块里没有 location / {，请人工检查 " + path)
s = s[:i] + block + s[i:]
open(path, "w", encoding="utf-8").write(s)
print("nginx 根路径配置已写入（幂等）")
PYCFG
ssh "$HOST" 'python3 /tmp/setup_root_web.py && nginx -t && systemctl reload nginx'

echo "==> 自检"
curl -s -o /dev/null -w "首页 %{http_code}\n" https://chat.example.com/
curl -s -o /dev/null -w "登录页 %{http_code}\n" https://chat.example.com/login.html
curl -s -o /dev/null -w "API   %{http_code}\n" https://api.example.com/health
# 查 PNG 而不是 /favicon.ico：后者在 Cloudflare 上有旧缓存（301 -> icon.png），
# 源站已改直出，但边缘要等 TTL 过期，用它自检会误报。
curl -s -o /dev/null -w "图标  %{http_code}\n" https://chat.example.com/favicon-32x32.png
