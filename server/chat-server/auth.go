package main

// 账号体系：邮箱 + 密码（bcrypt），邮箱验证码通过 SMTP（PMail）发送。
//
//	POST /v1/auth/register  {email,password}      注册，发送验证码
//	POST /v1/auth/code      {email}               重发验证码
//	POST /v1/auth/verify    {email,code}          校验验证码 → 返回登录令牌
//	POST /v1/auth/login     {email,password}      密码登录 → 返回登录令牌
//	GET  /v1/auth/me                              当前用户

import (
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	smtpHost     = env("SMTP_HOST", "127.0.0.1")
	smtpPort     = envInt("SMTP_PORT", 465)
	smtpUser     = env("SMTP_USER", "")
	smtpPass     = env("SMTP_PASS", "")
	smtpFrom     = env("SMTP_FROM", "noreply@example.com")
	smtpTLS      = env("SMTP_TLS", "implicit") // implicit(465) | starttls(587) | none
	mailFromName = env("SMTP_FROM_NAME", "DeepSeek 自托管版")
)

func migrateAuth() {
	for _, col := range []struct{ name, ddl string }{
		{"email", "ALTER TABLE users ADD COLUMN email TEXT"},
		{"pass_hash", "ALTER TABLE users ADD COLUMN pass_hash TEXT"},
		{"verified", "ALTER TABLE users ADD COLUMN verified INTEGER NOT NULL DEFAULT 0"},
		{"verify_code", "ALTER TABLE users ADD COLUMN verify_code TEXT"},
		{"code_expires", "ALTER TABLE users ADD COLUMN code_expires INTEGER NOT NULL DEFAULT 0"},
	} {
		if !hasColumn("users", col.name) {
			if _, err := db.Exec(col.ddl); err != nil {
				log.Printf("!! 迁移 %s 失败: %v", col.name, err)
			}
		}
	}
	_, _ = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email)`)
}

func hasColumn(table, col string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if name == col {
			return true
		}
	}
	return false
}

func readJSON(r *http.Request, v any) bool {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(raw) == 0 {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newCode() string {
	n := make([]byte, 3)
	_, _ = rand.Read(n)
	v := int(n[0])<<16 | int(n[1])<<8 | int(n[2])
	return fmt.Sprintf("%06d", v%1000000)
}

func validEmail(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 6 || len(s) > 200 || strings.Count(s, "@") != 1 {
		return false
	}
	at := strings.Index(s, "@")
	dot := strings.LastIndex(s, ".")
	return dot > at+1 && dot < len(s)-1
}

// ---------------------------------------------------------------- 邮件

type mailer struct{}

func (mailer) send(to, subject, body string) error {
	if smtpUser == "" && smtpHost == "127.0.0.1" {
		return fmt.Errorf("SMTP 未配置（设置 SMTP_HOST/SMTP_PORT/SMTP_USER/SMTP_PASS）")
	}
	addr := net.JoinHostPort(smtpHost, strconv.Itoa(smtpPort))
	msg := buildMessage(smtpFrom, mailFromName, to, subject, body)

	switch smtpTLS {
	case "implicit":
		return sendImplicitTLS(addr, to, msg)
	case "none":
		// 明文（如 smtpdm.aliyun.com:80，内网/已有链路加密时用）
		return sendPlain(addr, to, msg)
	default:
		return sendStartTLS(addr, to, msg)
	}
}

func buildMessage(from, fromName, to, subject, body string) []byte {
	headers := []string{
		"From: " + fromName + " <" + from + ">",
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"Date: " + time.Now().Format(time.RFC1123Z),
	}
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body + "\r\n")
}

func authPlain() smtp.Auth {
	if smtpUser == "" {
		return nil
	}
	return smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
}

func sendImplicitTLS(addr, to string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: smtpHost, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, smtpHost)
	if err != nil {
		return err
	}
	defer c.Close()
	if a := authPlain(); a != nil {
		if err := c.Auth(a); err != nil {
			return err
		}
	}
	if err := c.Mail(smtpFrom); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func sendPlain(addr, to string, msg []byte) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if a := authPlain(); a != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(a); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(smtpFrom); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func sendStartTLS(addr, to string, msg []byte) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		cfg := &tls.Config{ServerName: smtpHost, MinVersion: tls.VersionTLS12}
		// 连本机/内网 IP 时证书一定对不上（证书签给域名），直接跳过校验
		if net.ParseIP(smtpHost) != nil || smtpHost == "localhost" {
			cfg.InsecureSkipVerify = true
		}
		if err := c.StartTLS(cfg); err != nil {
			return err
		}
	}
	if a := authPlain(); a != nil {
		if err := c.Auth(a); err != nil {
			return err
		}
	}
	if err := c.Mail(smtpFrom); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// ---------------------------------------------------------------- 处理

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func handleAuth(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/auth")
	switch {
	case path == "/register" && r.Method == http.MethodPost:
		authRegister(w, r)
	case path == "/code" && r.Method == http.MethodPost:
		authSendCode(w, r)
	case path == "/verify" && r.Method == http.MethodPost:
		authVerify(w, r)
	case path == "/login" && r.Method == http.MethodPost:
		authLogin(w, r)
	case path == "/me" && r.Method == http.MethodGet:
		authMe(w, r)
	case path == "/account" && r.Method == http.MethodGet:
		authAccount(w, r)
	case path == "/password" && r.Method == http.MethodPost:
		authPassword(w, r)
	case path == "/email" && r.Method == http.MethodPost:
		authEmail(w, r)
	case path == "/oidc/start" && r.Method == http.MethodGet:
		handleOIDCStart(w, r)
	case path == "/oidc/callback" && r.Method == http.MethodGet:
		handleOIDCCallback(w, r)
	default:
		writeErr(w, 404, "not found")
	}
}

func authRegister(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !readJSON(r, &in) {
		writeErr(w, 400, "bad json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !validEmail(email) {
		writeErr(w, 400, "邮箱格式不正确")
		return
	}
	if len(in.Password) < 8 {
		writeErr(w, 400, "密码至少 8 位")
		return
	}
	var exists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM users WHERE email=?`, email).Scan(&exists)
	if exists > 0 {
		writeErr(w, 409, "邮箱已注册")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "hash error")
		return
	}
	code := newCode()
	expires := time.Now().Add(15 * time.Minute).Unix()
	now := time.Now().Unix()
	// token 列有 UNIQUE 约束，未登录用户必须写 NULL（SQLite 允许多个 NULL，不允许两个空串）
	_, err = db.Exec(`INSERT INTO users(id, token, created_at, last_seen, email, pass_hash, verified, verify_code, code_expires)
		VALUES(?,NULL,?,?,?,?,0,?,?)`,
		"u-"+randomToken()[:16], now, now, email, string(hash), code, expires)
	if err != nil {
		writeErr(w, 500, "db error: "+err.Error())
		return
	}
	if err := (mailer{}).send(email, "DeepSeek 自托管版 · 注册验证码",
		fmt.Sprintf("你的验证码是 %s，15 分钟内有效。\r\n如果不是你本人在注册，请忽略这封邮件。", code)); err != nil {
		// 信件发不出去就别留半个账号
		_, _ = db.Exec(`DELETE FROM users WHERE email=? AND verified=0`, email)
		log.Printf("!! 发送邮件失败: %v", err)
		writeErr(w, 502, "验证码邮件发送失败："+err.Error())
		return
	}
	log.Printf("  register %s", email)
	writeJSON(w, 200, map[string]any{"ok": true, "message": "验证码已发送"})
}

func authSendCode(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string }
	if !readJSON(r, &in) {
		writeErr(w, 400, "bad json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	code := newCode()
	expires := time.Now().Add(15 * time.Minute).Unix()
	res, err := db.Exec(`UPDATE users SET verify_code=?, code_expires=? WHERE email=?`, code, expires, email)
	if err != nil {
		writeErr(w, 500, "db error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 404, "邮箱未注册")
		return
	}
	if err := (mailer{}).send(email, "DeepSeek 自托管版 · 验证码",
		fmt.Sprintf("你的验证码是 %s，15 分钟内有效。", code)); err != nil {
		writeErr(w, 502, "验证码邮件发送失败："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "验证码已发送"})
}

func authVerify(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Code, Device string }
	if !readJSON(r, &in) {
		writeErr(w, 400, "bad json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	var stored string
	var expires int64
	err := db.QueryRow(`SELECT COALESCE(verify_code,''), code_expires FROM users WHERE email=?`, email).
		Scan(&stored, &expires)
	if err != nil {
		writeErr(w, 404, "邮箱未注册")
		return
	}
	if stored == "" || stored != strings.TrimSpace(in.Code) {
		writeErr(w, 400, "验证码不正确")
		return
	}
	if time.Now().Unix() > expires {
		writeErr(w, 400, "验证码已过期")
		return
	}
	var uid string
	_ = db.QueryRow(`SELECT id FROM users WHERE email=?`, email).Scan(&uid)
	tok, err := issueClientToken(uid, in.Device)
	if err != nil {
		writeErr(w, 500, "发放令牌失败")
		return
	}
	_, _ = db.Exec(`UPDATE users SET verified=1, verify_code='', code_expires=0, last_seen=? WHERE email=?`,
		time.Now().Unix(), email)
	writeJSON(w, 200, map[string]any{"ok": true, "token": tok, "email": email, "device": normalizeDevice(in.Device)})
}

func authLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password, Device string }
	if !readJSON(r, &in) {
		writeErr(w, 400, "bad json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	var id, hash string
	var verified int
	err := db.QueryRow(`SELECT id, COALESCE(pass_hash,''), verified FROM users WHERE email=?`, email).
		Scan(&id, &hash, &verified)
	if err != nil || hash == "" {
		writeErr(w, 401, "邮箱或密码不正确")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		writeErr(w, 401, "邮箱或密码不正确")
		return
	}
	if verified == 0 {
		writeErr(w, 403, "邮箱未验证，请先完成验证码校验")
		return
	}
	tok, err := issueClientToken(id, in.Device)
	if err != nil {
		writeErr(w, 500, "发放令牌失败")
		return
	}
	_, _ = db.Exec(`UPDATE users SET last_seen=? WHERE id=?`, time.Now().Unix(), id)
	log.Printf("  login %s device=%s", email, normalizeDevice(in.Device))
	writeJSON(w, 200, map[string]any{"ok": true, "token": tok, "email": email, "device": normalizeDevice(in.Device)})
}

func authMe(w http.ResponseWriter, r *http.Request) {
	tok := tokenOf(r)
	if tok == "" {
		writeErr(w, 401, "missing token")
		return
	}
	uid, _, ok := resolveToken(tok)
	if !ok {
		writeErr(w, 401, "invalid token")
		return
	}
	var email string
	var verified int
	err := db.QueryRow(`SELECT COALESCE(email,''), verified FROM users WHERE id=?`, uid).Scan(&email, &verified)
	if err != nil {
		writeErr(w, 401, "invalid token")
		return
	}
	writeJSON(w, 200, map[string]any{"email": email, "verified": verified == 1})
}

// authAccount 返回账号信息与使用统计
func authAccount(w http.ResponseWriter, r *http.Request) {
	tok := tokenOf(r)
	if tok == "" {
		writeErr(w, 401, "missing token")
		return
	}
	id, _, ok := resolveToken(tok)
	if !ok {
		writeErr(w, 401, "invalid token")
		return
	}
	var email string
	var verified int64
	var createdAt float64
	if err := db.QueryRow(`SELECT COALESCE(email,''), verified, created_at FROM users WHERE id=?`, id).
		Scan(&email, &verified, &createdAt); err != nil {
		writeErr(w, 401, "invalid token")
		return
	}
	var convs, msgs int
	_ = db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE user_id=?`, id).Scan(&convs)
	_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv_id IN (SELECT id FROM conversations WHERE user_id=?)`, id).Scan(&msgs)
	writeJSON(w, 200, map[string]any{
		"email":         email,
		"verified":      verified == 1,
		"created_at":    int64(createdAt),
		"conversations": convs,
		"messages":      msgs,
		"has_password":  hasPassword(id),
	})
}

func hasPassword(userID string) bool {
	var h string
	_ = db.QueryRow(`SELECT COALESCE(pass_hash,'') FROM users WHERE id=?`, userID).Scan(&h)
	return h != ""
}

// authPassword 修改密码：必须验证旧密码（OIDC 用户首次设置旧密码可为空）
func authPassword(w http.ResponseWriter, r *http.Request) {
	tok := tokenOf(r)
	if tok == "" {
		writeErr(w, 401, "missing token")
		return
	}
	var in struct{ Old, New string }
	if !readJSON(r, &in) {
		writeErr(w, 400, "bad json")
		return
	}
	if len(in.New) < 8 {
		writeErr(w, 400, "新密码至少 8 位")
		return
	}
	id, _, ok := resolveToken(tok)
	if !ok {
		writeErr(w, 401, "invalid token")
		return
	}
	var hash string
	_ = db.QueryRow(`SELECT COALESCE(pass_hash,'') FROM users WHERE id=?`, id).Scan(&hash)
	if hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Old)) != nil {
		writeErr(w, 403, "当前密码不正确")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "hash error")
		return
	}
	if _, err := db.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, string(newHash), id); err != nil {
		writeErr(w, 500, "保存失败")
		return
	}
	// 只旋转当前设备类型的令牌，其它设备继续可用
	device := "web"
	if _, d, ok := resolveToken(tok); ok && d != "" {
		device = d
	}
	newTok, err := issueClientToken(id, device)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "token": newTok, "device": device})
}

// authEmail 修改邮箱（账号标识）：需要密码确认
func authEmail(w http.ResponseWriter, r *http.Request) {
	tok := tokenOf(r)
	if tok == "" {
		writeErr(w, 401, "missing token")
		return
	}
	var in struct{ Email, Password string }
	if !readJSON(r, &in) {
		writeErr(w, 400, "bad json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !validEmail(email) {
		writeErr(w, 400, "邮箱格式不正确")
		return
	}
	id, _, ok := resolveToken(tok)
	if !ok {
		writeErr(w, 401, "invalid token")
		return
	}
	var hash string
	_ = db.QueryRow(`SELECT COALESCE(pass_hash,'') FROM users WHERE id=?`, id).Scan(&hash)
	if hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		writeErr(w, 403, "密码不正确")
		return
	}
	var exists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM users WHERE email=? AND id<>?`, email, id).Scan(&exists)
	if exists > 0 {
		writeErr(w, 409, "该邮箱已被占用")
		return
	}
	if _, err := db.Exec(`UPDATE users SET email=? WHERE id=?`, email, id); err != nil {
		writeErr(w, 500, "db error: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "email": email})
}

// 启动时调用
func initAuth() {
	migrateAuth()
	if smtpUser == "" {
		log.Printf("!! 未配置 SMTP（%s），注册/验证码邮件会发送失败", os.Getenv("SMTP_USER"))
	} else {
		log.Printf("smtp: %s:%d tls=%s from=%s", smtpHost, smtpPort, smtpTLS, smtpFrom)
	}
}
