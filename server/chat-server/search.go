package main

// 会话搜索：跨所有会话按关键词检索消息正文，返回命中的会话与对应段落。
//
// 与 history_search 工具的区别：那个把结果回灌给模型；这个是给界面用的
// （侧栏搜索框），所以要带消息 id，前端据此跳到对应段落。

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// searchHit 一条命中：会话信息 + 命中的那条消息 + 截断后的片段。
type searchHit struct {
	ConvID    string  `json:"conv_id"`
	Title     string  `json:"title"`
	UpdatedAt float64 `json:"updated_at"`
	MessageID int64   `json:"message_id"`
	Role      string  `json:"role"`
	Excerpt   string  `json:"excerpt"`
	// Matched 命中的关键词，前端用它做高亮
	Matched []string `json:"matched"`
	// Score 命中词数，仅用于排序，不下发给界面
	Score int `json:"-"`
}

// handleSearch 处理 GET /v1/search?q=关键词&limit=N
//
// 排序：命中词多的优先，其次按会话更新时间倒序；同一会话最多 3 条，
// 避免一个长会话把整页占满。
func handleSearch(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if query == "" {
		writeJSON(w, 200, map[string]any{"query": query, "hits": []searchHit{}, "truncated": false})
		return
	}

	words := splitQueryWords(query)
	if len(words) == 0 {
		words = []string{query}
	}

	conds := make([]string, 0, len(words))
	args := make([]any, 0, len(words)+2)
	args = append(args, uid)
	for _, word := range words {
		conds = append(conds, "m.content LIKE ?")
		args = append(args, "%"+word+"%")
	}
	// 多取一些做评分与去重，最终再截断到 limit
	args = append(args, limit*8)

	rows, err := db.Query(`SELECT m.id, m.conv_id, COALESCE(c.title,''), COALESCE(c.updated_at,0),
			m.content, m.role
		FROM messages m JOIN conversations c ON c.id = m.conv_id
		WHERE c.user_id = ? AND (`+strings.Join(conds, " OR ")+`)
		ORDER BY c.updated_at DESC, m.id DESC
		LIMIT ?`, args...)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()

	perConv := map[string]int{}
	hits := make([]searchHit, 0, limit)
	for rows.Next() {
		var hit searchHit
		var content string
		if err := rows.Scan(&hit.MessageID, &hit.ConvID, &hit.Title, &hit.UpdatedAt,
			&content, &hit.Role); err != nil {
			continue
		}
		if perConv[hit.ConvID] >= 3 {
			continue
		}
		matched := make([]string, 0, len(words))
		for _, word := range words {
			if strings.Contains(content, word) {
				matched = append(matched, word)
			}
		}
		if len(matched) == 0 {
			continue
		}
		perConv[hit.ConvID]++
		hit.Matched = matched
		hit.Score = len(matched)
		hit.Excerpt = excerptAround(strings.TrimSpace(content), matched, 160)
		hits = append(hits, hit)
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].UpdatedAt > hits[j].UpdatedAt
	})
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}

	writeJSON(w, 200, map[string]any{
		"query":     query,
		"hits":      hits,
		"truncated": truncated,
		"now":       float64(time.Now().Unix()),
	})
}
