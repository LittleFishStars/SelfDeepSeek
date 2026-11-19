package main

// 用 TinyAuth 当 OIDC 身份提供方：
//
//	GET /v1/auth/oidc/start?app_origin=http://127.0.0.1:12345
//	    → 302 到 https://auth.example.com/authorize?...
//	GET /v1/auth/oidc/callback?code=&state=
//	    → 换 token、取 userinfo、建/找用户、发本服务的登录令牌
//	    → 302 回 <app_origin>/login.html#token=<token>
//
// 第三方登录（GitHub/Google/…）都配在 TinyAuth 里，本服务只认 OIDC，
// 以后加登录方式不用改这边的代码。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	oidcIssuer   = env("TINYAUTH_ISSUER", "https://auth.example.com")
	oidcClientID = env("OIDC_CLIENT_ID", "chat")
	oidcSecret   = env("OIDC_CLIENT_SECRET", "")
	oidcRedirect = env("OIDC_REDIRECT", "https://api.example.com/auth/v1/auth/oidc/callback")
	oidcScope    = env("OIDC_SCOPE", "openid profile email")
)

type oidcState struct {
	origin  string
	created time.Time
}

var (
	stateMu sync.Mutex
	states  = map[string]oidcState{}
)

func putState(origin string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	stateMu.Lock()
	defer stateMu.Unlock()
	// 顺手清理超过 10 分钟的
	for k, v := range states {
		if time.Since(v.created) > 10*time.Minute {
			delete(states, k)
		}
	}
	states[s] = oidcState{origin: origin, created: time.Now()}
	return s
}

func takeState(s string) (string, bool) {
	stateMu.Lock()
	defer stateMu.Unlock()
	v, ok := states[s]
	if ok {
		delete(states, s)
	}
	return v.origin, ok
}

func handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimRight(r.URL.Query().Get("app_origin"), "/")
	if origin == "" {
		writeErr(w, 400, "缺少 app_origin")
		return
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeErr(w, 400, "app_origin 不合法")
		return
	}
	state := putState(origin)
	q := url.Values{}
	q.Set("client_id", oidcClientID)
	q.Set("redirect_uri", oidcRedirect)
	q.Set("response_type", "code")
	q.Set("scope", oidcScope)
	q.Set("state", state)
	http.Redirect(w, r, strings.TrimRight(oidcIssuer, "/")+"/authorize?"+q.Encode(), http.StatusFound)
}

func handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if e := r.URL.Query().Get("error"); e != "" {
		writeOIDCError(w, "登录失败："+e)
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	origin, ok := takeState(state)
	if !ok {
		writeOIDCError(w, "登录会话已过期，请回到 App 重新登录")
		return
	}
	if code == "" {
		writeOIDCError(w, "回调缺少 code")
		return
	}

	// 1) 换 token
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oidcRedirect)
	form.Set("client_id", oidcClientID)
	form.Set("client_secret", oidcSecret)
	req, _ := http.NewRequest("POST", strings.TrimRight(oidcIssuer, "/")+"/api/oidc/token",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		writeOIDCError(w, "无法连接登录服务："+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		log.Printf("!! token 交换失败 %d: %s", resp.StatusCode, string(body))
		writeOIDCError(w, fmt.Sprintf("登录服务拒绝（HTTP %d）", resp.StatusCode))
		return
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		writeOIDCError(w, "登录服务返回的令牌无法解析")
		return
	}

	// 2) 取 userinfo
	ureq, _ := http.NewRequest("GET", strings.TrimRight(oidcIssuer, "/")+"/api/oidc/userinfo", nil)
	ureq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp, err := (&http.Client{Timeout: 20 * time.Second}).Do(ureq)
	if err != nil {
		writeOIDCError(w, "无法获取用户信息："+err.Error())
		return
	}
	defer uresp.Body.Close()
	ubody, _ := io.ReadAll(io.LimitReader(uresp.Body, 1<<20))
	if uresp.StatusCode != 200 {
		log.Printf("!! userinfo 失败 %d: %s", uresp.StatusCode, string(ubody))
		writeOIDCError(w, "登录服务没有返回用户信息")
		return
	}
	var info struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		Picture           string `json:"picture"`
	}
	if err := json.Unmarshal(ubody, &info); err != nil || info.Sub == "" {
		writeOIDCError(w, "用户信息解析失败")
		return
	}

	// 3) 建/找用户并发令牌
	email := strings.ToLower(strings.TrimSpace(info.Email))
	if email == "" {
		email = strings.ToLower(info.PreferredUsername) + "@oidc.local"
	}
	now := time.Now().Unix()
	_, err = db.Exec(`INSERT INTO users(id, created_at, last_seen, email, verified)
		VALUES(?,?,?,?,1)
		ON CONFLICT(email) DO UPDATE SET last_seen=excluded.last_seen, verified=1`,
		"o-"+hashToken(info.Sub)[:16], now, now, email)
	if err != nil {
		log.Printf("!! OIDC 建用户失败: %v", err)
		writeOIDCError(w, "创建账户失败")
		return
	}
	var uid string
	if err := db.QueryRow(`SELECT id FROM users WHERE email=?`, email).Scan(&uid); err != nil {
		writeOIDCError(w, "读取账户失败")
		return
	}
	token, err := issueClientToken(uid, r.URL.Query().Get("device"))
	if err != nil {
		writeOIDCError(w, "发放令牌失败")
		return
	}
	log.Printf("  oidc login sub=%s email=%s", short(info.Sub), email)

	// 4) 带令牌回 App 页面
	target := origin + "/login.html#token=" + url.QueryEscape(token) + "&email=" + url.QueryEscape(email)
	http.Redirect(w, r, target, http.StatusFound)
}

// writeOIDCError 返回一个简单的结果页，App 里直接看到原因
func writeOIDCError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(400)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>登录失败</title>
<style>body{font:15px/1.6 -apple-system,"PingFang SC",system-ui;display:flex;height:100vh;
margin:0;align-items:center;justify-content:center;background:#fff;color:#1a1a1a;padding:24px;text-align:center}
@media(prefers-color-scheme:dark){body{background:#191a1c;color:#e9e9eb}}
.b{max-width:360px}.e{color:#d4380d;font-size:17px;font-weight:600;margin-bottom:12px}
.h{color:#8a8a8e;font-size:13px}</style>
<div class="b"><div class="e">`+msg+`</div>
<div class="h">返回 App 重新点击「使用 TinyAuth 登录」</div></div>`)
}
