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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
<title>{{.Title}} · SelfDeepSeek | SDS</title>
<link rel="stylesheet" href="/share/assets/share.css?v={{.AssetsV}}">
</head>
<body>
<main class="page">
  <header class="head">
    <span class="brand">SelfDeepSeek | SDS</span>
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
  <footer class="foot">内容由 AI 生成，请仔细甄别</footer>
</main>
<script src="/share/assets/share.js?v={{.AssetsV}}" defer></script>
</body>
</html>
`))

var shareMD = goldmark.New(goldmark.WithExtensions(extension.GFM))

// shareAssetsVersion 返回分享页静态资源的内容指纹（取前 8 位 sha256）。
// 读取顺序：CHAT_SHARE_ASSETS 指向的源目录（部署时与线上一致），
// 其次线上目录 /opt/chatweb/share/assets。读不到就退回进程启动时间，
// 保证 URL 至少每次重启变化，不会永久钉死旧缓存。
var shareAssetsVersion = func() func() string {
	var once sync.Once
	var ver string
	return func() string {
		once.Do(func() {
			var sb strings.Builder
			for _, dir := range []string{env("CHAT_SHARE_ASSETS", ""), shareDir() + "/assets"} {
				if dir == "" {
					continue
				}
				for _, name := range []string{"share.css", "share.js"} {
					if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
						sb.Write(b)
					}
				}
				if sb.Len() > 0 {
					break
				}
			}
			if sb.Len() == 0 {
				ver = strconv.FormatInt(time.Now().Unix(), 36)
				return
			}
			sum := sha256.Sum256([]byte(sb.String()))
			ver = hex.EncodeToString(sum[:4])
		})
		return ver
	}
}()

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
	Messages []shareMessage
	// AssetsV 是 share.css/share.js 的内容指纹，作为 ?v= 拼进资源 URL。
	// 不加它的话改样式后浏览器/Cloudflare 会继续用旧文件（max-age 4 小时），
	// 表现为「代码改了、页面没变」（2026-11-22 踩过）。内容一变 URL 就变。
	AssetsV string
}

// writeSharePage 渲染并落盘
func writeSharePage(convID, shareID, title string, updated float64) error {
	nodes := coalesceToolSteps(activePath(convID))
	data := shareData{
		Title:   strings.TrimSpace(title),
		Meta:    fmtTime(updated) + " · 对话分享",
		AssetsV: shareAssetsVersion(),
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

// markdownHTML 渲染 Markdown；goldmark 默认转义原始 HTML，分享页不放行。
//
// 公式要单独保出来：goldmark 会把 `\Delta` 这类反斜杠当 Markdown 转义吃掉
// （`\Delta` → `Delta`），`$$...$$` 也会被当普通文本，页面上就只剩残缺的
// 「Delta R_A」（2026-11-22 踩过）。这里先把公式抽成占位符，渲染完再换回
// <span class="math">原文</span>，由 share.js 的 KaTeX auto-render 负责排版；
// KaTeX 没加载时也还能看出这是一段公式。
func markdownHTML(md string) template.HTML {
	protected, maths := protectMath(md)
	var sb strings.Builder
	if err := shareMD.Convert([]byte(protected), &sb); err != nil {
		return template.HTML(template.HTMLEscapeString(md))
	}
	out := sb.String()
	for i, m := range maths {
		token := mathToken(i)
		class := "math inline"
		if m.block {
			class = "math display"
		}
		// 公式原文做 HTML 转义后放进 span 的文本里，KaTeX 从 textContent 读取
		repl := `<span class="` + class + `">` + template.HTMLEscapeString(m.src) + `</span>`
		out = strings.Replace(out, token, repl, 1)
	}
	return template.HTML(out)
}

// protectedMath 是一段被保出来的公式原文
type protectedMath struct {
	src   string
	block bool
}

// mathToken 占位符：用不可能出现在正文里的形式，且必须能穿过 goldmark 不被破坏
func mathToken(i int) string {
	return "MATHPLACEHOLDER" + strconv.Itoa(i) + "X"
}

// protectMath 把 $$...$$ 与 $...$ 抽成占位符，返回替换后的文本与公式表。
// 代码块（``` / ~~~ / 行内 `code`）里的美元符号不动，避免把代码里的 $ 当公式。
func protectMath(md string) (string, []protectedMath) {
	var maths []protectedMath
	var out strings.Builder

	// 先按代码块切开，奇数项是代码块，原样保留
	parts := codeBlockSplit(md)
	for pi, part := range parts {
		if pi%2 == 1 {
			out.WriteString(part)
			continue
		}
		out.WriteString(protectMathInText(part, &maths))
	}
	return out.String(), maths
}

// codeBlockSplit 按围栏代码块切分：返回 [文本, 代码块, 文本, 代码块, ...]
func codeBlockSplit(md string) []string {
	var parts []string
	lines := strings.Split(md, "\n")
	var buf strings.Builder
	var code strings.Builder
	inFence := false
	var fence string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inFence {
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				fence = trimmed[:3]
				inFence = true
				parts = append(parts, buf.String())
				buf.Reset()
				code.Reset()
				code.WriteString(line + "\n")
				continue
			}
			buf.WriteString(line + "\n")
			continue
		}
		code.WriteString(line + "\n")
		if strings.HasPrefix(trimmed, fence) {
			inFence = false
			parts = append(parts, code.String())
		}
	}
	if inFence {
		// 围栏没闭合：整段当代码，别丢
		buf.WriteString(code.String())
	}
	parts = append(parts, buf.String())
	return parts
}

// protectMathInText 处理非代码块文本里的公式，并把行内 `code` 跳过
func protectMathInText(text string, maths *[]protectedMath) string {
	var out strings.Builder
	i := 0
	n := len(text)
	for i < n {
		// 行内代码：原样跳过
		if text[i] == '`' {
			j := strings.IndexByte(text[i+1:], '`')
			if j >= 0 {
				out.WriteString(text[i : i+j+2])
				i += j + 2
				continue
			}
		}
		// 块级公式 $$...$$
		if strings.HasPrefix(text[i:], "$$") {
			if end := strings.Index(text[i+2:], "$$"); end >= 0 {
				src := text[i : i+2+end+2]
				token := mathToken(len(*maths))
				*maths = append(*maths, protectedMath{src: src, block: true})
				out.WriteString(token)
				i += 2 + end + 2
				continue
			}
		}
		// 行内公式 $...$（要求紧跟非空白，且闭合前不跨空行）
		if text[i] == '$' {
			if end := scanInlineMath(text, i); end > 0 {
				src := text[i : end+1]
				token := mathToken(len(*maths))
				*maths = append(*maths, protectedMath{src: src, block: false})
				out.WriteString(token)
				i = end + 1
				continue
			}
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String()
}

// scanInlineMath 从 start（指向开头的 $）找行内公式的闭合 $，返回其下标；找不到返回 -1。
// 规则：开头 $ 后面不能是空白；闭合 $ 前面不能是空白；中途不跨空行。
func scanInlineMath(text string, start int) int {
	if start+1 >= len(text) || text[start+1] == ' ' || text[start+1] == '\t' || text[start+1] == '\n' {
		return -1
	}
	for i := start + 1; i < len(text); i++ {
		c := text[i]
		if c == '\\' {
			i++ // 跳过被转义的字符
			continue
		}
		if c == '\n' {
			// 空行意味着段落结束，不是公式
			if i+1 < len(text) && text[i+1] == '\n' {
				return -1
			}
			continue
		}
		if c == '$' {
			if i > start+1 && text[i-1] != ' ' && text[i-1] != '\t' && text[i-1] != '\n' {
				return i
			}
			return -1
		}
	}
	return -1
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

// rerenderAllShares 按当前模板重写全部分享页（保留原 share_id，链接不变）。
// 改分享页模板后用它批量刷新；单条失败只记日志，不影响其它页面。
func rerenderAllShares() {
	rows, err := db.Query(`SELECT id, share_id, title, updated_at FROM conversations WHERE share_id<>''`)
	if err != nil {
		log.Printf("!! 读取分享列表失败: %v", err)
		return
	}
	type item struct {
		convID, shareID, title string
		updated                float64
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.convID, &it.shareID, &it.title, &it.updated); err == nil {
			items = append(items, it)
		}
	}
	rows.Close()

	ok, fail := 0, 0
	for _, it := range items {
		if err := writeSharePage(it.convID, it.shareID, it.title, it.updated); err != nil {
			log.Printf("!! 重写分享页失败 conv=%s id=%s: %v", short(it.convID), it.shareID, err)
			fail++
			continue
		}
		ok++
	}
	log.Printf("分享页重写完成：成功 %d，失败 %d", ok, fail)
}
