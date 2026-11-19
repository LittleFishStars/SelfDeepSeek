// chat-server —— 给 chat-demo 客户端用的聊天后端。
//
// 客户端（deepseek-ai/chat-demo 的改造版）只跟本服务说话：
//
//	GET  /models                     模型列表（代理官方，注入服务端 Key）
//	POST /anthropic/v1/messages      Messages API（SSE 流式，代理 + 落库）
//	POST /anthropic/v1/files         附件上传（代理）
//	DELETE /anthropic/v1/files/{id}  附件删除（代理）
//	/v1/tools/*                  已下线（410）：语音接口按用量计费，成本过高
//	GET  /v1/conversations           会话列表
//	GET  /v1/conversations/{id}      某个会话的消息
//
// 用户身份用请求头里的 x-api-key / Authorization: Bearer 区分（tokens 表）。
// 上游 Key 只存在服务端，客户端不需要知道。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	port       = envInt("CHAT_PORT", 8083)
	bind       = env("CHAT_BIND", "127.0.0.1")
	apiKey     = env("DS_API_KEY", "")
	apiBase    = strings.TrimRight(env("DS_API_BASE", "https://api.deepseek.com"), "/")
	dbPath     = env("CHAT_DB", "/var/lib/chat-server/chat.db")
	allowAnon  = env("CHAT_ALLOW_ANON", "1") == "1"
	defaultTok = env("CHAT_TOKEN", "")
	reqTimeout = time.Duration(envInt("CHAT_UPSTREAM_TIMEOUT", 300)) * time.Second
	// 客户端不传 max_tokens 时用的默认上限（模型本身允许更大）
	defaultMaxTokens = envInt("CHAT_MAX_TOKENS", 8192)
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envFloat(k string, d float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

// ---------------------------------------------------------------- 数据库

var db *sql.DB

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id         TEXT PRIMARY KEY,
  token      TEXT UNIQUE,
  created_at REAL NOT NULL,
  last_seen  REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS conversations (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL DEFAULT '',
  created_at REAL NOT NULL,
  updated_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conv_user ON conversations(user_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS messages (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  conv_id    TEXT NOT NULL,
  role       TEXT NOT NULL,
  content    TEXT NOT NULL,
  thinking   TEXT NOT NULL DEFAULT '',
  created_at REAL NOT NULL,
  files      TEXT NOT NULL DEFAULT '',
  blocks     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_msg_conv ON messages(conv_id, id);
CREATE TABLE IF NOT EXISTS conv_changes (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id TEXT NOT NULL,
  conv_id TEXT NOT NULL,
  op      TEXT NOT NULL,
  ts      REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_changes_user ON conv_changes(user_id, ts);
CREATE TABLE IF NOT EXISTS reply_cache (
  conv_id    TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL,
  payload    TEXT NOT NULL,
  created_at REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS client_tokens (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    TEXT NOT NULL,
  device     TEXT NOT NULL,
  token      TEXT NOT NULL UNIQUE,
  created_at REAL NOT NULL DEFAULT 0,
  updated_at REAL NOT NULL DEFAULT 0,
  last_sync  REAL NOT NULL DEFAULT 0,
  UNIQUE(user_id, device)
);
CREATE TABLE IF NOT EXISTS compactions (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  conv_id        TEXT NOT NULL,
  created_at     REAL NOT NULL,
  tokens_before  INTEGER NOT NULL DEFAULT 0,
  messages_before INTEGER NOT NULL DEFAULT 0,
  summary        TEXT NOT NULL
);
`

// migrateStore 补齐后加的列（老库平滑升级）
func migrateStore() {
	cols := []struct{ table, name, ddl string }{
		{"conversations", "tokens_in", "ALTER TABLE conversations ADD COLUMN tokens_in INTEGER NOT NULL DEFAULT 0"},
		{"conversations", "tokens_out", "ALTER TABLE conversations ADD COLUMN tokens_out INTEGER NOT NULL DEFAULT 0"},
		{"messages", "stop_reason", "ALTER TABLE messages ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''"},
		{"messages", "parent_id", "ALTER TABLE messages ADD COLUMN parent_id INTEGER"},
		{"messages", "files", "ALTER TABLE messages ADD COLUMN files TEXT NOT NULL DEFAULT ''"},
		{"client_tokens", "last_sync", "ALTER TABLE client_tokens ADD COLUMN last_sync REAL NOT NULL DEFAULT 0"},
		{"messages", "blocks", "ALTER TABLE messages ADD COLUMN blocks TEXT NOT NULL DEFAULT ''"},
		{"conversations", "active_leaf", "ALTER TABLE conversations ADD COLUMN active_leaf INTEGER NOT NULL DEFAULT 0"},
		{"conversations", "share_id", "ALTER TABLE conversations ADD COLUMN share_id TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range cols {
		if hasColumn(c.table, c.name) {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			log.Printf("!! 迁移 %s.%s 失败: %v", c.table, c.name, err)
		}
	}
}

func openDB(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		log.Fatalf("建库目录失败: %v", err)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	var err error
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("建表失败: %v", err)
	}
}

// ---------------------------------------------------------------- 用户

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:16])
}

// userFor 找/建用户：优先 DB 里的 token，其次 CHAT_TOKEN，最后允许匿名（按 UA+IP 派生）。
func userFor(r *http.Request) (string, bool) {
	tok := tokenOf(r)
	if tok == "" {
		if !allowAnon {
			return "", false
		}
		sum := sha256.Sum256([]byte(r.Header.Get("User-Agent") + "|" + clientIP(r)))
		return "anon-" + hex.EncodeToString(sum[:6]), true
	}
	if uid, _, ok := resolveToken(tok); ok {
		_, _ = db.Exec(`UPDATE users SET last_seen=? WHERE id=?`, time.Now().Unix(), uid)
		return uid, true
	}
	id := hashToken(tok)
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO users(id, token, created_at, last_seen) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET last_seen=excluded.last_seen`, id, tok, now, now); err != nil {
		log.Printf("!! 写用户失败: %v", err)
	}
	return id, true
}

func tokenOf(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------- 落库

var (
	storeMu sync.Mutex
)

func ensureConversation(convID, userID, model string) {
	if convID == "" || userID == "" {
		return
	}
	now := float64(time.Now().UnixNano()) / 1e9
	storeMu.Lock()
	res, err := db.Exec(`INSERT INTO conversations(id,user_id,title,model,created_at,updated_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at`,
		convID, userID, "", model, now, now)
	storeMu.Unlock()
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			// SQLite 的 upsert 在仅更新时间时也算 affected，这里用插入是否新建来判断
			storeMu.Lock()
			var cnt int
			_ = db.QueryRow(`SELECT COUNT(*) FROM conv_changes WHERE user_id=? AND conv_id=? AND op='upsert'`, userID, convID).Scan(&cnt)
			storeMu.Unlock()
			if cnt == 0 {
				recordChange(userID, convID, "upsert")
			}
		}
	}
}

type storedMsg struct {
	Role     string            `json:"role"`
	Content  string            `json:"content"`
	Thinking string            `json:"thinking,omitempty"`
	Stop     string            `json:"stop_reason,omitempty"`
	Files    []storedFile      `json:"files,omitempty"`
	Blocks   []json.RawMessage `json:"blocks,omitempty"`
}

// ---------------------------------------------------------------- 路由

func main() {
	if apiKey == "" {
		log.Fatal("缺少 DS_API_KEY")
	}
	openDB(dbPath)
	migrateStore()
	initProfile()
	backfillTree()
	initAuth()
	initAdmin()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		var u, c, m int
		_ = db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&u)
		_ = db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&c)
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&m)
		writeJSON(w, 200, map[string]any{"ok": true, "users": u, "conversations": c, "messages": m})
	})
	mux.HandleFunc("/v1/auth/", handleAuth)
	mux.HandleFunc("/v1/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			handleDeleteAllConversations(w, r)
			return
		}
		handleConversations(w, r)
	})
	mux.HandleFunc("/v1/conversations/", handleConversationDetail)
	mux.HandleFunc("/v1/import/zip", handleZipImport)
	mux.HandleFunc("/v1/import/status", handleImportStatus)
	mux.HandleFunc("/v1/search", handleSearch)
	mux.HandleFunc("/v1/profile", handleProfile)
	mux.HandleFunc("/v1/profile/", handleProfile)
	mux.HandleFunc("/v1/import/chunk", handleImportChunk)
	mux.HandleFunc("/v1/import/finish", handleImportFinish)
	mux.HandleFunc("/v1/cache", handleCacheStream)
	mux.HandleFunc("/v1/files", handleFileUpload)
	mux.HandleFunc("/v1/files/", handleFileDownload)
	mux.HandleFunc("/admin/", handleAdmin)
	mux.HandleFunc("/", handleProxy)

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", bind, port),
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("chat-server listening on %s upstream=%s db=%s", srv.Addr, apiBase, dbPath)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, X-Conversation-Id, anthropic-beta")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// ---------------------------------------------------------------- 会话查询

// sanitizeHistory 清理历史消息：
//   - assistant 消息只保留 text / thinking 块（工具块会让上游流中途校验失败）
//   - 丢弃空的 assistant 消息：上游遇到 content 为空会直接断流（实测 14 字节即结束）
func sanitizeHistory(messages []any) ([]any, bool) {
	changed := false
	keptMessages := make([]any, 0, len(messages))
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok {
			keptMessages = append(keptMessages, item)
			continue
		}
		if m["role"] != "assistant" {
			keptMessages = append(keptMessages, m)
			continue
		}
		switch content := m["content"].(type) {
		case string:
			if strings.TrimSpace(content) == "" {
				changed = true
				continue
			}
		case []any:
			kept := make([]any, 0, len(content))
			for _, b := range content {
				bm, ok := b.(map[string]any)
				if !ok {
					changed = true
					continue
				}
				kind, _ := bm["type"].(string)
				if kind != "text" && kind != "thinking" {
					changed = true
					continue
				}
				kept = append(kept, bm)
			}
			if len(kept) == 0 {
				changed = true
				continue
			}
			if len(kept) != len(content) {
				changed = true
			}
			m["content"] = kept
		}
		keptMessages = append(keptMessages, m)
	}
	// 丢掉空 assistant 后可能出现连续两条 user，上游同样会拒绝：合并成一条
	merged := make([]any, 0, len(keptMessages))
	for _, item := range keptMessages {
		m, ok := item.(map[string]any)
		if !ok || len(merged) == 0 {
			merged = append(merged, item)
			continue
		}
		prev, ok := merged[len(merged)-1].(map[string]any)
		if !ok || prev["role"] != "user" || m["role"] != "user" {
			merged = append(merged, item)
			continue
		}
		prevText, _ := prev["content"].(string)
		curText, _ := m["content"].(string)
		if prevText == "" || curText == "" {
			merged = append(merged, item)
			continue
		}
		prev["content"] = prevText + "\n\n" + curText
		changed = true
	}
	return merged, changed
}

// handleDeleteAllConversations 删除当前用户的全部会话与消息（客户端「删除所有对话」用）
func handleDeleteAllConversations(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	ids := []string{}
	rows, _ := db.Query(`SELECT id FROM conversations WHERE user_id=?`, uid)
	if rows != nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	storeMu.Lock()
	_, _ = db.Exec(`DELETE FROM messages WHERE conv_id IN (SELECT id FROM conversations WHERE user_id=?)`, uid)
	res, err := db.Exec(`DELETE FROM conversations WHERE user_id=?`, uid)
	storeMu.Unlock()
	for _, id := range ids {
		recordChange(uid, id, "delete")
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	n, _ := res.RowsAffected()
	log.Printf("  清空会话 user=%s 删除 %d 个", short(uid), n)
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": n})
}

// listLimit 会话列表条数：?limit= 指定，默认全部（最多 5000）
func listLimit(r *http.Request) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			return n
		}
	}
	return 5000
}

func handleConversations(w http.ResponseWriter, r *http.Request) {
	tok := tokenOf(r)
	uid, device, ok := resolveToken(tok)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	// 同步模式：
	//   mode=incr（默认）只取上次成功同步之后的会话
	//   mode=full        取全部会话（覆盖本地用）
	//   mode=range       取 updated_at 落在 [from, to] 的会话（unix 秒）
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if r.URL.Query().Get("full") == "1" {
		mode = "full"
	}
	if mode != "full" && mode != "range" {
		mode = "incr"
	}

	var lastSync float64
	_ = db.QueryRow(`SELECT last_sync FROM client_tokens WHERE token=?`, tok).Scan(&lastSync)
	since := lastSync
	if mode == "full" {
		since = 0
	}
	from, to := 0.0, 0.0
	if mode == "range" {
		from = queryFloat(r, "from")
		to = queryFloat(r, "to")
		if to == 0 {
			to = float64(time.Now().Unix()) + 1
		}
		since = from
	}

	// 消息条数用「视图条数」（工具轮已合并）而不是库里行数，见 viewMessageCount
	query := `SELECT c.id, c.title, c.model, c.updated_at, c.tokens_in, c.tokens_out
		FROM conversations c WHERE c.user_id=? AND c.updated_at>?`
	args := []any{uid, since}
	if mode == "range" {
		query += ` AND c.updated_at<=?`
		args = append(args, to)
	}
	query += ` ORDER BY c.updated_at DESC LIMIT ?`
	args = append(args, listLimit(r))

	rows, err := db.Query(query, args...)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	// 先把结果读完再算消息条数：db 是单连接池（SetMaxOpenConns(1)），
	// 在 rows 未关闭时再发起查询会直接死锁 —— 整个服务会挂在这一条 sync 上。
	type convRow struct {
		id, title, model string
		updated          float64
		tin, tout        int
	}
	var collected []convRow
	for rows.Next() {
		var r convRow
		if err := rows.Scan(&r.id, &r.title, &r.model, &r.updated, &r.tin, &r.tout); err == nil {
			collected = append(collected, r)
		}
	}
	rows.Close()
	out := []map[string]any{}
	for _, r := range collected {
		out = append(out, map[string]any{
			"id": r.id, "title": r.title, "model": r.model,
			"updated_at": r.updated, "messages": viewMessageCount(r.id),
			"tokens_in": r.tin, "tokens_out": r.tout,
		})
	}
	// 变更日志：让客户端知道哪些会话被删了 / 新增了
	changes := changesSince(uid, since)

	// 记录本次同步时刻（用服务端时间，避免客户端时钟漂移）
	now := float64(time.Now().Unix())
	storeMu.Lock()
	_, _ = db.Exec(`UPDATE client_tokens SET last_sync=? WHERE token=?`, now, tok)
	storeMu.Unlock()
	_ = device

	writeJSON(w, 200, map[string]any{
		"conversations": out, "changes": changes,
		"since": since, "now": now, "full": since == 0,
		"mode": mode, "from": from, "to": to,
	})
}

func handleConversationDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/conversations/")
	if rest == "" {
		writeJSON(w, 400, map[string]any{"error": "missing id"})
		return
	}

	// /v1/conversations/{id}/active  → 切换分支
	// /v1/conversations/{id}/tree    → 整棵树（含所有历史版本）
	action := ""
	convID := rest
	if i := strings.Index(rest, "/"); i > 0 {
		convID, action = rest[:i], rest[i+1:]
	}

	var owner string
	if err := db.QueryRow(`SELECT user_id FROM conversations WHERE id=?`, convID).Scan(&owner); err != nil || owner != uid {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}

	switch {
	case action == "active" && r.Method == http.MethodPost:
		var in struct {
			MessageID int64 `json:"message_id"`
		}
		if !readJSON(r, &in) || in.MessageID == 0 {
			writeJSON(w, 400, map[string]any{"error": "需要 message_id"})
			return
		}
		if !switchLeaf(convID, in.MessageID) {
			writeJSON(w, 404, map[string]any{"error": "message not in this conversation"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "active": activePathView(convID)})
		return

	case action == "share":
		handleShare(w, r, uid, convID)
		return

	case action == "tree" && r.Method == http.MethodGet:
		// 数据获取：同时给出当前分支与完整树，客户端一次请求就够
		treeOut := viewsFrom(coalesceToolSteps(tree(convID)))
		writeJSON(w, 200, map[string]any{
			"id": convID, "messages": activePathView(convID), "tree": treeOut, "title": convTitle(convID),
		})
		return

	case action == "" && r.Method == http.MethodDelete:
		storeMu.Lock()
		_, _ = db.Exec(`DELETE FROM messages WHERE conv_id=?`, convID)
		_, _ = db.Exec(`DELETE FROM conversations WHERE id=?`, convID)
		recordChange(uid, convID, "delete")
		storeMu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": convID})
		return

	case action == "" && r.Method == http.MethodGet:
		if r.URL.Query().Get("export") == "1" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+convID+`.json"`)
		}
		// 数据获取：一次返回当前分支 + 完整树 + 标题
		treeOut := viewsFrom(coalesceToolSteps(tree(convID)))
		writeJSON(w, 200, map[string]any{
			"id": convID, "messages": activePathView(convID), "tree": treeOut, "title": convTitle(convID),
		})
		return
	}

	writeJSON(w, 404, map[string]any{"error": "not found"})
}

// queryFloat 读取查询参数里的数值（支持 unix 秒；空或非法返回 0）
func queryFloat(r *http.Request, key string) float64 {
	if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0
}

// convTitle 取会话标题
func convTitle(convID string) string {
	var title string
	_ = db.QueryRow(`SELECT COALESCE(title,'') FROM conversations WHERE id=?`, convID).Scan(&title)
	return title
}

// siblingIndex 一次性算出「父节点 → 兄弟列表」的序号映射，避免 nodeView
// 对每个节点各查一次库（每个节点一次 SQL，大会话上是明显的 N+1）。
// key 是 (parent_id, id)，value 是 (第几版, 共几版)。
func siblingIndex(convID string) map[[2]int64][2]int {
	rows, err := db.Query(`SELECT id, parent_id FROM messages WHERE conv_id=? ORDER BY id`, convID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	// 先按 parent 分组（NULL 用 0 代表，主键从 1 开始不会撞）
	groups := map[int64][]int64{}
	for rows.Next() {
		var id int64
		var parent sql.NullInt64
		if err := rows.Scan(&id, &parent); err != nil {
			continue
		}
		k := int64(0)
		if parent.Valid {
			k = parent.Int64
		}
		groups[k] = append(groups[k], id)
	}
	out := make(map[[2]int64][2]int, len(groups))
	for k, ids := range groups {
		for i, id := range ids {
			out[[2]int64{k, id}] = [2]int{i + 1, len(ids)}
		}
	}
	return out
}

// nodeView 把节点包成给客户端看的结构，带上「第 N 版 / 共 M 版」。
// 单个节点用；批量场景改用 nodeViewPre（避免逐节点查库）。
func nodeView(convID string, n node) map[string]any {
	idx, total := siblings(convID, n)
	return nodeViewWith(n, idx, total)
}

// nodeViewWith 纯内存构造视图，版本号由调用方给。
func nodeViewWith(n node, idx, total int) map[string]any {
	var parent any
	if n.ParentID.Valid {
		parent = n.ParentID.Int64
	}
	view := map[string]any{
		"id": n.ID, "parent_id": parent, "role": n.Role,
		"content": n.Content, "thinking": n.Thinking,
		"stop_reason": n.Stop, "created_at": n.Created,
		"version": idx, "versions": total,
	}
	if n.Blocks != "" {
		var blocks []json.RawMessage
		if err := json.Unmarshal([]byte(n.Blocks), &blocks); err == nil && len(blocks) > 0 {
			view["blocks"] = blocks
		}
	}
	if n.Files != "" {
		var files []storedFile
		if err := json.Unmarshal([]byte(n.Files), &files); err == nil && len(files) > 0 {
			view["files"] = files
		}
	}
	return view
}

// nodeViewPre 用预先算好的兄弟索引构造节点视图（批量场景，避免 N+1 查询）。
func nodeViewPre(convID string, n node, idx map[[2]int64][2]int) map[string]any {
	view := nodeViewWith(n, 0, 0)
	k := int64(0)
	if n.ParentID.Valid {
		k = n.ParentID.Int64
	}
	if v, ok := idx[[2]int64{k, n.ID}]; ok {
		view["version"], view["versions"] = v[0], v[1]
	}
	return view
}

// activePathView 当前分支的完整视图
func activePathView(convID string) []map[string]any {
	// 工具轮的空正文消息在出视图前合并（见 coalesceToolSteps 的注释），
	// 版本号按合并后的结构算，不能回库查
	return viewsFrom(coalesceToolSteps(activePath(convID)))
}

// ---------------------------------------------------------------- 上游代理

// 上游一律走 HTTP/1.1：HTTP/2 下偶发 stream reset 会表现为「思考完/输出到一半直接 EOF」，
// 也就是没有 message_delta 的断流；HTTP/1.1 的 SSE 更稳。
var upstreamClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		ForceAttemptHTTP2:   false,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		DisableCompression:  true,
	},
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}

	path := r.URL.Path

	// 语音接口已下线：TTS 按字符计费，长回复一次就是几十万字符，ASR 同样收费。
	// 前端入口已全部移除，这里再挡一层，避免旧客户端或直接调用继续产生费用。
	if strings.HasPrefix(path, "/v1/tools/") {
		writeJSON(w, 410, map[string]any{
			"error": "语音接口已下线（TTS/ASR 按用量计费，成本过高）",
		})
		return
	}

	isMessages := strings.HasSuffix(path, "/v1/messages")

	var body []byte
	if r.Method != http.MethodGet {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 64<<20))
	}

	// 落库：客户端每轮都会带全量历史，直接以最后一次为准
	convID := r.Header.Get("X-Conversation-Id")
	var reqObj map[string]any
	if isMessages && len(body) > 0 {
		_ = json.Unmarshal(body, &reqObj)
		// 客户端不传 max_tokens 时上游默认偏小，长回答会被截断；这里补一个够用的默认值
		if _, ok := reqObj["max_tokens"]; !ok {
			reqObj["max_tokens"] = defaultMaxTokens
			if patched, err := json.Marshal(reqObj); err == nil {
				body = patched
			}
		}
		model, _ := reqObj["model"].(string)
		ensureConversation(convID, uid, model)
		// 本轮是不是这段会话的第一轮：决定要不要注入跨会话上下文、要不要总结上一段对话
		isNewConv := countConvMessages(convID) == 0
		// 历史会话检索工具：开关开启时和其它工具一起下发（每一轮都给，不只新会话）
		toolInjected := ensureHistoryTool(uid, reqObj)
		// 沙箱脚本工具：与跨会话开关无关，属于通用能力
		if ensureRhaiTool(reqObj) {
			toolInjected = true
		}
		if toolInjected {
			if patched, err := json.Marshal(reqObj); err == nil {
				body = patched
			}
		}
		// 客户端把本轮附件元数据随请求带上（只用于落库，转发前删除）
		var attachMeta []storedFile
		if am, ok := reqObj["attachment_meta"].([]any); ok {
			for _, item := range am {
				if m, ok := item.(map[string]any); ok {
					f := storedFile{}
					if v, ok := m["id"].(string); ok {
						f.ID = v
					}
					if v, ok := m["name"].(string); ok {
						f.Name = v
					}
					if v, ok := m["mimeType"].(string); ok {
						f.MimeType = v
					}
					if v, ok := m["isImage"].(bool); ok {
						f.IsImage = v
					}
					if v, ok := m["size"].(float64); ok {
						f.Size = int64(v)
					}
					if f.ID != "" {
						attachMeta = append(attachMeta, f)
					}
				}
			}
			delete(reqObj, "attachment_meta")
			if patched, err := json.Marshal(reqObj); err == nil {
				body = patched
			}
		}
		if msgs := extractMessages(reqObj); len(msgs) > 0 {
			if len(attachMeta) > 0 {
				for i := len(msgs) - 1; i >= 0; i-- {
					if msgs[i].Role == "user" {
						msgs[i].Files = attachMeta
						break
					}
				}
			}
			syncPath(convID, msgs)
		}
		// latest_reminder 是官方支持的角色（格式 <date>,<weekday>,<location>,<platform>,<language>），
		// 原样透传，不要改写，否则影响上下文缓存命中。
		// 历史里残留的 server_tool_use / 搜索结果块会让上游在流中途校验失败并断开，
		// 发出去之前统一清掉，只保留 text 与 thinking。
		if raw, ok := reqObj["messages"].([]any); ok {
			if sanitized, changed := sanitizeHistory(raw); changed {
				reqObj["messages"] = sanitized
				if patched, err := json.Marshal(reqObj); err == nil {
					body = patched
				}
			}
		}
		// 跨会话共享：新会话第一轮把画像与上次对话摘要注入 system。
		// 放在历史清洗之后，注入块不会再被 sanitizeHistory 动到。
		if isNewConv {
			if injectSharedContext(uid, convID, reqObj) {
				if patched, err := json.Marshal(reqObj); err == nil {
					body = patched
				}
				log.Printf("  跨会话共享已注入 conv=%s", short(convID))
			}
			// 开新对话后（无论开关是否开启）总结上一段对话并并入画像
			autoSummarizePrevious(uid, convID)
		}

		// 撞上下文上限前先压缩早期对话（库里原文不动）
		if isMessages {
			var tools []string
			if tl, ok := reqObj["tools"].([]any); ok {
				for _, t := range tl {
					if m, ok := t.(map[string]any); ok {
						if n, ok := m["name"].(string); ok {
							tools = append(tools, n)
						}
					}
				}
			}
			msgsN := 0
			if ml, ok := reqObj["messages"].([]any); ok {
				msgsN = len(ml)
			}
			log.Printf("  请求摘要 conv=%s 消息=%d 工具=%v 含附件=%v 体积=%d",
				convID, msgsN, tools, strings.Contains(string(body), `"file_id"`), len(body))
		}
		if raw, ok := reqObj["messages"].([]any); ok {
			if compacted, changed := compactMessages(r.Context(), convID, raw); changed {
				reqObj["messages"] = compacted
				if patched, err := json.Marshal(reqObj); err == nil {
					body = patched
				}
			}
		}
	}

	target := apiBase + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "bad upstream request"})
		return
	}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		// Accept-Encoding 必须剔除：浏览器会带 br/gzip，透传后上游压缩响应，
		// 我们按文本解析 SSE 就什么都读不到（表现为「思考完直接断流」）。
		if lk == "host" || lk == "content-length" || lk == "x-api-key" ||
			lk == "authorization" || lk == "origin" || lk == "referer" ||
			lk == "accept-encoding" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// 统一用服务端的 Key（Anthropic 兼容接口的鉴权头）
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	ctx, cancel := context.WithTimeout(r.Context(), reqTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	if td := traceDirOf(); td != "" && isMessages && strings.Contains(string(body), `"file_id"`) {
		saveAttachTrace(td, "request", body)
	}
	// CHAT_TRACE_ALL=1 时把所有 messages 请求体也留存，便于逐字段比对
	if td := traceDirOf(); td != "" && isMessages && env("CHAT_TRACE_ALL", "") == "1" {
		saveAttachTrace(td, "all", body)
	}

	// 流式对话交给后台任务：客户端断开也继续生成，重连带 X-Resume 续传
	if isMessages && strings.Contains(string(body), `"stream":true`) {
		serveMessageJob(w, r, req, body, convID)
		return
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "upstream: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "transfer-encoding" || lk == "connection" || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if !isMessages || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// SSE：边转发边解析，拿到助手文本后落库
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var text, thinking strings.Builder
	var meta streamMeta
	var event string
	startedAt := time.Now()
	bytesOut := 0
	sawStop := false
	var tailLines []string
	blockAcc := newBlockCollector()

	// 上游有时长时间没有字节（搜索/思考之间），中间层和运营商会把空闲连接掐掉。
	// 这里每 2 秒没数据就补一条 SSE 注释，客户端解析器会忽略它。
	writeMu := &sync.Mutex{}
	lastWrite := time.Now()
	stopHeartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				writeMu.Lock()
				if time.Since(lastWrite) >= 2*time.Second {
					if _, err := io.WriteString(w, ": keep-alive\n\n"); err == nil {
						if flusher != nil {
							flusher.Flush()
						}
						lastWrite = time.Now()
					}
				}
				writeMu.Unlock()
			}
		}
	}()

	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			tailLines = append(tailLines, line)
			if len(tailLines) > 12 {
				tailLines = tailLines[1:]
			}
		}
		if strings.Contains(line, `"type":"error"`) && len(line) < 4000 {
			log.Printf("  上游错误帧 conv=%s %s", convID, line)
		}
		writeMu.Lock()
		lastWrite = time.Now()
		writeMu.Unlock()
		n, err := io.WriteString(w, line+"\n")
		bytesOut += n
		if err != nil {
			// 客户端断开（点了停止）：已收到的内容照样落库
			log.Printf("  客户端断开 conv=%s 已转发 %d 字节 用时 %.1fs",
				convID, bytesOut, time.Since(startedAt).Seconds())
			break
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if strings.Contains(payload, `"type":"message_stop"`) {
				sawStop = true
			}
			collect(payload, event, &text, &thinking, &meta, blockAcc)
		}
		if line == "" && flusher != nil {
			flusher.Flush()
		}
	}
	close(stopHeartbeat)
	if flusher != nil {
		flusher.Flush()
	}
	if thinking.Len() > 0 || text.Len() > 0 {
		appendAssistant(convID, text.String(), thinking.String(), meta, blockAcc.build())
	}
	// stop 为空说明上游没发 message_delta 就结束了，通常是被中途掐断
	if td := traceDirOf(); td != "" && isMessages && strings.Contains(string(body), `"file_id"`) {
		saveAttachTrace(td, "tail", []byte(strings.Join(tailLines, "\n")))
	}
	if !sawStop {
		// 上游没发 message_stop：真正的断流。留存请求体与尾部事件，便于逐字段比对
		reqCopy, _ := json.MarshalIndent(reqObj, "", "  ")
		name := "/tmp/chatdbg-fail-" + time.Now().Format("150405") + ".json"
		_ = os.WriteFile(name, append(reqCopy, []byte("\n\n--- tail ---\n"+strings.Join(tailLines, "\n"))...), 0o600)
		log.Printf("  断流（无 message_stop）conv=%s 正文=%d字 尾部已留存 %s", convID, text.Len(), name)
		log.Printf("  断流尾部:\n    %s", strings.Join(tailLines, "\n    "))
	}
	log.Printf("  chat user=%s conv=%s out=%d chars tokens=%d/%d stop=%q bytes=%d 用时 %.1fs",
		short(uid), convID, text.Len(), meta.tokensIn, meta.tokensOut, meta.stop,
		bytesOut, time.Since(startedAt).Seconds())
}

// streamMeta 是流里收集到的可落库信息（标题、用量、结束原因）。
type streamMeta struct {
	title     string
	tokensIn  int
	tokensOut int
	stop      string
}

// blockCollector 按 index 收集原始内容块，保证工具调用/搜索结果的展示过程不丢
type blockCollector struct {
	blocks map[int]map[string]any
}

func newBlockCollector() *blockCollector {
	return &blockCollector{blocks: map[int]map[string]any{}}
}

// build 返回按 index 排序的原始块数组
func (c *blockCollector) build() []json.RawMessage {
	if len(c.blocks) == 0 {
		return nil
	}
	idx := make([]int, 0, len(c.blocks))
	for i := range c.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]json.RawMessage, 0, len(idx))
	for _, i := range idx {
		if raw, err := json.Marshal(c.blocks[i]); err == nil {
			out = append(out, raw)
		}
	}
	return out
}

func collect(payload, event string, text, thinking *strings.Builder, meta *streamMeta, blocks *blockCollector) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Delta struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			Thinking   string `json:"thinking"`
			Title      string `json:"title"`
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return
	}
	switch ev.Type {
	case "content_block_start":
		var start struct {
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
		}
		if json.Unmarshal([]byte(payload), &start) == nil && start.ContentBlock != nil && blocks != nil {
			blocks.blocks[start.Index] = start.ContentBlock
		}
	case "content_block_delta":
		if ev.Delta.Text != "" {
			text.WriteString(ev.Delta.Text)
		}
		if ev.Delta.Thinking != "" {
			thinking.WriteString(ev.Delta.Thinking)
		}
		var delta struct {
			Index int            `json:"index"`
			Delta map[string]any `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &delta) == nil && blocks != nil {
			blk := blocks.blocks[delta.Index]
			if blk == nil {
				blk = map[string]any{"type": "text", "text": ""}
				blocks.blocks[delta.Index] = blk
			}
			if t, ok := delta.Delta["type"].(string); ok {
				switch t {
				case "text_delta":
					if v, ok := delta.Delta["text"].(string); ok {
						cur, _ := blk["text"].(string)
						blk["text"] = cur + v
					}
				case "thinking_delta":
					if v, ok := delta.Delta["thinking"].(string); ok {
						cur, _ := blk["thinking"].(string)
						blk["thinking"] = cur + v
					}
				case "signature_delta":
					if v, ok := delta.Delta["signature"].(string); ok {
						blk["signature"] = v
					}
				}
			}
		}
	case "message_start":
		if ev.Message.Usage.InputTokens > 0 {
			meta.tokensIn = ev.Message.Usage.InputTokens
		}
	case "message_delta":
		if ev.Delta.Title != "" {
			meta.title = ev.Delta.Title
		}
		if ev.Delta.StopReason != "" {
			meta.stop = ev.Delta.StopReason
		}
		if ev.Usage.OutputTokens > 0 {
			meta.tokensOut = ev.Usage.OutputTokens
		}
	}
}

// extractMessages 把请求里的 content（字符串或块数组）拍平成纯文本。
func extractMessages(reqObj map[string]any) []storedMsg {
	raw, _ := reqObj["messages"].([]any)
	out := make([]storedMsg, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		// latest_reminder 只是本轮上下文提示，不当作对话消息落库（否则两端会把它渲染成一条回复）
		if role != "user" && role != "assistant" {
			continue
		}
		out = append(out, storedMsg{Role: role, Content: flatten(m["content"])})
	}
	return out
}

func flatten(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, b := range t {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := bm["type"].(string)
			switch typ {
			case "text":
				s, _ := bm["text"].(string)
				sb.WriteString(s)
			case "thinking":
				s, _ := bm["thinking"].(string)
				sb.WriteString(s)
				// 图片/文档不写占位符：落库只保留文字，否则标题与消息里会出现「[附件]」前缀
			}
		}
		return sb.String()
	}
	return ""
}

func short(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// 生成随机 id（备用）
