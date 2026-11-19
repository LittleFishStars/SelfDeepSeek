package main

// 后台管理：一个页面看全站用户、会话与 token 消耗。
//
//	GET  /admin/                          管理页面（内嵌，单文件）
//	GET  /admin/api/overview              仪表盘汇总 + 近 N 天趋势
//	GET  /admin/api/users                 用户列表（搜索/排序/分页）
//	GET  /admin/api/users/{id}            单个用户详情 + 他的会话
//	POST /admin/api/users/{id}/disable    禁用 / 恢复用户
//	DELETE /admin/api/users/{id}          删除用户及其全部数据
//	GET  /admin/api/conversations         全站会话列表
//	GET  /admin/api/conversations/{id}    会话正文（排查用）
//	DELETE /admin/api/conversations/{id}  删除会话
//	GET  /admin/api/usage                 用量明细（按天/用户/模型聚合）
//	GET  /admin/api/shares                已分享的会话（分享页管理）
//	DELETE /admin/api/shares/{convId}     取消分享并删除静态页
//
// 鉴权：请求头 Authorization: Bearer <CHAT_ADMIN_TOKEN>，与业务令牌完全隔离，
// 令牌没配置时整个后台关闭（503），不会退化成「人人可进」。

import (
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed admin.html
var adminHTML []byte

// 管理端令牌：留空 = 后台不开放
var adminToken = env("CHAT_ADMIN_TOKEN", "")

// usage_log 是「每一轮对话」的 token 流水。
// conversations.tokens_in/out 只是累计值，做不出趋势图与按天统计，所以单独记流水。
const adminSchema = `
CREATE TABLE IF NOT EXISTS usage_log (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     TEXT NOT NULL DEFAULT '',
  conv_id     TEXT NOT NULL DEFAULT '',
  model       TEXT NOT NULL DEFAULT '',
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  stop_reason TEXT NOT NULL DEFAULT '',
  source      TEXT NOT NULL DEFAULT 'live',
  created_at  REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_log(created_at);
CREATE INDEX IF NOT EXISTS idx_usage_user ON usage_log(user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_conv ON usage_log(conv_id);
`

// initAdmin 建表 + 补列 + 把历史累计值回填成流水（幂等，可反复启动）
func initAdmin() {
	if _, err := db.Exec(adminSchema); err != nil {
		log.Printf("!! 建 usage_log 失败: %v", err)
		return
	}
	if !hasColumn("users", "disabled") {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`); err != nil {
			log.Printf("!! 迁移 users.disabled 失败: %v", err)
		}
	}
	// 老数据只有会话级累计值，没有流水：按「一整个会话算一条」补进去，
	// 时间用会话最后更新时间，source 标 backfill 便于区分。
	res, err := db.Exec(`INSERT INTO usage_log(user_id, conv_id, model, tokens_in, tokens_out, source, created_at)
		SELECT c.user_id, c.id, COALESCE(c.model,''), c.tokens_in, c.tokens_out, 'backfill', c.updated_at
		FROM conversations c
		WHERE (c.tokens_in > 0 OR c.tokens_out > 0)
		  AND NOT EXISTS (SELECT 1 FROM usage_log u WHERE u.conv_id = c.id)`)
	if err != nil {
		log.Printf("!! 回填 usage_log 失败: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("已回填 %d 条历史用量流水", n)
	}
}

// recordUsage 记一轮 token 消耗。user_id / model 直接从 conversations 带出来，
// 省一次查询，也避免调用方传错人。调用方可能已持有 storeMu，这里不再加锁。
func recordUsage(convID string, in, out int, stop string) {
	if in == 0 && out == 0 {
		return
	}
	now := float64(time.Now().UnixNano()) / 1e9
	res, err := db.Exec(`INSERT INTO usage_log(user_id, conv_id, model, tokens_in, tokens_out, stop_reason, source, created_at)
		SELECT user_id, id, COALESCE(model,''), ?, ?, ?, 'live', ? FROM conversations WHERE id=?`,
		in, out, stop, now, convID)
	if err != nil {
		log.Printf("!! 写用量流水失败: %v", err)
		return
	}
	// 会话还没入库（极少见）：至少把用量留在流水里，不丢账
	if n, _ := res.RowsAffected(); n == 0 {
		_, _ = db.Exec(`INSERT INTO usage_log(user_id, conv_id, model, tokens_in, tokens_out, stop_reason, source, created_at)
			VALUES('',?,'',?,?,?,'live',?)`, convID, in, out, stop, now)
	}
}

// ---------------------------------------------------------------- 鉴权

func adminAuthorized(r *http.Request) bool {
	if adminToken == "" {
		return false
	}
	tok := tokenOf(r)
	if tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(adminToken)) == 1
}

// requireAdmin 校验管理员令牌；不通过时已经写好响应，返回 false
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if adminToken == "" {
		writeJSON(w, 503, map[string]any{"error": "后台未开启：服务端未配置 CHAT_ADMIN_TOKEN"})
		return false
	}
	if !adminAuthorized(r) {
		writeJSON(w, 401, map[string]any{"error": "令牌无效"})
		return false
	}
	return true
}

// ---------------------------------------------------------------- 路由

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin")

	// 页面本身不含数据，但也不能让搜索引擎/误入者看到结构；数据统一要令牌，
	// 页面打开后会弹输入框换取令牌。
	// 走真路径路由（/admin/users 等），所以非 api/ 的路径都回同一个页面，
	// 刷新和直接输入地址都能落到对应视图（前端自己读 pathname）。
	if !strings.HasPrefix(path, "/api/") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(adminHTML)
		return
	}

	if !strings.HasPrefix(path, "/api/") {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	if !requireAdmin(w, r) {
		return
	}
	api := strings.TrimPrefix(path, "/api/")

	switch {
	case api == "overview" && r.Method == http.MethodGet:
		adminOverview(w, r)
	case api == "users" && r.Method == http.MethodGet:
		adminUsers(w, r)
	case api == "conversations" && r.Method == http.MethodGet:
		adminConversations(w, r)
	case api == "usage" && r.Method == http.MethodGet:
		adminUsage(w, r)
	case api == "shares" && r.Method == http.MethodGet:
		adminShares(w, r)
	case strings.HasPrefix(api, "shares/"):
		adminShareItem(w, r, strings.TrimPrefix(api, "shares/"))
	case strings.HasPrefix(api, "users/"):
		adminUserItem(w, r, strings.TrimPrefix(api, "users/"))
	case strings.HasPrefix(api, "conversations/"):
		adminConvItem(w, r, strings.TrimPrefix(api, "conversations/"))
	default:
		writeJSON(w, 404, map[string]any{"error": "not found"})
	}
}

// ---------------------------------------------------------------- 查询参数

func queryInt(r *http.Request, key string, def int) int {
	if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// pageParams 统一分页：page 从 1 开始，size 限 1..200
func pageParams(r *http.Request) (limit, offset int) {
	page := queryInt(r, "page", 1)
	if page < 1 {
		page = 1
	}
	limit = queryInt(r, "size", 20)
	if limit < 1 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	return limit, (page - 1) * limit
}

func likeArg(q string) string { return "%" + q + "%" }

// ---------------------------------------------------------------- 仪表盘

func adminOverview(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 30)
	if days < 7 {
		days = 7
	}
	if days > 180 {
		days = 180
	}

	type stat struct {
		Total int64 `json:"total"`
		New   int64 `json:"new"`
	}
	get := func(sqlStr string, args ...any) int64 {
		var n int64
		_ = db.QueryRow(sqlStr, args...).Scan(&n)
		return n
	}
	now := time.Now()
	dayAgo := float64(now.Add(-24 * time.Hour).Unix())
	weekAgo := float64(now.Add(-7 * 24 * time.Hour).Unix())
	monthAgo := float64(now.Add(-30 * 24 * time.Hour).Unix())

	// 用户
	users := stat{
		Total: get(`SELECT COUNT(*) FROM users`),
		New:   get(`SELECT COUNT(*) FROM users WHERE created_at>?`, weekAgo),
	}
	verified := get(`SELECT COUNT(*) FROM users WHERE verified=1`)
	disabled := get(`SELECT COUNT(*) FROM users WHERE disabled=1`)
	active7d := get(`SELECT COUNT(DISTINCT user_id) FROM usage_log WHERE created_at>?`, weekAgo)

	// 会话与消息
	convs := stat{
		Total: get(`SELECT COUNT(*) FROM conversations`),
		New:   get(`SELECT COUNT(*) FROM conversations WHERE created_at>?`, dayAgo),
	}
	msgs := stat{
		Total: get(`SELECT COUNT(*) FROM messages`),
		New:   get(`SELECT COUNT(*) FROM messages WHERE created_at>?`, dayAgo),
	}

	// token：全量 / 今日 / 近 7 天
	var inAll, outAll, inDay, outDay, inWeek, outWeek int64
	_ = db.QueryRow(`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0) FROM usage_log`).Scan(&inAll, &outAll)
	_ = db.QueryRow(`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0) FROM usage_log WHERE created_at>?`, dayAgo).Scan(&inDay, &outDay)
	_ = db.QueryRow(`SELECT COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0) FROM usage_log WHERE created_at>?`, weekAgo).Scan(&inWeek, &outWeek)

	writeJSON(w, 200, map[string]any{
		"users": map[string]any{
			"total": users.Total, "new": users.New, "verified": verified,
			"disabled": disabled, "active7d": active7d,
		},
		"conversations": map[string]any{"total": convs.Total, "new": convs.New},
		"messages":      map[string]any{"total": msgs.Total, "new": msgs.New},
		"tokens": map[string]any{
			"in": inAll, "out": outAll, "total": inAll + outAll,
			"today_in": inDay, "today_out": outDay,
			"week_in": inWeek, "week_out": outWeek,
			// 近 30 天用户增长用于副标题
			"month_new": get(`SELECT COUNT(*) FROM users WHERE created_at>?`, monthAgo),
		},
		"series":     adminSeries(days),
		"top_users":  adminTopUsers(8),
		"models":     adminModelStats(),
		"recent":     adminRecentConvs(8),
		"server_now": now.Unix(),
	})
}

// adminSeries 按天返回趋势（缺失的日期补 0，前端画图不用处理空洞）
func adminSeries(days int) []map[string]any {
	type bucket struct{ in, out, msgs, convs int64 }
	data := map[string]*bucket{}

	for _, d := range daysRange(days) {
		data[d] = &bucket{}
	}

	// token 流水
	if rows, err := db.Query(`SELECT date(created_at,'unixepoch','localtime') d,
		COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		FROM usage_log WHERE created_at >= ? GROUP BY d`, daysStart(days)); err == nil {
		for rows.Next() {
			var d string
			var in, out int64
			if err := rows.Scan(&d, &in, &out); err == nil {
				if b, ok := data[d]; ok {
					b.in, b.out = in, out
				}
			}
		}
		rows.Close()
	}
	// 消息数
	if rows, err := db.Query(`SELECT date(created_at,'unixepoch','localtime') d, COUNT(*)
		FROM messages WHERE created_at >= ? GROUP BY d`, daysStart(days)); err == nil {
		for rows.Next() {
			var d string
			var n int64
			if err := rows.Scan(&d, &n); err == nil {
				if b, ok := data[d]; ok {
					b.msgs = n
				}
			}
		}
		rows.Close()
	}
	// 新建会话数
	if rows, err := db.Query(`SELECT date(created_at,'unixepoch','localtime') d, COUNT(*)
		FROM conversations WHERE created_at >= ? GROUP BY d`, daysStart(days)); err == nil {
		for rows.Next() {
			var d string
			var n int64
			if err := rows.Scan(&d, &n); err == nil {
				if b, ok := data[d]; ok {
					b.convs = n
				}
			}
		}
		rows.Close()
	}

	out := make([]map[string]any, 0, days)
	for _, d := range daysRange(days) {
		b := data[d]
		out = append(out, map[string]any{
			"date": d, "tokens_in": b.in, "tokens_out": b.out,
			"tokens": b.in + b.out, "messages": b.msgs, "conversations": b.convs,
		})
	}
	return out
}

// daysRange 生成从 (今天 - days + 1) 到今天的日期串（本地时区）
func daysRange(days int) []string {
	out := make([]string, 0, days)
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		out = append(out, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

// daysStart 返回 days 天前那一天的零点（本地时区）的 unix 秒
func daysStart(days int) float64 {
	now := time.Now()
	y, m, d := now.AddDate(0, 0, -(days - 1)).Date()
	return float64(time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix())
}

func adminTopUsers(limit int) []map[string]any {
	rows, err := db.Query(`SELECT u.id, COALESCE(u.email,''),
		COALESCE(SUM(l.tokens_in),0), COALESCE(SUM(l.tokens_out),0),
		COUNT(DISTINCT l.conv_id)
		FROM usage_log l LEFT JOIN users u ON u.id = l.user_id
		GROUP BY l.user_id ORDER BY SUM(l.tokens_in + l.tokens_out) DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var id, email string
		var tokIn, tokOut, convs int64
		if err := rows.Scan(&id, &email, &tokIn, &tokOut, &convs); err == nil {
			out = append(out, map[string]any{
				"id": id, "email": email, "tokens_in": tokIn, "tokens_out": tokOut,
				"tokens": tokIn + tokOut, "conversations": convs,
			})
		}
	}
	return out
}

func adminModelStats() []map[string]any {
	rows, err := db.Query(`SELECT COALESCE(NULLIF(model,''),'未知') m, COUNT(*),
		COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		FROM conversations GROUP BY m ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 8)
	for rows.Next() {
		var m string
		var n, tokIn, tokOut int64
		if err := rows.Scan(&m, &n, &tokIn, &tokOut); err == nil {
			out = append(out, map[string]any{
				"model": m, "conversations": n, "tokens_in": tokIn, "tokens_out": tokOut,
			})
		}
	}
	return out
}

func adminRecentConvs(limit int) []map[string]any {
	rows, err := db.Query(`SELECT c.id, COALESCE(c.title,''), COALESCE(u.email,''), c.user_id,
		COALESCE(c.model,''), c.updated_at, c.tokens_in, c.tokens_out,
		(SELECT COUNT(*) FROM messages m WHERE m.conv_id=c.id)
		FROM conversations c LEFT JOIN users u ON u.id=c.user_id
		ORDER BY c.updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var id, title, email, uid, model string
		var updated float64
		var in, outN, msgs int64
		if err := rows.Scan(&id, &title, &email, &uid, &model, &updated, &in, &outN, &msgs); err == nil {
			out = append(out, map[string]any{
				"id": id, "title": title, "email": email, "user_id": uid, "model": model,
				"updated_at": updated, "tokens_in": in, "tokens_out": outN, "messages": msgs,
			})
		}
	}
	return out
}

// ---------------------------------------------------------------- 用户

func adminUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sortKey := r.URL.Query().Get("sort")

	// 排序只允许白名单，避免拼接注入
	order := "u.last_seen DESC"
	switch sortKey {
	case "tokens":
		// 按「合计」排序；页面展示的也是合计，单看 tokens_out 会显得乱序
		order = "(tokens_in + tokens_out) DESC"
	case "convs":
		order = "conversations DESC"
	case "created":
		order = "u.created_at DESC"
	case "seen":
		order = "u.last_seen DESC"
	}

	where := `WHERE (?='' OR u.email LIKE ? OR u.id LIKE ?)`
	args := []any{q, likeArg(q), likeArg(q)}

	var total int64
	_ = db.QueryRow(`SELECT COUNT(*) FROM users u `+where, args...).Scan(&total)

	// 每行的会话数/消息数/用量都用子查询聚合，数据量在个人部署下完全够用
	sqlStr := `
SELECT u.id, COALESCE(u.email,''), COALESCE(u.verified,0), COALESCE(u.disabled,0),
       u.created_at, u.last_seen,
       (SELECT COUNT(*) FROM conversations c WHERE c.user_id=u.id) conversations,
       (SELECT COUNT(*) FROM messages m JOIN conversations c ON m.conv_id=c.id WHERE c.user_id=u.id) messages,
       (SELECT COALESCE(SUM(c.tokens_in),0) FROM conversations c WHERE c.user_id=u.id) tokens_in,
       (SELECT COALESCE(SUM(c.tokens_out),0) FROM conversations c WHERE c.user_id=u.id) tokens_out,
       (SELECT GROUP_CONCAT(device) FROM client_tokens t WHERE t.user_id=u.id) devices
FROM users u ` + where + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "查询失败: " + err.Error()})
		return
	}
	defer rows.Close()

	items := make([]map[string]any, 0, limit)
	for rows.Next() {
		var id, email string
		var verified, disabled int
		var created, seen float64
		var convs, msgs, in, out int64
		var devices sql.NullString
		if err := rows.Scan(&id, &email, &verified, &disabled, &created, &seen,
			&convs, &msgs, &in, &out, &devices); err != nil {
			continue
		}
		dev := []string{}
		if devices.Valid && devices.String != "" {
			for _, d := range strings.Split(devices.String, ",") {
				if d != "" {
					dev = append(dev, d)
				}
			}
		}
		items = append(items, map[string]any{
			"id": id, "email": email, "verified": verified == 1, "disabled": disabled == 1,
			"created_at": created, "last_seen": seen,
			"conversations": convs, "messages": msgs,
			"tokens_in": in, "tokens_out": out, "tokens": in + out,
			"devices": dev,
		})
	}
	writeJSON(w, 200, map[string]any{"total": total, "items": items, "page": offset/limit + 1, "size": limit})
}

// adminUserItem 处理 /admin/api/users/{id} 与 /users/{id}/disable
func adminUserItem(w http.ResponseWriter, r *http.Request, rest string) {
	id, action := rest, ""
	if i := strings.Index(rest, "/"); i > 0 {
		id, action = rest[:i], rest[i+1:]
	}

	// 禁用/恢复
	if action == "disable" && r.Method == http.MethodPost {
		var in struct {
			Disabled bool `json:"disabled"`
		}
		if !readJSON(r, &in) {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		flag := 0
		if in.Disabled {
			flag = 1
		}
		res, err := db.Exec(`UPDATE users SET disabled=? WHERE id=?`, flag, id)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, 404, map[string]any{"error": "用户不存在"})
			return
		}
		log.Printf("  管理端：用户 %s disabled=%d", short(id), flag)
		writeJSON(w, 200, map[string]any{"ok": true, "disabled": in.Disabled})
		return
	}

	if action != "" {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		var email string
		var verified, disabled int
		var created, seen float64
		err := db.QueryRow(`SELECT COALESCE(email,''), COALESCE(verified,0), COALESCE(disabled,0), created_at, last_seen
			FROM users WHERE id=?`, id).Scan(&email, &verified, &disabled, &created, &seen)
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": "用户不存在"})
			return
		}
		// 跨会话共享那部分：画像内容 + 更新时间 + 开关状态，管理端排查用
		profile, profileAt := profileOf(id)
		writeJSON(w, 200, map[string]any{
			"id": id, "email": email, "verified": verified == 1, "disabled": disabled == 1,
			"created_at": created, "last_seen": seen,
			"has_password":    hasPassword(id),
			"share_context":   shareContextEnabled(id),
			"profile":         profile,
			"profile_updated": profileAt,
			"conversations":   adminConvsOfUser(id, 100),
		})
	case http.MethodDelete:
		// 级联删干净：会话、消息、令牌、流水、变更日志、离线缓存
		storeMu.Lock()
		_, _ = db.Exec(`DELETE FROM messages WHERE conv_id IN (SELECT id FROM conversations WHERE user_id=?)`, id)
		_, _ = db.Exec(`DELETE FROM conversations WHERE user_id=?`, id)
		_, _ = db.Exec(`DELETE FROM usage_log WHERE user_id=?`, id)
		_, _ = db.Exec(`DELETE FROM client_tokens WHERE user_id=?`, id)
		_, _ = db.Exec(`DELETE FROM conv_changes WHERE user_id=?`, id)
		_, _ = db.Exec(`DELETE FROM reply_cache WHERE user_id=?`, id)
		_, _ = db.Exec(`DELETE FROM user_profile WHERE user_id=?`, id)
		_, _ = db.Exec(`DELETE FROM conv_summary WHERE user_id=?`, id)
		_, _ = db.Exec(`DELETE FROM user_settings WHERE user_id=?`, id)
		res, err := db.Exec(`DELETE FROM users WHERE id=?`, id)
		storeMu.Unlock()
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, 404, map[string]any{"error": "用户不存在"})
			return
		}
		log.Printf("  管理端：删除用户 %s", short(id))
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}
}

func adminConvsOfUser(uid string, limit int) []map[string]any {
	rows, err := db.Query(`SELECT c.id, COALESCE(c.title,''), COALESCE(c.model,''),
		c.created_at, c.updated_at, c.tokens_in, c.tokens_out,
		(SELECT COUNT(*) FROM messages m WHERE m.conv_id=c.id)
		FROM conversations c WHERE c.user_id=? ORDER BY c.updated_at DESC LIMIT ?`, uid, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		var id, title, model string
		var created, updated float64
		var in, outN, msgs int64
		if err := rows.Scan(&id, &title, &model, &created, &updated, &in, &outN, &msgs); err == nil {
			out = append(out, map[string]any{
				"id": id, "title": title, "model": model,
				"created_at": created, "updated_at": updated,
				"tokens_in": in, "tokens_out": outN, "messages": msgs,
			})
		}
	}
	return out
}

// ---------------------------------------------------------------- 会话

func adminConversations(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	uid := strings.TrimSpace(r.URL.Query().Get("user"))

	where := `WHERE (?='' OR c.title LIKE ? OR c.id LIKE ?) AND (?='' OR c.user_id=?)`
	args := []any{q, likeArg(q), likeArg(q), uid, uid}

	var total int64
	_ = db.QueryRow(`SELECT COUNT(*) FROM conversations c `+where, args...).Scan(&total)

	sqlStr := `SELECT c.id, COALESCE(c.title,''), c.user_id, COALESCE(u.email,''),
		COALESCE(c.model,''), c.created_at, c.updated_at, c.tokens_in, c.tokens_out,
		(SELECT COUNT(*) FROM messages m WHERE m.conv_id=c.id) messages,
		(SELECT COALESCE(SUM(LENGTH(m.content)),0) FROM messages m WHERE m.conv_id=c.id) chars
		FROM conversations c LEFT JOIN users u ON u.id=c.user_id ` + where + `
		ORDER BY c.updated_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "查询失败: " + err.Error()})
		return
	}
	defer rows.Close()

	items := make([]map[string]any, 0, limit)
	for rows.Next() {
		var id, title, uid2, email, model string
		var created, updated float64
		var in, outN, msgs, chars int64
		if err := rows.Scan(&id, &title, &uid2, &email, &model, &created, &updated,
			&in, &outN, &msgs, &chars); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"id": id, "title": title, "user_id": uid2, "email": email, "model": model,
			"created_at": created, "updated_at": updated,
			"tokens_in": in, "tokens_out": outN, "tokens": in + outN,
			"messages": msgs, "chars": chars,
		})
	}
	writeJSON(w, 200, map[string]any{"total": total, "items": items, "page": offset/limit + 1, "size": limit})
}

func adminConvItem(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		var title, uid, email, model string
		var created, updated float64
		var in, outN int64
		err := db.QueryRow(`SELECT COALESCE(c.title,''), c.user_id, COALESCE(u.email,''),
			COALESCE(c.model,''), c.created_at, c.updated_at, c.tokens_in, c.tokens_out
			FROM conversations c LEFT JOIN users u ON u.id=c.user_id WHERE c.id=?`, id).
			Scan(&title, &uid, &email, &model, &created, &updated, &in, &outN)
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": "会话不存在"})
			return
		}
		// 走完整树而不是扁平列表：重新生成会产生分支，只按 id 顺序铺开会让
		// 多条版本混在一条时间线上，看不出谁是谁的替代（管理员排查时常被误导）。
		// nodeView 已带 parent_id / version / versions，前端据此画分支切换。
		allNodes := tree(id)
		// 兄弟序号一次算好，避免每个节点各查一次库（大会话上百节点时很明显）
		idx := siblingIndex(id)
		treeOut := make([]map[string]any, 0, len(allNodes))
		// 正文只读不写；单条截断到 4000 字，够排查又不至于把整库拖进浏览器
		const maxRunes = 4000
		for _, n := range allNodes {
			v := nodeViewPre(id, n, idx)
			truncated := false
			rs := []rune(n.Content)
			if len(rs) > maxRunes {
				v["content"] = string(rs[:maxRunes]) + "…"
				truncated = true
			}
			v["truncated"] = truncated
			treeOut = append(treeOut, v)
		}
		// 当前分支（根 → active_leaf）单独给一份，前端默认只看这条，
		// 点「第 N 版」再展开其它分支；messages 字段保留给老的调用方。
		activeNodes := activePath(id)
		msgs := make([]map[string]any, 0, len(activeNodes))
		for _, n := range activeNodes {
			m := nodeViewPre(id, n, idx)
			truncated := false
			if c, ok := m["content"].(string); ok {
				rs := []rune(c)
				if len(rs) > maxRunes {
					m["content"] = string(rs[:maxRunes]) + "…"
					truncated = true
				}
			}
			m["truncated"] = truncated
			msgs = append(msgs, m)
		}
		writeJSON(w, 200, map[string]any{
			"id": id, "title": title, "user_id": uid, "email": email, "model": model,
			"created_at": created, "updated_at": updated,
			"tokens_in": in, "tokens_out": outN,
			"messages": msgs, "tree": treeOut,
		})
	case http.MethodDelete:
		// 先取 owner：删完就查不到了，tombstone 要按原 owner 记
		var uid string
		_ = db.QueryRow(`SELECT user_id FROM conversations WHERE id=?`, id).Scan(&uid)
		res, err := db.Exec(`DELETE FROM conversations WHERE id=?`, id)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeJSON(w, 404, map[string]any{"error": "会话不存在"})
			return
		}
		storeMu.Lock()
		_, _ = db.Exec(`DELETE FROM messages WHERE conv_id=?`, id)
		_, _ = db.Exec(`DELETE FROM reply_cache WHERE conv_id=?`, id)
		storeMu.Unlock()
		// 通知客户端同步到「已删除」
		recordChange(uid, id, "delete")
		log.Printf("  管理端：删除会话 %s", short(id))
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}
}

// ---------------------------------------------------------------- 用量

func adminUsage(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 30)
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}
	start := daysStart(days)

	byDay := make([]map[string]any, 0, days)
	if rows, err := db.Query(`SELECT date(created_at,'unixepoch','localtime') d,
		COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COUNT(*)
		FROM usage_log WHERE created_at>=? GROUP BY d ORDER BY d`, start); err == nil {
		m := map[string]map[string]any{}
		for rows.Next() {
			var d string
			var in, out, n int64
			if err := rows.Scan(&d, &in, &out, &n); err == nil {
				m[d] = map[string]any{"date": d, "tokens_in": in, "tokens_out": out,
					"tokens": in + out, "turns": n}
			}
		}
		rows.Close()
		for _, d := range daysRange(days) {
			if v, ok := m[d]; ok {
				byDay = append(byDay, v)
			} else {
				byDay = append(byDay, map[string]any{"date": d, "tokens_in": 0, "tokens_out": 0, "tokens": 0, "turns": 0})
			}
		}
	}

	// 按用户
	byUser := make([]map[string]any, 0, 32)
	if rows, err := db.Query(`SELECT l.user_id, COALESCE(u.email,''),
		COALESCE(SUM(l.tokens_in),0), COALESCE(SUM(l.tokens_out),0), COUNT(*),
		COUNT(DISTINCT l.conv_id)
		FROM usage_log l LEFT JOIN users u ON u.id=l.user_id
		WHERE l.created_at>=? GROUP BY l.user_id
		ORDER BY SUM(l.tokens_in+l.tokens_out) DESC LIMIT 50`, start); err == nil {
		for rows.Next() {
			var uid, email string
			var in, out, turns, convs int64
			if err := rows.Scan(&uid, &email, &in, &out, &turns, &convs); err == nil {
				byUser = append(byUser, map[string]any{
					"id": uid, "email": email, "tokens_in": in, "tokens_out": out,
					"tokens": in + out, "turns": turns, "conversations": convs,
				})
			}
		}
		rows.Close()
	}

	// 按模型
	byModel := make([]map[string]any, 0, 8)
	if rows, err := db.Query(`SELECT COALESCE(NULLIF(model,''),'未知'),
		COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COUNT(*)
		FROM usage_log WHERE created_at>=? GROUP BY 1 ORDER BY SUM(tokens_in+tokens_out) DESC`, start); err == nil {
		for rows.Next() {
			var m string
			var in, out, turns int64
			if err := rows.Scan(&m, &in, &out, &turns); err == nil {
				byModel = append(byModel, map[string]any{
					"model": m, "tokens_in": in, "tokens_out": out,
					"tokens": in + out, "turns": turns,
				})
			}
		}
		rows.Close()
	}

	// 最近 50 次调用明细
	recent := make([]map[string]any, 0, 50)
	if rows, err := db.Query(`SELECT l.id, l.user_id, COALESCE(u.email,''), l.conv_id,
		COALESCE(NULLIF(l.model,''),'-'), l.tokens_in, l.tokens_out, l.stop_reason, l.created_at
		FROM usage_log l LEFT JOIN users u ON u.id=l.user_id
		ORDER BY l.created_at DESC LIMIT 50`); err == nil {
		for rows.Next() {
			var id int64
			var uid, email, conv, model, stop string
			var in, out int64
			var ts float64
			if err := rows.Scan(&id, &uid, &email, &conv, &model, &in, &out, &stop, &ts); err == nil {
				recent = append(recent, map[string]any{
					"id": id, "user_id": uid, "email": email, "conv_id": conv,
					"model": model, "tokens_in": in, "tokens_out": out,
					"stop_reason": stop, "created_at": ts,
				})
			}
		}
		rows.Close()
	}

	writeJSON(w, 200, map[string]any{
		"days": days, "by_day": byDay, "by_user": byUser,
		"by_model": byModel, "recent": recent,
	})
}

// ---------------------------------------------------------------- 分享页管理

// adminShares 列出所有已分享的会话（分享页是静态文件，这里顺带报文件大小与是否还在）
func adminShares(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE COALESCE(share_id,'')<>''`).Scan(&total); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	rows, err := db.Query(`SELECT c.id, COALESCE(c.title,''), c.share_id, c.updated_at,
		COALESCE(u.email,''), c.user_id
		FROM conversations c LEFT JOIN users u ON u.id=c.user_id
		WHERE COALESCE(c.share_id,'')<>'' ORDER BY c.updated_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	// 单连接池：先读完再 stat 文件，别在 rows 里发别的查询
	type shareRow struct {
		id, title, shareID string
		updated            float64
		email, userID      string
	}
	var collected []shareRow
	for rows.Next() {
		var it shareRow
		if err := rows.Scan(&it.id, &it.title, &it.shareID, &it.updated, &it.email, &it.userID); err == nil {
			collected = append(collected, it)
		}
	}
	rows.Close()

	items := make([]map[string]any, 0, len(collected))
	for _, it := range collected {
		path := filepath.Join(shareDir(), it.shareID+".html")
		size := int64(0)
		exists := true
		if st, err := os.Stat(path); err == nil {
			size = st.Size()
		} else {
			exists = false
		}
		items = append(items, map[string]any{
			"conv_id": it.id, "title": it.title, "share_id": it.shareID,
			"url": shareBaseURL + it.shareID, "updated_at": it.updated,
			"email": it.email, "user_id": it.userID,
			"bytes": size, "exists": exists,
		})
	}
	writeJSON(w, 200, map[string]any{"total": total, "items": items, "page": offset/limit + 1, "size": limit})
}

// adminShareItem 取消分享：删静态页 + 清 share_id（两者都做，避免列表里留死链接）
func adminShareItem(w http.ResponseWriter, r *http.Request, convID string) {
	if r.Method != http.MethodDelete {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	var shareID string
	_ = db.QueryRow(`SELECT COALESCE(share_id,'') FROM conversations WHERE id=?`, convID).Scan(&shareID)
	if shareID != "" {
		_ = os.Remove(filepath.Join(shareDir(), shareID+".html"))
	}
	storeMu.Lock()
	_, _ = db.Exec(`UPDATE conversations SET share_id='' WHERE id=?`, convID)
	storeMu.Unlock()
	log.Printf("  后台取消分享 conv=%s id=%s", short(convID), shareID)
	writeJSON(w, 200, map[string]any{"ok": true})
}
