package main

// 对话的树形存储：支持「编辑消息 → 重新生成 → 保留旧分支」，
// 也就是 DeepSeek 官方那种「1/2 版本切换」。
//
// 数据结构：
//   messages.parent_id 指向同会话里上一条消息，根消息为 NULL；
//   conversations.active_leaf 记录当前选中的叶子消息。
//
// 客户端每轮发全量历史（当前分支），服务端做：
//   1. 取当前分支（从 active_leaf 回溯到根）；
//   2. 与来料求最长公共前缀；
//   3. 只把超出前缀的部分挂成新节点（parent = 上一条），这就是新分支；
//   4. active_leaf 指向最后一个节点。
// 于是「编辑第 2 条再发」会自动产生一个兄弟节点，历史版本全部保留。

import (
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"time"
)

// node 是树上的一个消息节点
type node struct {
	ID       int64
	ParentID sql.NullInt64
	Role     string
	Content  string
	Thinking string
	Stop     string
	Files    string
	Blocks   string
	Created  float64
}

// decodeBlockList 把 blocks 列的 JSON 数组解成原始块，坏数据当空处理
func decodeBlockList(raw string) []json.RawMessage {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// mergeNode 把 srcs 的思考与块按顺序拼到 dst 前面（dst 自己的放最后）
func mergeNode(dst node, srcs []node) node {
	var think strings.Builder
	blocks := make([]json.RawMessage, 0, 8)
	push := func(n node) {
		if t := strings.TrimSpace(n.Thinking); t != "" {
			if think.Len() > 0 {
				think.WriteString("\n\n")
			}
			think.WriteString(t)
		}
		blocks = append(blocks, decodeBlockList(n.Blocks)...)
	}
	for _, s := range srcs {
		push(s)
	}
	push(dst)
	dst.Thinking = think.String()
	if len(blocks) > 0 {
		if raw, err := json.Marshal(blocks); err == nil {
			dst.Blocks = string(raw)
		}
	}
	return dst
}

// coalesceToolSteps 把「同一分支上连续的空正文 assistant」合并成一条。
//
// 为什么要合并：上游协议里每次工具调用都是一条独立的 assistant 消息（content 为空
// + tool_use 块），回传历史时必须保持这个形状，所以**存储不动**。但那是存储格式，
// 不是展示格式 —— 实时渲染时这些块本来就属于同一条消息，历史里逐条返回就变成一串
// 「只有思考、没有正文」的空消息，前端只能显示成「已停止」(messageThinkStopped)，
// 分支切换器也会被这些空节点撑乱（一次联网搜索能冒出三四个「已停止」）。
//
// 规则：
//   - 空正文 assistant 先攒着，由后面第一条有正文的 assistant 吸收；
//     parent 链拉平到攒之前那个节点，id 仍用被吸收的那条（前端据此切分支）
//   - 攒到 user 或链尾还没人吸收，就原样收成一条（这一轮确实中断了，保留「已停止」）
func coalesceToolSteps(nodes []node) []node {
	if len(nodes) < 2 {
		return nodes
	}
	out := make([]node, 0, len(nodes))
	var pending []node
	// absorb：dst 是 pending 之外的一条，把所有 pending 拼到它前面
	absorb := func(dst node) node {
		merged := mergeNode(dst, pending)
		merged.ParentID = pending[0].ParentID
		pending = nil
		return merged
	}
	// flush：无人吸收，pending 自成一条（最后一条作主，其余拼到它前面）
	flush := func() node {
		dst := pending[len(pending)-1]
		merged := mergeNode(dst, pending[:len(pending)-1])
		merged.ParentID = pending[0].ParentID
		pending = nil
		return merged
	}
	chainOK := func(n node) bool {
		// tree() 是按 id 全局排序的，相邻的两条未必在同一条分支上；
		// 只有「上一条正是这一条的父节点」时才允许合并，否则会把兄弟分支串起来
		return len(pending) > 0 && pending[len(pending)-1].ID == parentOf(n)
	}
	for _, n := range nodes {
		if n.Role == "assistant" && strings.TrimSpace(n.Content) == "" {
			if len(pending) > 0 && !chainOK(n) {
				out = append(out, flush())
			}
			pending = append(pending, n)
			continue
		}
		if len(pending) > 0 && chainOK(n) {
			if n.Role == "assistant" {
				n = absorb(n)
			} else {
				// 这批工具轮没有正文（这一轮被中断），单独保留一条
				out = append(out, flush())
			}
		}
		out = append(out, n)
	}
	if len(pending) > 0 {
		out = append(out, flush())
	}
	return out
}

// parentOf 取父节点 id（NULL 用 0）
func parentOf(n node) int64 {
	if n.ParentID.Valid {
		return n.ParentID.Int64
	}
	return 0
}

// siblingIndexFrom 用内存里的节点列表算「第几版 / 共几版」。
// 合并过的节点 parent 已经改变，不能再回库查（库里还是旧父子关系），
// 必须按合并后的结构算，否则分支切换器会显示 0/0。
func siblingIndexFrom(nodes []node) map[[2]int64][2]int {
	groups := map[int64][]int64{}
	for _, n := range nodes {
		k := parentOf(n)
		groups[k] = append(groups[k], n.ID)
	}
	out := make(map[[2]int64][2]int, len(nodes))
	for k, ids := range groups {
		for i, id := range ids {
			out[[2]int64{k, id}] = [2]int{i + 1, len(ids)}
		}
	}
	return out
}

// viewsFrom 合并工具轮后一次性构造视图（版本号按合并后的结构算）
func viewsFrom(nodes []node) []map[string]any {
	idx := siblingIndexFrom(nodes)
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeViewPre("", n, idx))
	}
	return out
}

// viewMessageCount 返回会话在客户端眼里的消息条数（工具轮已合并）。
//
// /v1/sync 用它做「本地是否陈旧」的判定，必须与 /v1/conversations/{id} 返回的
// tree 条数一致：之前用的是库里 messages 表的原始行数，合并工具轮之后视图条数
// 变少而库里不变，客户端永远看不出差异，于是一直用旧缓存渲染（表现为改完
// 服务端逻辑，重启 App 还是老样子）。
func viewMessageCount(convID string) int {
	rows, err := db.Query(`SELECT id, parent_id, role, content FROM messages WHERE conv_id=? ORDER BY id`, convID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var nodes []node
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Role, &n.Content); err == nil {
			nodes = append(nodes, n)
		}
	}
	return len(coalesceToolSteps(nodes))
}

// activePath 返回从根到 active_leaf 的消息序列（不含 assistant 之外的装饰）
func activePath(convID string) []node {
	var leaf int64
	_ = db.QueryRow(`SELECT active_leaf FROM conversations WHERE id=?`, convID).Scan(&leaf)
	if leaf == 0 {
		return nil
	}
	var out []node
	cur := leaf
	for i := 0; i < 5000 && cur != 0; i++ {
		var n node
		err := db.QueryRow(`SELECT id, parent_id, role, content, thinking, COALESCE(stop_reason,''), COALESCE(files,''), COALESCE(blocks,''), created_at
			FROM messages WHERE id=? AND conv_id=?`, cur, convID).
			Scan(&n.ID, &n.ParentID, &n.Role, &n.Content, &n.Thinking, &n.Stop, &n.Files, &n.Blocks, &n.Created)
		if err != nil {
			break
		}
		out = append(out, n)
		if !n.ParentID.Valid {
			break
		}
		cur = n.ParentID.Int64
	}
	// reverse
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// syncPath 用客户端发来的全量历史同步当前分支，返回新的叶子 id。
func syncPath(convID string, incoming []storedMsg) int64 {
	if convID == "" {
		return 0
	}
	storeMu.Lock()
	defer storeMu.Unlock()

	path := activePathLocked(convID)

	// 最长公共前缀（比较 role + 正文）
	k := 0
	for k < len(path) && k < len(incoming) {
		if path[k].Role != incoming[k].Role || strings.TrimRight(path[k].Content, "\n") != strings.TrimRight(incoming[k].Content, "\n") {
			break
		}
		k++
	}
	// 客户端"编辑后重发"时，编辑点之后的旧节点不再属于当前分支：
	// 这里不删旧节点，只是把新内容作为兄弟挂上去。

	var parent sql.NullInt64
	if k > 0 {
		parent = sql.NullInt64{Int64: path[k-1].ID, Valid: true}
	}
	leaf := int64(0)
	if k > 0 {
		leaf = path[k-1].ID
	}
	now := float64(time.Now().UnixNano()) / 1e9
	for i := k; i < len(incoming); i++ {
		m := incoming[i]
		filesJSON := ""
		if len(m.Files) > 0 {
			if raw, err := json.Marshal(m.Files); err == nil {
				filesJSON = string(raw)
			}
		}
		blocksJSON := ""
		if len(m.Blocks) > 0 {
			if raw, err := json.Marshal(m.Blocks); err == nil {
				blocksJSON = string(raw)
			}
		}
		res, err := db.Exec(`INSERT INTO messages(conv_id, parent_id, role, content, thinking, files, blocks, created_at)
			VALUES(?,?,?,?,?,?,?,?)`, convID, nullable(parent), m.Role, m.Content, m.Thinking, filesJSON, blocksJSON, now+float64(i)*0.001)
		if err != nil {
			log.Printf("!! 写消息失败: %v", err)
			break
		}
		id, _ := res.LastInsertId()
		parent = sql.NullInt64{Int64: id, Valid: true}
		leaf = id
	}
	if leaf != 0 {
		_, _ = db.Exec(`UPDATE conversations SET active_leaf=?, updated_at=? WHERE id=?`, leaf, now, convID)
	}
	// 记一条 upsert（recordChange 用独立锁，这里保持 storeMu 不释放更安全）
	var owner string
	_ = db.QueryRow(`SELECT user_id FROM conversations WHERE id=?`, convID).Scan(&owner)
	recordChange(owner, convID, "upsert")
	// 标题：仍取根下的第一条用户消息
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE id=? AND title=''`, convID).Scan(&n)
	if n > 0 {
		if t := firstUserTextLocked(convID); t != "" {
			_, _ = db.Exec(`UPDATE conversations SET title=? WHERE id=?`, t, convID)
		}
	}
	return leaf
}

func activePathLocked(convID string) []node {
	return activePath(convID)
}

func nullable(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func firstUserTextLocked(convID string) string {
	var txt string
	_ = db.QueryRow(`SELECT content FROM messages WHERE conv_id=? AND role='user' ORDER BY (parent_id IS NOT NULL), id LIMIT 1`, convID).Scan(&txt)
	txt = strings.TrimSpace(txt)
	r := []rune(txt)
	if len(r) > 30 {
		r = r[:30]
	}
	return string(r)
}

// appendAssistant 把这一轮的助手回复挂到当前叶子下面（成为新的 active_leaf）。
func appendAssistant(convID, text, thinking string, meta streamMeta, blocks []json.RawMessage) {
	if convID == "" || (strings.TrimSpace(text) == "" && strings.TrimSpace(thinking) == "") {
		return
	}
	storeMu.Lock()
	defer storeMu.Unlock()

	var leaf int64
	_ = db.QueryRow(`SELECT active_leaf FROM conversations WHERE id=?`, convID).Scan(&leaf)

	now := float64(time.Now().UnixNano()) / 1e9
	var parent any
	if leaf != 0 {
		parent = leaf
	}
	blocksJSON := ""
	if len(blocks) > 0 {
		if raw, err := json.Marshal(blocks); err == nil {
			blocksJSON = string(raw)
		}
	}
	res, err := db.Exec(`INSERT INTO messages(conv_id, parent_id, role, content, thinking, stop_reason, blocks, created_at)
		VALUES(?,?,?,?,?,?,?,?)`, convID, parent, "assistant", text, thinking, meta.stop, blocksJSON, now)
	if err != nil {
		log.Printf("!! 写助手消息失败: %v", err)
		return
	}
	newID, _ := res.LastInsertId()
	_, _ = db.Exec(`UPDATE conversations SET active_leaf=?, updated_at=?,
		tokens_in=tokens_in+?, tokens_out=tokens_out+? WHERE id=?`,
		newID, now, meta.tokensIn, meta.tokensOut, convID)
	// 会话累计值之外，再记一条流水，仪表盘才能画趋势
	recordUsage(convID, meta.tokensIn, meta.tokensOut, meta.stop)
	if strings.TrimSpace(meta.title) != "" {
		_, _ = db.Exec(`UPDATE conversations SET title=? WHERE id=?`, strings.TrimSpace(meta.Title()), convID)
	}
}

// Title 兼容 streamMeta 的字段名
func (m streamMeta) Title() string { return m.title }

// switchLeaf 切换当前分支到指定消息（UI 上的「1/2」切换）。
func switchLeaf(convID string, id int64) bool {
	var owner string
	if err := db.QueryRow(`SELECT conv_id FROM messages WHERE id=?`, id).Scan(&owner); err != nil || owner != convID {
		return false
	}
	// 允许选中间节点：它的后代里挑一个最深的叶子，保证路径完整
	leaf := id
	for i := 0; i < 5000; i++ {
		var child int64
		err := db.QueryRow(`SELECT id FROM messages WHERE conv_id=? AND parent_id=? ORDER BY id DESC LIMIT 1`, convID, leaf).Scan(&child)
		if err != nil {
			break
		}
		leaf = child
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	_, _ = db.Exec(`UPDATE conversations SET active_leaf=? WHERE id=?`, leaf, convID)
	return true
}

// tree 返回整棵消息树（导出/调试用）
func tree(convID string) []node {
	rows, err := db.Query(`SELECT id, parent_id, role, content, thinking, COALESCE(stop_reason,''), COALESCE(files,''), COALESCE(blocks,''), created_at
		FROM messages WHERE conv_id=? ORDER BY id`, convID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []node
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Role, &n.Content, &n.Thinking, &n.Stop, &n.Files, &n.Blocks, &n.Created); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// siblings 统计某条消息在同一父节点下的兄弟数量与自身序号（1-based）
func siblings(convID string, n node) (idx, total int) {
	q := `SELECT id FROM messages WHERE conv_id=? AND parent_id IS NULL ORDER BY id`
	if n.ParentID.Valid {
		q = `SELECT id FROM messages WHERE conv_id=? AND parent_id=? ORDER BY id`
	}
	var rows *sql.Rows
	var err error
	if n.ParentID.Valid {
		rows, err = db.Query(q, convID, n.ParentID.Int64)
	} else {
		rows, err = db.Query(q, convID)
	}
	if err != nil {
		return 1, 1
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		i++
		if id == n.ID {
			idx = i
		}
	}
	total = i
	if idx == 0 {
		idx = 1
	}
	if total == 0 {
		total = 1
	}
	return
}

// backfillTree 把旧版（线性、无 parent_id/active_leaf）的会话补成一条链，
// 让历史数据在新接口下也能正常显示。幂等：只处理 active_leaf=0 的会话。
func backfillTree() {
	rows, err := db.Query(`SELECT id FROM conversations WHERE active_leaf=0`)
	if err != nil {
		return
	}
	var convs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			convs = append(convs, id)
		}
	}
	rows.Close()

	for _, conv := range convs {
		mrows, err := db.Query(`SELECT id FROM messages WHERE conv_id=? ORDER BY id`, conv)
		if err != nil {
			continue
		}
		var ids []int64
		for mrows.Next() {
			var id int64
			if err := mrows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		mrows.Close()
		if len(ids) == 0 {
			continue
		}
		for i, id := range ids {
			if i == 0 {
				_, _ = db.Exec(`UPDATE messages SET parent_id=NULL WHERE id=?`, id)
			} else {
				_, _ = db.Exec(`UPDATE messages SET parent_id=? WHERE id=?`, ids[i-1], id)
			}
		}
		_, _ = db.Exec(`UPDATE conversations SET active_leaf=? WHERE id=?`, ids[len(ids)-1], conv)
	}
	if len(convs) > 0 {
		log.Printf("已把 %d 个旧会话补成单链结构", len(convs))
	}
}
