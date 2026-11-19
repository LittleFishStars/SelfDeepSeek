package main

// history_search：由服务端执行的历史会话检索工具
//
// 模型在需要回忆「以前聊过的某件事」时调用它，服务端在本机库里检索该用户的历史
// 会话，把命中片段作为工具结果回灌给模型继续回答。
//
// 和联网搜索的区别：联网搜索是上游自己执行的（web_search_tool_result），这里是我们
// 自己跑循环——上游返回 tool_use 后，服务端执行检索、拼一条 tool_result 再请求一次，
// 直到模型不再调工具为止（最多 3 轮，避免失控）。
//
// 结果块单独定义成 history_search_tool_result，前端按「历史检索」样式渲染，
// 不和联网搜索的引用列表混在一起。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"
)

// historyToolName 是注入给模型的工具名。
const historyToolName = "history_search"

// historyMaxRounds 限制工具循环轮数：正常一两轮就够，多了基本是模型在乱调。
const historyMaxRounds = 3

// historyResultLimit 单次检索最多返回几条会话。
const historyResultLimit = 5

// historyToolDef 返回注入到 tools 里的定义。
// 措辞尽量克制：模型能自己判断「要不要查历史」，不要鼓励乱调（每次都查等于白花钱）。
func historyToolDef() map[string]any {
	return map[string]any{
		"name": historyToolName,
		"description": "检索用户过去的历史会话（你自己的聊天记录），用于回忆以前讨论过的事情、" +
			"用户提到过的偏好或结论。只在确实需要旧信息时调用；当前对话里已经有的内容不要查。" +
			"查询用简短关键词，不要整句。",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "检索关键词，多个词用空格分隔",
				},
			},
			"required": []string{"query"},
		},
	}
}

// ensureHistoryTool 在开关开启时把工具塞进请求的 tools 数组（去重）。
func ensureHistoryTool(uid string, reqObj map[string]any) bool {
	if !shareContextEnabled(uid) {
		return false
	}
	list, _ := reqObj["tools"].([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if name, _ := m["name"].(string); name == historyToolName {
				return false
			}
		}
	}
	reqObj["tools"] = append(list, historyToolDef())
	return true
}

// ---------------------------------------------------------------- 工具调用收集

type toolCall struct {
	ID    string
	Name  string
	Input string
}

// toolCallCollector 从 SSE 事件里捞 tool_use 块。
// 客户端用的 stream 事件格式：content_block_start 带 id/name，随后
// content_block_delta 里 input_json_delta 逐段给出 partial_json。
type toolCallCollector struct {
	byIndex map[int]*toolCall
	order   []int
}

func newToolCallCollector() *toolCallCollector {
	return &toolCallCollector{byIndex: map[int]*toolCall{}}
}

func (c *toolCallCollector) feed(payload string) {
	var ev struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input any    `json:"input"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return
	}
	switch ev.Type {
	case "content_block_start":
		if ev.ContentBlock == nil || ev.ContentBlock.Type != "tool_use" {
			return
		}
		call := &toolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
		if ev.ContentBlock.Input != nil {
			if raw, err := json.Marshal(ev.ContentBlock.Input); err == nil && string(raw) != "{}" {
				call.Input = string(raw)
			}
		}
		c.byIndex[ev.Index] = call
		c.order = append(c.order, ev.Index)
	case "content_block_delta":
		if ev.Delta == nil || ev.Delta.Type != "input_json_delta" {
			return
		}
		if call, ok := c.byIndex[ev.Index]; ok {
			call.Input += ev.Delta.PartialJSON
		}
	}
}

// calls 返回收集到的、由服务端执行的工具调用（历史检索 + 沙箱脚本）。
// 其它工具名一律忽略，交给上游自己处理。
func (c *toolCallCollector) calls() []toolCall {
	var out []toolCall
	for _, idx := range c.order {
		call := c.byIndex[idx]
		if call == nil {
			continue
		}
		if call.Name != historyToolName && call.Name != rhaiToolName {
			continue
		}
		out = append(out, *call)
	}
	return out
}

// splitToolCalls 按工具名分组，便于分别执行。
func splitToolCalls(calls []toolCall) (history []toolCall, rhai []toolCall) {
	for _, call := range calls {
		switch call.Name {
		case historyToolName:
			history = append(history, call)
		case rhaiToolName:
			rhai = append(rhai, call)
		}
	}
	return
}

func parseToolQuery(input string) string {
	var v struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return strings.TrimSpace(input)
	}
	return strings.TrimSpace(v.Query)
}

// ---------------------------------------------------------------- 检索

type historyHit struct {
	ConvID    string  `json:"conv_id"`
	Title     string  `json:"title"`
	Excerpt   string  `json:"excerpt"`
	Role      string  `json:"role"`
	UpdatedAt float64 `json:"updated_at"`
}

// searchHistory 在该用户的历史消息里按关键词检索。
// 用 LIKE 做保守匹配（中文没有词边界，FTS 反而容易切错），命中越多的词权重越高。
func searchHistory(uid, query, exceptConvID string) []historyHit {
	words := splitQueryWords(query)
	if len(words) == 0 {
		return nil
	}
	var conds []string
	var args []any
	for _, w := range words {
		conds = append(conds, "m.content LIKE ?")
		args = append(args, "%"+w+"%")
	}
	args = append(args, uid, exceptConvID, historyResultLimit*4)
	sql := `SELECT m.conv_id, COALESCE(c.title,''), m.content, m.role, COALESCE(c.updated_at,0)
		FROM messages m JOIN conversations c ON c.id=m.conv_id
		WHERE (` + strings.Join(conds, " OR ") + `) AND c.user_id=? AND m.conv_id<>?
		ORDER BY c.updated_at DESC LIMIT ?`
	rows, err := db.Query(sql, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	seen := map[string]int{} // conv_id → 已收条数（同一会话最多两条，避免刷屏）
	var hits []historyHit
	for rows.Next() {
		var hit historyHit
		if err := rows.Scan(&hit.ConvID, &hit.Title, &hit.Excerpt, &hit.Role, &hit.UpdatedAt); err != nil {
			continue
		}
		if seen[hit.ConvID] >= 2 {
			continue
		}
		seen[hit.ConvID]++
		hit.Excerpt = excerptAround(hit.Excerpt, words, 200)
		hits = append(hits, hit)
		if len(hits) >= historyResultLimit {
			break
		}
	}
	return hits
}

// splitQueryWords 把查询拆成关键词：按空白与常见标点切，丢掉单字和过长片段。
func splitQueryWords(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，。;；、|/\\\"'()（）[]【】", r)
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len([]rune(f)) < 2 || len([]rune(f)) > 20 || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 && len(fields) > 0 {
		out = append(out, fields[0])
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// excerptAround 截取命中词周围的一段，避免把长回复整段塞给模型。
func excerptAround(text string, words []string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	pos := -1
	for _, w := range words {
		if i := strings.Index(text, w); i >= 0 {
			// 按 rune 定位
			pos = len([]rune(text[:i]))
			break
		}
	}
	if pos < 0 {
		pos = 0
	}
	start := pos - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
		start = end - width
		if start < 0 {
			start = 0
		}
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

// runHistorySearch 执行一次工具调用，返回给模型的纯文本结果。
func runHistorySearch(uid, convID, query string) ([]historyHit, string) {
	hits := searchHistory(uid, query, convID)
	if len(hits) == 0 {
		return nil, "没有检索到相关历史会话。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "检索到 %d 条相关历史记录（按最近更新排序）：\n", len(hits))
	for i, h := range hits {
		title := h.Title
		if title == "" {
			title = "（无标题会话）"
		}
		when := ""
		if h.UpdatedAt > 0 {
			when = time.Unix(int64(h.UpdatedAt), 0).Format("2006-01-02")
		}
		fmt.Fprintf(&sb, "\n%d. %s %s\n%s：%s\n", i+1, title, when, h.Role, h.Excerpt)
	}
	return hits, sb.String()
}

// ---------------------------------------------------------------- 回灌

// buildToolRound 把「助手调工具 + 工具结果」追加到请求体里，供下一轮请求使用。
func buildToolRound(body []byte, calls []toolCall, results []string, assistantText string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, _ := obj["messages"].([]any)

	// 助手这一轮的内容：文本（若有）+ 每个 tool_use
	var assistantBlocks []any
	if strings.TrimSpace(assistantText) != "" {
		assistantBlocks = append(assistantBlocks, map[string]any{"type": "text", "text": assistantText})
	}
	var resultBlocks []any
	for i, call := range calls {
		input := map[string]any{}
		if strings.TrimSpace(call.Input) != "" {
			_ = json.Unmarshal([]byte(call.Input), &input)
		}
		assistantBlocks = append(assistantBlocks, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": input,
		})
		resultBlocks = append(resultBlocks, map[string]any{
			"type":        "tool_result",
			"tool_use_id": call.ID,
			"content":     results[i],
		})
	}
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": assistantBlocks},
		map[string]any{"role": "user", "content": resultBlocks},
	)
	obj["messages"] = msgs
	// 工具轮次里不再让模型继续调同一个工具，避免死循环
	if i, ok := obj["tools"].([]any); ok {
		kept := make([]any, 0, len(i))
		for _, t := range i {
			if m, ok := t.(map[string]any); ok {
				if name, _ := m["name"].(string); name == historyToolName {
					continue
				}
			}
			kept = append(kept, t)
		}
		obj["tools"] = kept
	}
	if patched, err := json.Marshal(obj); err == nil {
		return patched
	}
	return body
}

// appendHistoryEvents 把本轮检索过程按客户端能解析的 SSE 事件写进任务流，
// 让界面显示「历史检索」块（和联网搜索的过程块并列）。
func appendHistoryEvents(j *streamJob, calls []toolCall, hits [][]historyHit, baseIndex int) {
	for i, call := range calls {
		query := parseToolQuery(call.Input)
		start := map[string]any{
			"type":  "content_block_start",
			"index": baseIndex + i*2,
			"content_block": map[string]any{
				"type":   "server_tool_use",
				"id":     call.ID,
				"name":   historyToolName,
				"input":  map[string]any{"query": query},
				"caller": map[string]any{"type": "direct"},
			},
		}
		j.appendSSE("content_block_start", start)
		j.appendSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": baseIndex + i*2})

		result := map[string]any{
			"type":  "content_block_start",
			"index": baseIndex + i*2 + 1,
			"content_block": map[string]any{
				"type":        "history_search_tool_result",
				"tool_use_id": call.ID,
				"query":       query,
				"results":     hits[i],
			},
		}
		j.appendSSE("content_block_start", result)
		j.appendSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": baseIndex + i*2 + 1})
	}
}

// handleHistoryToolRound 执行本轮所有检索调用，返回回灌文本与结果。
func handleHistoryToolRound(uid, convID string, calls []toolCall) ([]string, [][]historyHit) {
	var texts []string
	var hitsAll [][]historyHit
	for _, call := range calls {
		query := parseToolQuery(call.Input)
		hits, text := runHistorySearch(uid, convID, query)
		log.Printf("  历史检索 conv=%s query=%q 命中=%d", short(convID), query, len(hits))
		hitsAll = append(hitsAll, hits)
		texts = append(texts, text)
	}
	return texts, hitsAll
}

// toolCtx 给工具检索一个独立的小 context（只是本地查询，很快）。
func toolCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}
