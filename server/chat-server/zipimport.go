package main

// 服务端导入：客户端把导出的 ZIP 直接上传到这里，由服务器解析成会话与消息写入数据库。
// 支持的包结构（chat-demo / DeepSeek 导出格式）：
//
//	manifest.json                 { "export_format_version": 1 }
//	chat_session_index.json       [{ id, title, message_count, inserted_at, updated_at }]
//	chat_sessions/<id>.json       { id, title, current_message_id, inserted_at, updated_at,
//	                                messages: [{ role: USER|ASSISTANT, inserted_at,
//	                                            fragments: [{ type, content }] }] }
//	chat_sessions/<id>_files/*    图片原件（可选，不入库）

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// flexTime 兼容两种导出写法：数值（unix 秒）与 ISO 8601 字符串
type flexTime float64

func (t *flexTime) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "null" || raw == `""` {
		*t = 0
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
			if ts, err := time.Parse(layout, s); err == nil {
				*t = flexTime(ts.Unix())
				return nil
			}
		}
		*t = 0
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		*t = 0
		return nil
	}
	*t = flexTime(f)
	return nil
}

type zipIndexEntry struct {
	ID           string   `json:"id"`
	Title        *string  `json:"title"`
	MessageCount int      `json:"message_count"`
	InsertedAt   flexTime `json:"inserted_at"`
	UpdatedAt    flexTime `json:"updated_at"`
}

type zipFragment struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type zipMessage struct {
	ID         json.RawMessage  `json:"id"`
	ParentID   *json.RawMessage `json:"parent_id"`
	Role       string           `json:"role"`
	InsertedAt flexTime         `json:"inserted_at"`
	Fragments  []zipFragment    `json:"fragments"`
}

type zipSession struct {
	ID          string          `json:"id"`
	Title       *string         `json:"title"`
	InsertedAt  flexTime        `json:"inserted_at"`
	UpdatedAt   flexTime        `json:"updated_at"`
	CurrentID   json.RawMessage `json:"current_message_id"`
	Messages    []zipMessage    `json:"messages"`
	SessionPath string          `json:"-"`
}

// 导入任务状态：一个用户同时只有一个任务，客户端轮询显示解析进度
type importJob struct {
	Running       bool    `json:"running"`
	Total         int     `json:"total"`
	Processed     int     `json:"processed"`
	Conversations int     `json:"conversations"`
	Messages      int     `json:"messages"`
	Error         string  `json:"error,omitempty"`
	FinishedAt    float64 `json:"finished_at"`
	// 汇总阶段（导入完成后把会话并入用户画像），客户端据此显示「汇总中…」
	Summarizing  bool `json:"summarizing"`
	SummaryDone  int  `json:"summary_done"`
	SummaryTotal int  `json:"summary_total"`
}

var (
	importJobsMu sync.Mutex
	importJobs   = map[string]*importJob{}
)

func setJob(uid string, fn func(*importJob)) {
	importJobsMu.Lock()
	defer importJobsMu.Unlock()
	job, ok := importJobs[uid]
	if !ok {
		job = &importJob{}
		importJobs[uid] = job
	}
	fn(job)
}

// handleImportStatus 返回当前用户的导入进度
func handleImportStatus(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	importJobsMu.Lock()
	job := importJobs[uid]
	out := importJob{}
	if job != nil {
		out = *job
	}
	importJobsMu.Unlock()
	// 汇总进度单独维护，这里合进同一个响应，客户端只轮询一个接口
	if pj := profileJobStatus(uid); pj.Running || pj.Total > 0 {
		out.Summarizing = pj.Running
		out.SummaryDone = pj.Done
		out.SummaryTotal = pj.Total
	}
	writeJSON(w, 200, out)
}

// handleZipImport 接收 multipart 文件字段 file，解析后写入 conversations/messages。
func handleZipImport(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, 400, map[string]any{"error": "上传失败: " + err.Error()})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "缺少文件字段 file"})
		return
	}
	defer file.Close()

	// 先落盘再异步解析：大包解析要几十秒，让客户端立刻拿到响应，避免连接被中断
	tmp, err := os.CreateTemp("", "chat-import-*.zip")
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "服务端暂存失败"})
		return
	}
	if _, err := io.Copy(tmp, io.LimitReader(file, 512<<20)); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		writeJSON(w, 500, map[string]any{"error": "接收文件失败"})
		return
	}
	tmp.Close()
	name := tmp.Name()
	log.Printf("  收到导入包 user=%s 大小=%.1fMB", short(uid), float64(header.Size)/(1<<20))
	setJob(uid, func(j *importJob) {
		*j = importJob{Running: true}
	})
	writeJSON(w, 202, map[string]any{"ok": true, "queued": true})
	go importZipAsync(name, uid)
}

// importZipAsync 在后台解析压缩包并写入数据库
func importZipAsync(path, uid string) {
	defer os.Remove(path)
	file, err := os.Open(path)
	if err != nil {
		log.Printf("!! 打开导入包失败: %v", err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return
	}

	zr, err := zip.NewReader(file, info.Size())
	if err != nil {
		log.Printf("!! 导入包不是有效 ZIP: %v", err)
		setJob(uid, func(j *importJob) {
			j.Running = false
			j.Error = "不是有效的 ZIP 压缩包"
		})
		return
	}

	sessions := map[string]*zipSession{}
	titles := map[string]string{}
	version := 0
	var entries []string
	var candidates []*zip.File

	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		if !f.FileInfo().IsDir() {
			entries = append(entries, name)
		}
		if strings.HasSuffix(strings.ToLower(name), ".json") {
			candidates = append(candidates, f)
		}
		switch {
		case name == "manifest.json":
			raw, err := readZipEntry(f)
			if err != nil {
				log.Printf("!! manifest.json 读取失败")
				return
			}
			var m struct {
				Version int `json:"export_format_version"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				log.Printf("!! manifest.json 格式不正确")
				return
			}
			version = m.Version
		case name == "chat_session_index.json":
			raw, err := readZipEntry(f)
			if err != nil {
				continue
			}
			var list []zipIndexEntry
			if err := json.Unmarshal(raw, &list); err == nil {
				for _, e := range list {
					if e.Title != nil {
						titles[e.ID] = *e.Title
					}
				}
			}
		case strings.HasPrefix(name, "chat_sessions/") && strings.HasSuffix(name, ".json") && !strings.Contains(name, "_files/"):
			raw, err := readZipEntry(f)
			if err != nil {
				continue
			}
			var s zipSession
			if err := json.Unmarshal(raw, &s); err != nil || s.ID == "" {
				continue
			}
			s.SessionPath = name
			sessions[s.ID] = &s
		}
	}

	if version > 3 {
		log.Printf("!! 压缩包版本 %d 过新，未导入", version)
		return
	}
	// 官方导出的包结构不止一种：没有 chat_sessions/ 时按通用 JSON 结构再试一次
	if len(sessions) == 0 {
		sessions, titles = parseGenericArchive(candidates)
	}
	if len(sessions) == 0 {
		hint := ""
		if len(entries) > 0 {
			shown := entries
			if len(shown) > 8 {
				shown = shown[:8]
			}
			hint = "；包内文件：" + strings.Join(shown, "、")
			if len(entries) > 8 {
				hint += " 等 " + strconv.Itoa(len(entries)) + " 个"
			}
		}
		log.Printf("!! 压缩包里没有找到可识别的会话%s", hint)
		return
	}

	setJob(uid, func(j *importJob) {
		j.Total = len(sessions)
	})

	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	convs, msgsTotal := 0, 0
	var importedIDs []string
	for _, id := range ids {
		s := sessions[id]
		// 保留导出包里的 parent_id 关系：同一提问下多次「重新生成」是兄弟节点，
		// 只写当前分支会丢掉历史版本，版本切换也就失效了。
		var conv []importedMsg
		for _, m := range s.Messages {
			role := strings.ToLower(m.Role)
			if role != "user" && role != "assistant" {
				continue
			}
			var text, thinking strings.Builder
			for _, fr := range m.Fragments {
				switch fr.Type {
				case "REQUEST", "RESPONSE", "TEMPLATE_RESPONSE":
					text.WriteString(fr.Content)
				case "THINK":
					thinking.WriteString(fr.Content)
				}
			}
			if text.Len() == 0 && thinking.Len() == 0 {
				continue
			}
			conv = append(conv, importedMsg{
				srcID:    rawID(m.ID),
				parentID: rawIDOf(m.ParentID),
				role:     role,
				content:  text.String(),
				thinking: thinking.String(),
			})
		}
		if len(conv) == 0 {
			continue
		}

		ensureConversation(id, uid, env("DS_MODEL", "deepseek-flash"))
		importTree(id, conv, rawID(s.CurrentID))

		title := ""
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		} else if t := titles[id]; t != "" {
			title = t
		}
		updated := float64(s.UpdatedAt)
		if updated == 0 {
			updated = float64(s.InsertedAt)
		}
		if updated == 0 {
			updated = float64(time.Now().Unix())
		}
		storeMu.Lock()
		if title != "" {
			_, _ = db.Exec(`UPDATE conversations SET title=? WHERE id=? AND (title='' OR title IS NULL)`, title, id)
		}
		_, _ = db.Exec(`UPDATE conversations SET updated_at=? WHERE id=?`, updated, id)
		storeMu.Unlock()

		convs++
		msgsTotal += len(conv)
		importedIDs = append(importedIDs, id)
		setJob(uid, func(j *importJob) {
			j.Processed++
			j.Conversations = convs
			j.Messages = msgsTotal
		})
	}

	log.Printf("  ZIP 导入完成 user=%s 会话=%d 消息=%d", short(uid), convs, msgsTotal)
	setJob(uid, func(j *importJob) {
		j.Running = false
		j.Conversations = convs
		j.Messages = msgsTotal
		j.FinishedAt = float64(time.Now().Unix())
	})
	// 导入完成后把这些会话批量摘要并并入用户画像；客户端据 profile job 显示「汇总中」
	startImportProfileJob(uid, importedIDs)
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 64<<20))
}

// parseGenericArchive 兼容常见的导出结构：
//
//	[{id,title,messages:[{role,content}]}] / {"conversations":[...]} / {"data":[...]}
//	消息内容支持字符串、{content:"…"}、{parts:["…"]}、[{type,text|content}]
func parseGenericArchive(files []*zip.File) (map[string]*zipSession, map[string]string) {
	sessions := map[string]*zipSession{}
	titles := map[string]string{}

	addConversation := func(raw map[string]any, fallbackID string) {
		msgsRaw, ok := raw["messages"].([]any)
		if !ok || len(msgsRaw) == 0 {
			return
		}
		id := fallbackID
		for _, key := range []string{"id", "conversation_id", "chat_session_id", "session_id"} {
			if v, ok := raw[key].(string); ok && v != "" {
				id = v
				break
			}
		}
		if id == "" {
			id = "import-" + strconv.FormatInt(int64(len(sessions)+1), 10)
		}
		s := &zipSession{ID: id}
		if t, ok := raw["title"].(string); ok {
			s.Title = &t
			titles[id] = t
		}
		s.UpdatedAt = flexTime(toFloat(raw["updated_at"]))
		if s.UpdatedAt == 0 {
			s.UpdatedAt = flexTime(toFloat(raw["inserted_at"]))
		}
		for _, mr := range msgsRaw {
			mm, ok := mr.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			role = strings.ToLower(role)
			if role != "user" && role != "assistant" {
				continue
			}
			text, thinking := flattenContent(mm)
			if text == "" && thinking == "" {
				continue
			}
			s.Messages = append(s.Messages, zipMessage{
				Role:       role,
				InsertedAt: flexTime(toFloat(mm["inserted_at"])),
				Fragments: []zipFragment{
					{Type: "RESPONSE", Content: text},
					{Type: "THINK", Content: thinking},
				},
			})
		}
		if len(s.Messages) > 0 {
			sessions[id] = s
		}
	}

	for _, f := range files {
		raw, err := readZipEntry(f)
		if err != nil {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		switch v := value.(type) {
		case []any:
			for i, item := range v {
				if m, ok := item.(map[string]any); ok {
					addConversation(m, "import-"+strconv.Itoa(i+1))
				}
			}
		case map[string]any:
			for _, key := range []string{"conversations", "data", "sessions", "chats"} {
				if list, ok := v[key].([]any); ok {
					for i, item := range list {
						if m, ok := item.(map[string]any); ok {
							addConversation(m, "import-"+strconv.Itoa(i+1))
						}
					}
				}
			}
			if _, ok := v["messages"]; ok {
				addConversation(v, "import-1")
			}
		}
	}
	return sessions, titles
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, n); err == nil {
				return float64(ts.Unix())
			}
		}
	}
	return 0
}

// flattenContent 从各种消息内容结构里抽出正文与思考
func flattenContent(m map[string]any) (string, string) {
	var text, thinking strings.Builder
	var sink func(v any, into *strings.Builder)
	sink = func(v any, into *strings.Builder) {
		switch c := v.(type) {
		case string:
			into.WriteString(c)
		case []any:
			for _, item := range c {
				switch part := item.(type) {
				case string:
					into.WriteString(part)
				case map[string]any:
					kind, _ := part["type"].(string)
					if strings.EqualFold(kind, "thinking") || strings.EqualFold(kind, "reasoning") {
						if str, ok := part["thinking"].(string); ok {
							thinking.WriteString(str)
							continue
						}
					}
					for _, k := range []string{"text", "content"} {
						if str, ok := part[k].(string); ok {
							into.WriteString(str)
							break
						}
					}
				}
			}
		case map[string]any:
			if parts, ok := c["parts"]; ok {
				sink(parts, into)
				return
			}
			for _, k := range []string{"text", "content"} {
				if str, ok := c[k].(string); ok {
					into.WriteString(str)
					return
				}
			}
		}
	}
	sink(m["content"], &text)
	if th, ok := m["thinking"].(string); ok {
		thinking.WriteString(th)
	}
	return text.String(), thinking.String()
}

// rawID 把 JSON 里的 id（可能是数字或字符串）统一成字符串
func rawID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	v := strings.TrimSpace(string(raw))
	v = strings.Trim(v, `"`)
	if v == "null" {
		return ""
	}
	return v
}

func rawIDOf(raw *json.RawMessage) string {
	if raw == nil {
		return ""
	}
	return rawID(*raw)
}

// importTree 按 parent_id 重建消息树，返回当前叶子对应新库里的节点 id。
// 找不到父节点（导出包里父消息被过滤掉）的消息当作根节点处理。
type importedMsg struct {
	srcID    string
	parentID string
	role     string
	content  string
	thinking string
}

func importTree(convID string, conv []importedMsg, currentID string) int64 {
	storeMu.Lock()
	defer storeMu.Unlock()

	now := float64(time.Now().UnixNano()) / 1e9
	idOf := make(map[string]int64, len(conv))
	// 多轮插入：父消息可能排在子消息之后
	pending := conv
	for round := 0; round < 8 && len(pending) > 0; round++ {
		var rest = pending[:0]
		progressed := false
		for _, m := range pending {
			var parent sql.NullInt64
			if m.parentID != "" {
				if pid, ok := idOf[m.parentID]; ok {
					parent = sql.NullInt64{Int64: pid, Valid: true}
				} else {
					rest = append(rest, m)
					continue
				}
			}
			res, err := db.Exec(`INSERT INTO messages(conv_id, parent_id, role, content, thinking, created_at)
				VALUES(?,?,?,?,?,?)`, convID, nullable(parent), m.role, m.content, m.thinking, now)
			if err != nil {
				log.Printf("!! 写导入消息失败: %v", err)
				continue
			}
			newID, _ := res.LastInsertId()
			if m.srcID != "" {
				idOf[m.srcID] = newID
			}
			progressed = true
		}
		if !progressed {
			// 剩下的都是孤儿（父消息缺失），按顺序挂到根
			for _, m := range rest {
				res, err := db.Exec(`INSERT INTO messages(conv_id, parent_id, role, content, thinking, created_at)
					VALUES(?,?,?,?,?,?)`, convID, nil, m.role, m.content, m.thinking, now)
				if err != nil {
					continue
				}
				newID, _ := res.LastInsertId()
				if m.srcID != "" {
					idOf[m.srcID] = newID
				}
			}
			break
		}
		pending = rest
	}

	// 当前叶子：优先用导出包里的 current_message_id；缺失时取最深的后代
	leaf := idOf[currentID]
	if leaf == 0 {
		leaf = deepestLeafLocked(convID)
	}
	_, _ = db.Exec(`UPDATE conversations SET active_leaf=? WHERE id=?`, leaf, convID)
	log.Printf("  导入树 conv=%s 消息=%d 叶子=%d", short(convID), len(conv), leaf)
	return leaf
}

// deepestLeafLocked 找没有子节点的最深节点（调用方需持有 storeMu）
func deepestLeafLocked(convID string) int64 {
	var leaf int64
	_ = db.QueryRow(`SELECT id FROM messages WHERE conv_id=? AND id NOT IN
		(SELECT parent_id FROM messages WHERE conv_id=? AND parent_id IS NOT NULL)
		ORDER BY id DESC LIMIT 1`, convID, convID).Scan(&leaf)
	return leaf
}
