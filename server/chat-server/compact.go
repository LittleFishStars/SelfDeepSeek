package main

// 上下文压缩：deepseek-flash 支持约 1M 上下文，但长对话迟早会撞上限。
// 做法参考 DSH 的 compaction：把「较早的对话」交给模型总结成一段结构化摘要，
// 保留最近若干轮原文，用摘要 + 近期原文继续对话；服务端库里始终保留完整原文。
//
//	CHAT_CONTEXT_WINDOW    模型上下文窗口（默认 1048576，deepseek-flash）
//	CHAT_COMPACT_RATIO     达到窗口的多少比例时折叠（默认 0.8）
//	CHAT_COMPACT_THRESHOLD 直接指定 token 阈值（留空则用窗口×比例）
//	CHAT_COMPACT_KEEP       保留最近多少条原文（默认 8）
//	CHAT_COMPACT_MODEL      用来做摘要的模型（默认与主模型相同）

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

var (
	// 压缩阈值按「单次输出预算 max_tokens」的 compactRatio（默认 80%）触发：
	// 达到 80% 时本轮照常完整输出，压缩只影响下一轮请求——compactMessages
	// 在 handleProxy 发请求前调用，绝不打断正在进行的生成。
	contextWindow    = envInt("CHAT_CONTEXT_WINDOW", 1048576) // 模型上下文窗口，仅日志参考
	compactRatio     = envFloat("CHAT_COMPACT_RATIO", 0.8)
	compactThreshold = envInt("CHAT_COMPACT_THRESHOLD", int(float64(defaultMaxTokens)*compactRatio))
	compactKeep      = envInt("CHAT_COMPACT_KEEP", 8)
)

func init() {
	log.Printf("上下文压缩阈值: %d tokens（%.0f%% of max_tokens=%d，上下文窗口 %d，保留最近 %d 条）",
		compactThreshold, compactRatio*100, defaultMaxTokens, contextWindow, compactKeep)
}

// estimateTokens 粗估 token：中日韩字符约 1 token/字，其余按 4 字符 1 token。
func estimateTokens(messages []any) int {
	total := 0
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		total += estimateText(flatten(m["content"]))
	}
	return total
}

func estimateText(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 { // CJK 及全角符号
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4 + 1
}

// compactMessages 返回压缩后的消息数组；第二个返回值表示是否真的压缩了。
func compactMessages(ctx context.Context, convID string, messages []any) ([]any, bool) {
	if compactThreshold <= 0 || len(messages) <= compactKeep+1 {
		return messages, false
	}
	if estimateTokens(messages) < compactThreshold {
		return messages, false
	}

	keep := compactKeep
	if keep > len(messages)-1 {
		keep = len(messages) - 1
	}
	older := messages[:len(messages)-keep]
	recent := messages[len(messages)-keep:]

	summary, err := summarize(ctx, older)
	if err != nil {
		log.Printf("!! 上下文压缩失败（继续用原文）: %v", err)
		return messages, false
	}
	if strings.TrimSpace(summary) == "" {
		return messages, false
	}

	// 摘要作为一条 user 消息放在最前面；若保留下来的第一条也是 user，则合并成一条，保证角色交替
	merged := []any{map[string]any{
		"role": "user",
		"content": "【以下是本次对话较早部分的摘要，供你继续对话时参考；细节可再询问用户】\n" +
			summary,
	}}
	if len(recent) > 0 {
		if m, ok := recent[0].(map[string]any); ok && m["role"] == "user" {
			if first, ok := merged[0].(map[string]any); ok {
				first["content"] = first["content"].(string) + "\n\n【随后用户继续提出】\n" + flatten(m["content"])
			}
			recent = recent[1:]
		}
	}
	merged = append(merged, recent...)

	saveCompaction(convID, estimateTokens(older), len(older), summary)
	log.Printf("  上下文压缩 conv=%s 早期 %d 条 → 摘要 %d 字，保留最近 %d 条",
		convID, len(older), len([]rune(summary)), len(recent))
	return merged, true
}

// summarize 调上游把一段对话压成结构化中文摘要。
func summarize(ctx context.Context, messages []any) (string, error) {
	var sb strings.Builder
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		text := flatten(m["content"])
		if strings.TrimSpace(text) == "" {
			continue
		}
		label := "用户"
		if role == "assistant" {
			label = "助手"
		}
		sb.WriteString(label)
		sb.WriteString("：")
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}

	prompt := "请把下面这段对话压缩成一份结构化摘要，供后续继续对话使用。要求：\n" +
		"1. 用中文；分点列出：对话主题、已达成的结论/产出、用户的关键偏好与约束、未完成的事项。\n" +
		"2. 保留必要的具体信息（名称、路径、数字、代码要点），不要臆造。\n" +
		"3. 控制在 600 字以内，不要寒暄。\n\n---\n对话原文：\n" + sb.String()

	body := map[string]any{
		"model":      env("CHAT_COMPACT_MODEL", env("DS_MODEL", "deepseek-flash")),
		"max_tokens": 1500,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": prompt,
		}},
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", apiBase+"/anthropic/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("摘要请求失败 HTTP %d: %s", resp.StatusCode, string(data[:min(len(data), 200)]))
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	var sb2 strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb2.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(sb2.String()), nil
}

func saveCompaction(convID string, tokens, count int, summary string) {
	if convID == "" {
		return
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	_, _ = db.Exec(`INSERT INTO compactions(conv_id, created_at, tokens_before, messages_before, summary)
		VALUES(?,?,?,?,?)`,
		convID, float64(time.Now().UnixNano())/1e9, tokens, count, summary)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
