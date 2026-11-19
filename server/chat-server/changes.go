package main

// 会话变更日志：新增/更新/删除都记一条，客户端增量同步时按 since 拉取，
// 这样「服务端删掉的会话」也能同步到客户端（删除是不可逆的 tombstone）。

import (
	"sync"
	"time"
)

// 独立互斥：recordChange 可能被已经持有 storeMu 的代码调用，不能再抢同一把锁
var changeMu sync.Mutex

// recordChange 记录一次会话变更；op 只有 upsert / delete
func recordChange(userID, convID, op string) {
	if userID == "" || convID == "" {
		return
	}
	changeMu.Lock()
	_, _ = db.Exec(`INSERT INTO conv_changes(user_id, conv_id, op, ts) VALUES(?,?,?,?)`,
		userID, convID, op, float64(time.Now().UnixNano())/1e9)
	changeMu.Unlock()
}

// changesSince 返回某用户在 since 之后的变更（按时间升序）
func changesSince(userID string, since float64) []map[string]any {
	rows, err := db.Query(`SELECT conv_id, op, ts FROM conv_changes
		WHERE user_id=? AND ts>? ORDER BY ts ASC, id ASC LIMIT 5000`, userID, since)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		var convID, op string
		var ts float64
		if err := rows.Scan(&convID, &op, &ts); err == nil {
			out = append(out, map[string]any{"conv_id": convID, "op": op, "ts": ts})
		}
	}
	return out
}
