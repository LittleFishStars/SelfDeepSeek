package main

// 按客户端类型发放的长期令牌：一个用户在同一设备类型上只有一个有效令牌。
// 规则：
//   - 令牌不过期，请求时通过 Authorization: Bearer 或 x-api-key 携带
//   - 同一设备类型再次登录 → 旋转该设备的令牌（旧令牌立即失效），另一种设备不受影响
//   - 设备类型：web（浏览器）/ android（安卓壳）/ harmony（鸿蒙壳），未知值一律按 web 处理

import (
	"log"
	"time"
)

// 只认这三种设备类型；其它值归一为 web
func normalizeDevice(device string) string {
	switch device {
	case "android", "web", "harmony":
		return device
	}
	return "web"
}

// issueClientToken 为 (user, device) 发放令牌：已存在就旋转，不存在则新建。
func issueClientToken(userID, device string) (string, error) {
	device = normalizeDevice(device)
	tok := randomToken()
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO client_tokens(user_id, device, token, created_at, updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(user_id, device) DO UPDATE SET token=excluded.token, updated_at=excluded.updated_at`,
		userID, device, tok, now, now)
	if err != nil {
		return "", err
	}
	log.Printf("  发放令牌 user=%s device=%s", short(userID), device)
	return tok, nil
}

// resolveToken 用任意客户端令牌找到用户；兼容老库里 users.token 的历史令牌。
func resolveToken(tok string) (userID, device string, ok bool) {
	if tok == "" {
		return "", "", false
	}
	err := db.QueryRow(`SELECT user_id, device FROM client_tokens WHERE token=?`, tok).Scan(&userID, &device)
	if err == nil {
		storeMu.Lock()
		_, _ = db.Exec(`UPDATE client_tokens SET updated_at=? WHERE token=?`, time.Now().Unix(), tok)
		storeMu.Unlock()
		return userID, device, true
	}
	return "", "", false
}
