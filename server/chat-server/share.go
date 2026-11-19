package main

// 会话分享：把当前分支渲染成一张**静态 HTML**，落到 chat.example.com 的静态目录。
//
// 为什么不做动态页：分享出去的链接要能被任何人直接打开，不该经过 API、不该带鉴权，
// 也不该因为服务重启/改版就失效。静态文件最省事。
//
// 体积：每个分享页只带正文 HTML，CSS / JS 全部指向
// /share/assets/share.{css,js}（全站共用一份），图片按 fileID 去重后共用。
//
// 目录约定（可用 CHAT_SHARE_DIR 覆盖）：
//   /opt/chatweb/share/<uid>.html              单页
//   /opt/chatweb/share/assets/share.css        共用样式
//   /opt/chatweb/share/assets/share.js         共用脚本
//   /opt/chatweb/share/img/<fileID>            分享里的图片（与上传目录同一份内容）
//
// **不能放在 www 里**：ops/deploy-web.sh 每次发布前端都会 `rm -rf /opt/chatweb/www`，
// 分享页会被一起删掉。所以放在 www 外面，nginx 用 `root /opt/chatweb;` 映射。
// nginx 侧要 `location /share/ { root /opt/chatweb; try_files $uri $uri.html =404; }`
// 才能让 /share/<uid> 命中 <uid>.html。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// 分享链接前缀与站点地址：换域名用环境变量覆盖，别改代码
var (
	shareBaseURL = env("CHAT_SHARE_BASE", "https://chat.example.com/share/")
	shareSiteURL = env("CHAT_WEB_BASE", "https://chat.example.com")
)

// shareAssets 由 deploy.sh 安装；这里只负责把单页写出来
var shareTpl = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="robots" content="noindex,nofollow">
<title>{{.Title}} · DeepSeek 自托管版</title>
<link rel="stylesheet" href="/share/assets/share.css">
</head>
<body>
<main class="page">
  <header class="head">
    <a class="brand" href="{{.Site}}">DeepSeek 自托管版</a>
    <h1>{{.Title}}</h1>
    <div class="meta">{{.Meta}}</div>
  </header>
  <section class="thread">
  {{range .Messages}}
    <article class="msg msg-{{.Role}}">
      <div class="who">{{.Who}}</div>
      <div class="body">{{.HTML}}</div>{{if .Files}}
      <div class="shots">{{range .Files}}<img src="{{.}}" alt="附件" loading="lazy">{{end}}</div>{{end}}
    </article>
  {{end}}
  </section>
  <footer class="foot">内容由 AI 生成，请仔细甄别 · <a href="{{.Site}}">试试这个客户端</a></footer>
</main>
<script src="/share/assets/share.js" defer></script>
</body>
</html>
`))

var shareMD = goldmark.New(goldmark.WithExtensions(extension.GFM))

// shareDir 分享页落盘目录
func shareDir() string {
	return env("CHAT_SHARE_DIR", "/opt/chatweb/share")
}

// shareID 10 字节随机 → 20 位十六进制，肉眼不可猜但也不长
func shareID() string {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// handleShare 处理 /v1/conversations/{id}/share
//
//	POST   建分享（已存在则复用，不重复生成）
//	GET    查看当前分享链接
//	DELETE 取消分享（删文件 + 清字段）
func handleShare(w http.ResponseWriter, r *http.Request, uid, convID string) {
	var owner, title string
	var updated float64
	err := db.QueryRow(`SELECT user_id, COALESCE(title,''), updated_at FROM conversations WHERE id=?`, convID).
		Scan(&owner, &title, &updated)
	if err != nil || owner != uid {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}

	var current string
	_ = db.QueryRow(`SELECT COALESCE(share_id,'') FROM conversations WHERE id=?`, convID).Scan(&current)

	switch r.Method {
	case http.MethodGet:
		if current == "" {
			writeJSON(w, 200, map[string]any{"shared": false})
			return
		}
		writeJSON(w, 200, map[string]any{"shared": true, "id": current, "url": shareBaseURL + current})
		return

	case http.MethodDelete:
		if current != "" {
			_ = os.Remove(filepath.Join(shareDir(), current+".html"))
			storeMu.Lock()
			_, _ = db.Exec(`UPDATE conversations SET share_id='' WHERE id=?`, convID)
			storeMu.Unlock()
		}
		writeJSON(w, 200, map[string]any{"shared": false})
		return

	case http.MethodPost:
		// 已分享过就直接复用：用户重复点「分享」应该拿到同一条链接
		if current != "" {
			if _, err := os.Stat(filepath.Join(shareDir(), current+".html")); err == nil {
				writeJSON(w, 200, map[string]any{"id": current, "url": shareBaseURL + current, "reused": true})
				return
			}
		}
		id := shareID()
		if id == "" {
			writeJSON(w, 500, map[string]any{"error": "生成分享 ID 失败"})
			return
		}
		if err := writeSharePage(convID, id, title, updated); err != nil {
			log.Printf("  分享落盘失败 conv=%s: %v", short(convID), err)
			writeJSON(w, 500, map[string]any{"error": "写入分享页失败: " + err.Error()})
			return
		}
		storeMu.Lock()
		_, _ = db.Exec(`UPDATE conversations SET share_id=? WHERE id=?`, id, convID)
		storeMu.Unlock()
		log.Printf("  分享会话 conv=%s id=%s", short(convID), id)
		writeJSON(w, 200, map[string]any{"id": id, "url": shareBaseURL + id})
		return
	}
	writeJSON(w, 405, map[string]any{"error": "method not allowed"})
}

type shareMessage struct {
	Role  string
	Who   string
	HTML  template.HTML
	Files []string
}

type shareData struct {
	Title    string
	Meta     string
	Site     string
	Messages []shareMessage
}

// writeSharePage 渲染并落盘
func writeSharePage(convID, shareID, title string, updated float64) error {
	nodes := coalesceToolSteps(activePath(convID))
	data := shareData{
		Title: strings.TrimSpace(title),
		Meta:  fmtTime(updated) + " · 对话分享",
		Site:  shareSiteURL,
	}
	if data.Title == "" {
		data.Title = "对话分享"
	}
	for _, n := range nodes {
		if n.Role != "user" && n.Role != "assistant" {
			continue
		}
		md := messageMarkdown(n)
		if strings.TrimSpace(md) == "" && n.Files == "" {
			continue
		}
		who := "我"
		if n.Role == "assistant" {
			who = "DeepSeek"
		}
		data.Messages = append(data.Messages, shareMessage{
			Role:  n.Role,
			Who:   who,
			HTML:  markdownHTML(md),
			Files: shareImages(n.Files, shareID),
		})
	}
	if len(data.Messages) == 0 {
		return errNoMessage
	}
	dir := shareDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "img"), 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, shareID+".html")
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := shareTpl.Execute(f, data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// fmtTime 分享页上的时间：人读格式，非 UTC 也没关系（服务器在 +8）
func fmtTime(sec float64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(int64(sec), 0).Format("2006-01-02 15:04")
}

var errNoMessage = &shareError{"这个会话没有可分享的内容"}

type shareError struct{ msg string }

func (e *shareError) Error() string { return e.msg }

// messageMarkdown 取一条消息的正文（只取 text 块，工具块/思考不进分享页）
func messageMarkdown(n node) string {
	if strings.TrimSpace(n.Blocks) == "" {
		return n.Content
	}
	blocks := decodeBlockList(n.Blocks)
	if len(blocks) == 0 {
		return n.Content
	}
	var sb strings.Builder
	for _, raw := range blocks {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			continue
		}
		if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(b.Text)
	}
	if sb.Len() == 0 {
		return n.Content
	}
	return sb.String()
}

// markdownHTML 渲染 Markdown；goldmark 默认转义原始 HTML，分享页不放行
func markdownHTML(md string) template.HTML {
	var sb strings.Builder
	if err := shareMD.Convert([]byte(md), &sb); err != nil {
		return template.HTML(template.HTMLEscapeString(md))
	}
	return template.HTML(sb.String())
}

// shareImages 把消息里的图片复制到 share/img（同一 fileID 只存一份），返回可引用的 URL
func shareImages(filesJSON, shareID string) []string {
	if strings.TrimSpace(filesJSON) == "" {
		return nil
	}
	var files []storedFile
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		return nil
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if !f.IsImage || f.ID == "" {
			continue
		}
		src := fileBlobPath(f.ID)
		dst := filepath.Join(shareDir(), "img", sanitizeUploadID(f.ID))
		if _, err := os.Stat(dst); err != nil {
			if err := copyFile(src, dst); err != nil {
				continue
			}
		}
		out = append(out, "/share/img/"+sanitizeUploadID(f.ID))
	}
	return out
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
