package main

// 回复缓存（离线补发）：服务端生成的回答如果没能完整送到客户端（断网、杀进程、切后台），
// 就先留在 reply_cache 里；客户端下次连上时拉取、落库确认后再删掉。

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type cachedReply struct {
	ConvID   string            `json:"conv_id"`
	Text     string            `json:"text"`
	Thinking string            `json:"thinking"`
	Stop     string            `json:"stop_reason"`
	Blocks   []json.RawMessage `json:"blocks,omitempty"`
	Created  float64           `json:"created_at"`
}

// saveCachedReply 写入（同一会话只保留最新一条，新的覆盖旧的）
func saveCachedReply(uid, convID, text, thinking, stop string, blocks []json.RawMessage) {
	if convID == "" || (strings.TrimSpace(text) == "" && strings.TrimSpace(thinking) == "") {
		return
	}
	payload, err := json.Marshal(cachedReply{
		ConvID: convID, Text: text, Thinking: thinking, Stop: stop,
		Blocks: blocks, Created: float64(time.Now().Unix()),
	})
	if err != nil {
		return
	}
	storeMu.Lock()
	_, _ = db.Exec(`INSERT INTO reply_cache(conv_id, user_id, payload, created_at)
		VALUES(?,?,?,?)
		ON CONFLICT(conv_id) DO UPDATE SET payload=excluded.payload, created_at=excluded.created_at`,
		convID, uid, string(payload), time.Now().Unix())
	storeMu.Unlock()
	log.Printf("  回复已入离线缓存 conv=%s 正文=%d字", short(convID), len([]rune(text)))
}

// handleCacheStream 缓存下发：先一次性把所有缓存内容发出去（发成功即删除），
// 之后保持连接，把后续产生的缓存内容继续推送，直到客户端断开。
func handleCacheStream(w http.ResponseWriter, r *http.Request) {
	uid, _, ok := resolveToken(tokenOf(r))
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	sendOnce := func() bool {
		rows, err := db.Query(`SELECT conv_id, payload FROM reply_cache WHERE user_id=? ORDER BY created_at ASC LIMIT 100`, uid)
		if err != nil {
			return true
		}
		type item struct{ convID, payload string }
		var items []item
		for rows.Next() {
			var it item
			if rows.Scan(&it.convID, &it.payload) == nil {
				items = append(items, it)
			}
		}
		rows.Close()
		for _, it := range items {
			if _, err := io.WriteString(w, "event: reply\ndata: "+it.payload+"\n\n"); err != nil {
				return false // 写失败：保留缓存，下次再发
			}
			if flusher != nil {
				flusher.Flush()
			}
			storeMu.Lock()
			_, _ = db.Exec(`DELETE FROM reply_cache WHERE user_id=? AND conv_id=?`, uid, it.convID)
			storeMu.Unlock()
			log.Printf("  缓存下发成功 conv=%s", short(it.convID))
		}
		return true
	}

	if !sendOnce() {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !sendOnce() {
				return
			}
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
