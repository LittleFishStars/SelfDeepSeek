package main

// 跨会话数据共享：用户画像 + 会话摘要
//
// 目标：开新对话时把「用户是谁」和「上一轮聊了什么」带进去，让模型不用每次从零
// 认识用户；同时提供一个 history_search 工具让模型自己回查历史会话。
//
// 存储分三张表：
//   user_profile  每个用户一段画像（有长度上限，靠模型合并而不是无限追加）
//   conv_summary  每段会话一份摘要（只存摘要，不存原文；原文始终在 messages 里）
//   user_settings 每用户的开关（share_context）
//
// 成本控制：所有摘要类调用一律 thinking=disabled，且限制输入长度；导入整包时
// 批量处理并限制单次总量（CHAT_PROFILE_IMPORT_LIMIT），避免一次性打爆额度。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const profileSchema = `
CREATE TABLE IF NOT EXISTS user_profile (
  user_id     TEXT PRIMARY KEY,
  profile     TEXT NOT NULL DEFAULT '',
  updated_at  REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS conv_summary (
  conv_id       TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL,
  summary       TEXT NOT NULL DEFAULT '',
  msg_fingerprint TEXT NOT NULL DEFAULT '',
  created_at    REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_conv_summary_user ON conv_summary(user_id, created_at DESC);
CREATE TABLE IF NOT EXISTS user_settings (
  user_id       TEXT PRIMARY KEY,
  share_context INTEGER NOT NULL DEFAULT 0
);
`

var (
	// 画像长度上限（字符）。整包导入一次生成时信息量大，1800 常常装不下；
	// 3000 字≈3000 token，每次新对话注入一次、且带 cache_control，成本可以接受。
	profileMaxChars = envInt("CHAT_PROFILE_MAX_CHARS", 3000)
	// 单次总结允许的最大输入字符数（超长会话只取后半段，保证聊到最近的内容）
	summaryMaxInput = envInt("CHAT_SUMMARY_MAX_INPUT", 24000)
	// 导入汇总的上下文预算（token）。模型窗口 1M，留出输出与提示词的余量：
	// 这段对话总量没超预算就一次性生成画像，超了才均分成多段分别总结再汇总。
	profileContextTokens = envInt("CHAT_PROFILE_CONTEXT_TOKENS", 700000)
)

func initProfile() {
	if _, err := db.Exec(profileSchema); err != nil {
		log.Printf("!! 建画像表失败: %v", err)
	}
}

// ---------------------------------------------------------------- 开关

func shareContextEnabled(uid string) bool {
	var on int
	_ = db.QueryRow(`SELECT share_context FROM user_settings WHERE user_id=?`, uid).Scan(&on)
	return on == 1
}

func setShareContext(uid string, on bool) {
	v := 0
	if on {
		v = 1
	}
	_, _ = db.Exec(`INSERT INTO user_settings(user_id, share_context) VALUES(?,?)
		ON CONFLICT(user_id) DO UPDATE SET share_context=excluded.share_context`, uid, v)
}

// ---------------------------------------------------------------- 画像

func profileOf(uid string) (string, float64) {
	var text string
	var updated float64
	_ = db.QueryRow(`SELECT profile, updated_at FROM user_profile WHERE user_id=?`, uid).Scan(&text, &updated)
	return text, updated
}

// profileSections 是画像的四个固定小节，顺序即输出顺序。
var profileSections = []string{"个人信息", "喜好", "习惯", "其他"}

// normalizeProfileSections 保证画像一定是「四节、按固定顺序、不缺节」。
// 模型经常漏掉最后一节（尤其是「其他」），这里补「（暂无）」而不是让结构缺一块——
// 结构稳定才能保证注入时的前缀稳定，命中缓存。
func normalizeProfileSections(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	// 按 "## xxx" 切段，收集已存在的小节
	found := map[string]string{}
	var current string
	var buf []string
	flush := func() {
		if current != "" {
			found[current] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if current != "" {
			buf = append(buf, line)
		}
	}
	flush()

	// 一节都没有：不是规范输出，原样返回，避免把模型的一段话压成空
	if len(found) == 0 {
		return text
	}

	var sb strings.Builder
	for _, name := range profileSections {
		body := strings.TrimSpace(found[name])
		if body == "" {
			body = "（暂无）"
		}
		sb.WriteString("## ")
		sb.WriteString(name)
		sb.WriteString("\n\n")
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

func saveProfile(uid, text string) {
	text = normalizeProfileSections(text)
	if len([]rune(text)) > profileMaxChars {
		r := []rune(text)
		text = string(r[:profileMaxChars])
	}
	_, _ = db.Exec(`INSERT INTO user_profile(user_id, profile, updated_at) VALUES(?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET profile=excluded.profile, updated_at=excluded.updated_at`,
		uid, text, float64(time.Now().UnixNano())/1e9)
}

// ---------------------------------------------------------------- 会话摘要

// convFingerprint 用消息条数 + 最后一条消息 id 标识「这段会话有没有变化」，
// 避免同一条会话被反复总结。
func convFingerprint(convID string) string {
	var n int
	var lastID int64
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id),0) FROM messages WHERE conv_id=?`, convID).Scan(&n, &lastID)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", convID, n, lastID)))
	return hex.EncodeToString(h[:8])
}

func summaryOf(convID string) (string, string) {
	var text, fp string
	_ = db.QueryRow(`SELECT summary, msg_fingerprint FROM conv_summary WHERE conv_id=?`, convID).Scan(&text, &fp)
	return text, fp
}

func saveSummary(convID, uid, text, fp string) {
	_, _ = db.Exec(`INSERT INTO conv_summary(conv_id, user_id, summary, msg_fingerprint, created_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(conv_id) DO UPDATE SET summary=excluded.summary,
			msg_fingerprint=excluded.msg_fingerprint, created_at=excluded.created_at`,
		convID, uid, text, fp, float64(time.Now().UnixNano())/1e9)
}

// latestOtherSummary 找用户最近一段「不是当前会话」的摘要，作为「上次对话」。
func latestOtherSummary(uid, exceptConvID string) string {
	var text string
	err := db.QueryRow(`SELECT s.summary FROM conv_summary s
		WHERE s.user_id=? AND s.conv_id<>? AND s.summary<>''
		ORDER BY s.created_at DESC LIMIT 1`, uid, exceptConvID).Scan(&text)
	if err != nil {
		return ""
	}
	return text
}

// ---------------------------------------------------------------- 总结

// conversationTranscript 把一段会话拼成「用户/助手」纯文本，供总结用。
// 超长时保留后段（越近越重要），并在开头标注省略。
func conversationTranscript(convID string, maxChars int) (string, int) {
	nodes := activePath(convID)
	var sb strings.Builder
	msgs := 0
	for _, n := range nodes {
		text := strings.TrimSpace(n.Content)
		if text == "" {
			continue
		}
		label := "用户"
		if n.Role == "assistant" {
			label = "助手"
		}
		sb.WriteString(label)
		sb.WriteString("：")
		sb.WriteString(text)
		sb.WriteString("\n\n")
		msgs++
	}
	out := sb.String()
	if rs := []rune(out); maxChars > 0 && len(rs) > maxChars {
		out = "（前段省略）\n\n" + string(rs[len(rs)-maxChars:])
	}
	return out, msgs
}

// profileShape 是画像的统一版式：四个固定小节，空的一节写「（暂无）」而不是省略，
// 这样无论是「增量合并」还是「整包导入」产出的画像结构都一致——
// 注入时前缀更稳定，命中缓存的比例也就更高。
//
// 每一节都写清楚「收什么、怎么写、什么不要」，因为模型最容易犯三种错：
// 写成空泛的形容词（“用户喜欢技术”）、把助手自己的行为记成用户特征、
// 以及把一次性的临时需求写成长期偏好。
const profileShape = `画像固定分四节，标题与顺序都不能变；某一节确实没有可靠内容时，写「（暂无）」，不要为了凑满而编造。

## 个人信息
收：能确定身份的客观事实——所在地或时区；学校、专业、年级、班级这类学业信息；职业、岗位、公司类型；年龄段；以及常用设备与环境（手机型号、操作系统、常用编辑器或客户端、自建服务的规模等）。
怎么写：写成名词短语，能具体就具体（“数学与应用数学专业、2025 级”好于“学生”）。不确定的一律不写，不要用“可能”“大概”。
不要：不要写联系方式、真实姓名、学号这类敏感信息（对话里出现也不要抄进画像）。

## 喜好
收三类：
1) 兴趣与关注方向，尽量到分支（“数学分析里的含参量积分与一致收敛”好于“喜欢数学”）；
2) 技术栈与工具偏好（语言、框架、编辑器、命令行工具、习惯用的库与服务）；
3) 对回答风格的偏好——要不要先给结论、要不要推导过程、要不要代码示例、篇幅偏长还是偏短、要不要分条或表格、要不要标注出处、语气正式还是随意。
同时记下明确表示排斥的东西（“不喜欢到处画框的界面”“不要寒暄”这类原话级要求，用引号保留关键措辞）。
不要：不要把你（助手）的做法当成用户喜好；不要写“用户喜欢高质量内容”这种没有信息量的话。

## 习惯
收：
1) 提问与协作方式——一次抛多个问题、先描述现象再提问、喜欢追问细节、习惯让我先复述需求再动手、会不会要求先备份；
2) 格式与篇幅要求——分条、表格、字数上限、公式要不要写推导、代码要不要注释、术语用中文还是英文；
3) 工作节奏与流程——先看现状再改、改完要提交、要清理临时文件、要一次做完多件事这类稳定要求；
4) 反复出现的口头要求（如“别猜，先查”“按你自己的判断来”），用引号保留原话。

## 其他
收：长期在做的项目与目标（带具体名称）；重要约束与前提（预算敏感、网络受限、设备或环境限制、必须遵守的规范）；反复提到但尚未收尾的事项；需要特别避开的禁忌与雷区。
不要：不要把已完成的琐事、一次性的问题清单塞进来；不要写“用户还有其他未提及的需求”这类空话。`

// 画像取样：写画像只需要「用户关注什么、怎么要求」，不需要完整的技术长文。
// 用户消息留头部（提问与要求通常在最前面），助手回复只留开头（够判断话题即可）。
// 这样同样的预算能覆盖更多会话，也不会被粘贴的代码/数据把上下文吃满。
const (
	profileUserHead   = 1000 // 每条用户消息最多保留多少字符
	profileAssistHead = 200  // 每条助手回复最多保留多少字符
)

func headRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…（后文略）"
}

// conversationTranscriptForProfile 是画像专用的会话拼装：按上面的规则压缩，
// 与给模型看的对话摘要（conversationTranscript）分开，避免互相牵连。
func conversationTranscriptForProfile(convID string) (string, int) {
	nodes := activePath(convID)
	var sb strings.Builder
	msgs := 0
	for _, n := range nodes {
		text := strings.TrimSpace(n.Content)
		if text == "" {
			continue
		}
		if n.Role == "assistant" {
			text = headRunes(text, profileAssistHead)
			sb.WriteString("助手：")
		} else {
			text = headRunes(text, profileUserHead)
			sb.WriteString("用户：")
		}
		sb.WriteString(text)
		sb.WriteString("\n\n")
		msgs++
	}
	return sb.String(), msgs
}

// summarizeConversation 用模型把一段会话压成条目化摘要（思考关闭，省 token）。
func summarizeConversation(ctx context.Context, convID string) (string, bool) {
	transcript, msgs := conversationTranscript(convID, summaryMaxInput)
	if msgs == 0 || strings.TrimSpace(transcript) == "" {
		return "", false
	}
	prompt := "把下面这段对话浓缩成一份中文摘要，供以后回忆上下文用。要求：\n" +
		"1. 分条列出：主题、结论或产出、用户明确的偏好与约束、未完成事项。\n" +
		"2. 保留具体信息（名称、路径、数字、结论），不要臆造，不要寒暄。\n" +
		"3. 400 字以内。\n\n---\n对话原文：\n" + transcript
	out, err := callModelText(ctx, prompt, 900)
	if err != nil {
		log.Printf("!! 总结会话失败 conv=%s: %v", short(convID), err)
		return "", false
	}
	return strings.TrimSpace(out), len([]rune(strings.TrimSpace(out))) > 0
}

// mergeIntoProfile 把新摘要并进画像；超长时让模型压缩自己，而不是简单截断。
func mergeIntoProfile(ctx context.Context, uid, summary string) {
	old, _ := profileOf(uid)
	var prompt string
	if strings.TrimSpace(old) == "" {
		prompt = "根据下面的对话摘要，写一份「用户画像」，供以后对话时参考。\n" + profileShape +
			fmt.Sprintf("\n用第三人称、条目化、中文，每条一句话，全文不超过 %d 字。只写有依据的内容，不要臆造、不要寒暄。", profileMaxChars) +
			"\n\n---\n对话摘要：\n" + summary
	} else {
		prompt = "下面是一份已有的「用户画像」和一段新的对话摘要。请把新信息合并进画像：\n" +
			"1. 保留仍然有效的旧信息，补充新信息；两者冲突时以新的为准。\n" +
			"2. 去掉一次性的、已经过时的细节；同一个事实只写一次，不要重复。\n" +
			"3. 保持下面这套四节结构，不要增删小节。" +
			fmt.Sprintf("全文不超过 %d 字。\n\n", profileMaxChars) + profileShape +
			"\n---\n已有画像：\n" + old + "\n\n---\n新的对话摘要：\n" + summary
	}
	out, err := callModelText(ctx, prompt, 1200)
	if err != nil {
		log.Printf("!! 合并画像失败 uid=%s: %v", short(uid), err)
		return
	}
	if s := strings.TrimSpace(out); s != "" {
		saveProfile(uid, s)
	}
}

// summarizeAndMerge 总结某段会话并合并进画像；已总结过且未变化时直接跳过。
// 返回是否真的做了工作（调用方据此判断要不要继续）。
func summarizeAndMerge(ctx context.Context, uid, convID string) bool {
	if uid == "" || convID == "" {
		return false
	}
	fp := convFingerprint(convID)
	if old, oldFP := summaryOf(convID); old != "" && oldFP == fp {
		return false
	}
	summary, ok := summarizeConversation(ctx, convID)
	if !ok {
		return false
	}
	saveSummary(convID, uid, summary, fp)
	mergeIntoProfile(ctx, uid, summary)
	log.Printf("  画像已更新 uid=%s conv=%s 摘要 %d 字", short(uid), short(convID), len([]rune(summary)))
	return true
}

// ---------------------------------------------------------------- 成本控制：模型调用

// callModelText 走上游 Anthropic 兼容接口取一段纯文本，思考显式关闭。
func callModelText(ctx context.Context, prompt string, maxTokens int) (string, error) {
	body := map[string]any{
		"model":      env("CHAT_PROFILE_MODEL", env("DS_MODEL", "deepseek-flash")),
		"max_tokens": maxTokens,
		// 摘要/合并这类活不需要思考链，关掉省 token
		"thinking": map[string]any{"type": "disabled"},
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	}
	raw, _ := json.Marshal(body)
	data, err := upstreamJSON(ctx, "/anthropic/v1/messages", raw)
	if err != nil {
		return "", err
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
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------- 导入后的批量汇总

type profileJobState struct {
	Running bool    `json:"running"`
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	Merged  int     `json:"merged"`
	Started float64 `json:"started_at"`
	Ended   float64 `json:"ended_at"`
	Error   string  `json:"error,omitempty"`
}

var (
	profileJobsMu sync.Mutex
	profileJobs   = map[string]*profileJobState{}
)

func profileJobStatus(uid string) profileJobState {
	profileJobsMu.Lock()
	defer profileJobsMu.Unlock()
	if j, ok := profileJobs[uid]; ok {
		return *j
	}
	return profileJobState{}
}

func setProfileJob(uid string, fn func(*profileJobState)) {
	profileJobsMu.Lock()
	defer profileJobsMu.Unlock()
	j, ok := profileJobs[uid]
	if !ok {
		j = &profileJobState{}
		profileJobs[uid] = j
	}
	fn(j)
}

// ---------------------------------------------------------------- 导入汇总

// convBlock 是一段会话的完整文本，导入汇总的输入单元。
type convBlock struct {
	title     string
	updatedAt float64
	text      string
	tokens    int
}

// collectConvBlocks 读出这批会话的正文（不限长，超限的分段在后面做）。
func collectConvBlocks(convIDs []string) []convBlock {
	blocks := make([]convBlock, 0, len(convIDs))
	for _, id := range convIDs {
		text, msgs := conversationTranscriptForProfile(id)
		if msgs == 0 || strings.TrimSpace(text) == "" {
			continue
		}
		var title string
		var updated float64
		_ = db.QueryRow(`SELECT COALESCE(title,''), COALESCE(updated_at,0) FROM conversations WHERE id=?`, id).
			Scan(&title, &updated)
		blocks = append(blocks, convBlock{
			title:     title,
			updatedAt: updated,
			text:      text,
			tokens:    estimateText(text),
		})
	}
	return blocks
}

func renderConvBlock(b convBlock) string {
	head := "### "
	if b.title != "" {
		head += b.title
	} else {
		head += "（无标题会话）"
	}
	if b.updatedAt > 0 {
		head += " · " + time.Unix(int64(b.updatedAt), 0).Format("2006-01-02")
	}
	return head + "\n" + b.text + "\n"
}

// splitBlocksEvenly 把会话按顺序均分成 n 组（尽量让每段体量接近，且不拆断单段会话）。
// n 由总量算出：n = ceil(总 token / 预算)。
func splitBlocksEvenly(blocks []convBlock, groups int) [][]convBlock {
	if groups <= 1 || len(blocks) == 0 {
		return [][]convBlock{blocks}
	}
	total := 0
	for _, b := range blocks {
		total += b.tokens
	}
	target := total / groups
	if target <= 0 {
		target = 1
	}
	out := make([][]convBlock, 0, groups)
	cur := make([]convBlock, 0, len(blocks))
	curTokens := 0
	for i, b := range blocks {
		remainingGroups := groups - len(out)
		remainingBlocks := len(blocks) - i
		// 剩下的会话数刚好够每组一个时，先收口，避免最后一组为空
		if remainingBlocks <= remainingGroups-1 && len(cur) > 0 {
			out = append(out, cur)
			cur = nil
			curTokens = 0
			continue
		}
		cur = append(cur, b)
		curTokens += b.tokens
		if curTokens >= target && len(out) < groups-1 {
			out = append(out, cur)
			cur = nil
			curTokens = 0
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// importSegmentPromptHead 用于「对话太多、分成多段」时的逐段提炼：
// 这里只收要点，不要求分节，最终由汇总那一步统一成四节画像。
const importSegmentPromptHead = "下面是一位用户与助手的若干段历史对话（### 开头是会话标题与日期）。\n" +
	"任务：提炼其中关于「用户本人」的稳定信息，作为后面写用户画像的素材。按四类归拢：\n\n" +
	"个人信息：身份、所在地、学校/专业/年级、职业岗位、年龄段、常用设备与使用环境。\n" +
	"喜好：关注的领域（尽量到具体分支）、技术栈与工具偏好、对回答方式的偏好（详略、格式、要不要推导与出处）、明确表示不喜欢的东西。\n" +
	"习惯：提问与协作的方式、对格式与篇幅的要求、工作节奏与固定流程（先备份、改完提交、清理临时文件等）、反复出现的口头要求。\n" +
	"其他：长期项目与目标、重要约束（预算、网络、设备、必须遵守的规范）、尚未收尾的事项、需要避开的禁忌。\n\n" +
	"写法要求：\n" +
	"1. 每条一句话，能具体就具体，带专有名词、数字、版本、路径；\n" +
	"2. 用户的原话要求保留关键措辞（加引号），不要改写成泛泛的描述；\n" +
	"3. 只写对话里确有依据的内容，不确定的不写；不要写助手自己的行为；\n" +
	"4. 某一类没有内容就跳过这一类，不要凑；\n" +
	"5. 中文、条目化，直接给要点，不要开场白。\n\n---\n"

// importProfilePromptHead 用于「总量在预算内、一次性出画像」。
const importProfilePromptHead = "下面是一位用户与助手的历史对话记录（### 开头是会话标题与日期）。\n" +
	"请据此写出这个用户的画像，供以后对话时参考。\n" + profileShape +
	"\n用第三人称、条目化、中文，每条一句话；只写有依据的内容，不要臆造、不要寒暄。"

// startImportProfileJob 在导入结束后异步生成用户画像。
//
// 策略（按总量决定，不按条数）：本次导入的对话全文若在上下文预算内，
// 就一次性喂给模型直接生成画像；超了才把会话均分成若干段分别提炼要点，
// 再把要点汇总成最终画像。这样常见规模只花一次调用。
func startImportProfileJob(uid string, convIDs []string) {
	if uid == "" || len(convIDs) == 0 {
		return
	}
	setProfileJob(uid, func(j *profileJobState) {
		if j.Running {
			return // 已有任务在跑，避免重复扣费
		}
		j.Running = true
		j.Done = 0
		j.Total = 0
		j.Merged = 0
		j.Started = float64(time.Now().Unix())
		j.Ended = 0
		j.Error = ""
	})
	if !profileJobStatus(uid).Running {
		return
	}

	go func() {
		defer setProfileJob(uid, func(j *profileJobState) {
			j.Running = false
			j.Ended = float64(time.Now().Unix())
		})

		blocks := collectConvBlocks(append([]string(nil), convIDs...))
		if len(blocks) == 0 {
			log.Printf("  导入汇总：没有可用正文 uid=%s", short(uid))
			return
		}
		total := 0
		for _, b := range blocks {
			total += b.tokens
		}
		groups := 1
		if profileContextTokens > 0 {
			groups = (total + profileContextTokens - 1) / profileContextTokens
			if groups < 1 {
				groups = 1
			}
		}
		split := splitBlocksEvenly(blocks, groups)
		setProfileJob(uid, func(j *profileJobState) { j.Total = len(split) })
		log.Printf("  导入汇总 uid=%s 会话=%d token≈%d 段数=%d", short(uid), len(blocks), total, len(split))

		summaries := make([]string, 0, len(split))
		for i, group := range split {
			var sb strings.Builder
			for _, b := range group {
				sb.WriteString(renderConvBlock(b))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
			var text string
			var err error
			if len(split) == 1 {
				text, err = callModelText(ctx, importProfilePromptHead+"\n\n"+sb.String(), 2000)
			} else {
				text, err = callModelText(ctx, importSegmentPromptHead+sb.String(), 1200)
			}
			cancel()
			if err != nil {
				log.Printf("!! 导入汇总第 %d/%d 段失败: %v", i+1, len(split), err)
				setProfileJob(uid, func(j *profileJobState) { j.Done++ })
				continue
			}
			summaries = append(summaries, strings.TrimSpace(text))
			setProfileJob(uid, func(j *profileJobState) { j.Done++ })
		}
		if len(summaries) == 0 {
			return
		}

		var profile string
		if len(split) == 1 {
			profile = summaries[0]
		} else {
			// 多段：把各段要点再汇总成一份画像
			var sb strings.Builder
			sb.WriteString("下面是一位用户多段历史对话的要点。请合并成一份最终的用户画像：\n")
			fmt.Fprintf(&sb, "1. 合并重复信息，冲突时以更具体的为准；2. 控制在 %d 字以内；\n", profileMaxChars)
			sb.WriteString("3. 第三人称、条目化、中文，只写有依据的内容；\n4. 保持下面这套四节结构。\n\n")
			sb.WriteString(profileShape)
			sb.WriteString("\n---\n")
			for i, item := range summaries {
				fmt.Fprintf(&sb, "\n第 %d 段要点：\n%s\n", i+1, item)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
			merged, err := callModelText(ctx, sb.String(), 2000)
			cancel()
			if err != nil {
				log.Printf("!! 导入汇总最终合并失败: %v", err)
				// 合并失败也别丢：用最长的一段要点兜底
				for _, item := range summaries {
					if len([]rune(item)) > len([]rune(profile)) {
						profile = item
					}
				}
			} else {
				profile = strings.TrimSpace(merged)
			}
		}
		profile = strings.TrimSpace(profile)
		if profile == "" {
			return
		}
		// 超出画像上限就压缩：模型常常不严格执行字数，压两轮还超才截断兜底
		for attempt := 0; attempt < 2 && len([]rune(profile)) > profileMaxChars; attempt++ {
			cur := len([]rune(profile))
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			short, err := callModelText(ctx, fmt.Sprintf(
				"必须把下面这份用户画像压缩到 %d 字以内（现在约 %d 字，超了）。"+
					"做法：合并同类项、删掉重复与修饰语、只保留最关键的事实与偏好；"+
					"保持四节结构不变，条目化、中文、第三人称，直接给结果不要解释。\n\n---\n%s",
				profileMaxChars, cur, profile), 1600)
			cancel()
			if err != nil {
				break
			}
			if next := strings.TrimSpace(short); next != "" && len([]rune(next)) < cur {
				profile = next
			} else {
				break
			}
		}
		saveProfile(uid, profile)
		setProfileJob(uid, func(j *profileJobState) { j.Merged = 1 })
		log.Printf("  导入汇总完成 uid=%s 画像 %d 字（%d 段）", short(uid), len([]rune(profile)), len(split))
	}()
}

// recentConvIDs 取用户最近的若干会话 id（重建画像时用）。
func recentConvIDs(uid string, limit int) []string {
	rows, err := db.Query(`SELECT id FROM conversations WHERE user_id=? ORDER BY updated_at DESC LIMIT ?`, uid, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return i < j })
	return out
}

// upstreamJSON 直接打上游 Anthropic 兼容接口，返回原始响应体。
// 和 compact.go 里的内联调用等价，集中在这里供画像相关逻辑复用。
func upstreamJSON(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data[:min(len(data), 200)]))
	}
	return data, nil
}

// ---------------------------------------------------------------- HTTP

// handleProfile 处理跨会话共享的设置与状态：
//
//	GET  /v1/profile          → {enabled, profile, updated_at, job}
//	POST /v1/profile/switch   → {enabled: bool}
func handleProfile(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/profile")

	switch {
	case path == "/switch" && r.Method == http.MethodPost:
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if !readJSON(r, &in) {
			writeJSON(w, 400, map[string]any{"error": "需要 enabled"})
			return
		}
		setShareContext(uid, in.Enabled)
		writeJSON(w, 200, map[string]any{"ok": true, "enabled": in.Enabled})

	case (path == "" || path == "/") && r.Method == http.MethodGet:
		text, updated := profileOf(uid)
		writeJSON(w, 200, map[string]any{
			"enabled":    shareContextEnabled(uid),
			"profile":    text,
			"updated_at": updated,
			"job":        profileJobStatus(uid),
		})

	case path == "/status" && r.Method == http.MethodGet:
		writeJSON(w, 200, profileJobStatus(uid))

	// 重建画像：拿最近 N 段会话按当前策略重新生成一次（换策略或画像跑偏时用）
	case path == "/rebuild" && r.Method == http.MethodPost:
		limit := envInt("CHAT_PROFILE_REBUILD_LIMIT", 50)
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		ids := recentConvIDs(uid, limit)
		if len(ids) == 0 {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "没有可用的历史会话"})
			return
		}
		startImportProfileJob(uid, ids)
		writeJSON(w, 200, map[string]any{"ok": true, "conversations": len(ids), "job": profileJobStatus(uid)})

	default:
		writeJSON(w, 404, map[string]any{"error": "not found"})
	}
}

// ---------------------------------------------------------------- 注入

// sharedContextHeader / lastConvHeader 是注入块的固定开头。
// 保持字面稳定：DeepSeek 按前缀命中缓存，头部措辞一变就整段失效。
const (
	sharedContextHeader = "【跨会话共享 · 用户画像】\n以下是你此前为该用户积累的画像，用于延续上下文；与当前对话冲突时以当前对话为准。\n\n"
	lastConvHeader      = "【跨会话共享 · 上一次对话摘要】\n以下摘要来自该用户最近一次其他对话，仅作背景参考，不要直接把它当成当前话题。\n\n"
	sharedContextFooter = "\n\n需要更早的细节时，可以调用 history_search 工具检索该用户的历史会话。"
)

// injectSharedContext 在开关开启时把画像与上次对话摘要写进 system。
//
// 缓存友好排序：原来的 system 提示词最稳定放最前，画像次之（每次总结后才变），
// 上次对话摘要最容易变放最后；这样同一用户连续追问时前缀命中率最高。
// 只在「新会话第一轮」注入，避免每轮都改 system 把缓存全打掉。
func injectSharedContext(uid, convID string, reqObj map[string]any) bool {
	if !shareContextEnabled(uid) {
		return false
	}
	profile, _ := profileOf(uid)
	last := latestOtherSummary(uid, convID)
	if strings.TrimSpace(profile) == "" && strings.TrimSpace(last) == "" {
		return false
	}

	var blocks []any
	// 1) 原 system（最稳定）
	switch v := reqObj["system"].(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": v})
		}
	case []any:
		blocks = append(blocks, v...)
	}
	// 2) 画像：变化慢，标缓存点
	if strings.TrimSpace(profile) != "" {
		blocks = append(blocks, map[string]any{
			"type":          "text",
			"text":          sharedContextHeader + profile,
			"cache_control": map[string]any{"type": "ephemeral"},
		})
	}
	// 3) 上次对话摘要：每次新会话都可能变
	if strings.TrimSpace(last) != "" {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": lastConvHeader + last + sharedContextFooter,
		})
	}
	if len(blocks) == 0 {
		return false
	}
	reqObj["system"] = blocks
	return true
}

// countConvMessages 数一段会话现有的消息条数（用于判断是不是新会话）。
func countConvMessages(convID string) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv_id=?`, convID).Scan(&n)
	return n
}

// previousConversation 返回该用户最近一段「不是当前会话」且有消息的会话 id。
func previousConversation(uid, exceptConvID string) string {
	var id string
	err := db.QueryRow(`SELECT c.id FROM conversations c
		WHERE c.user_id=? AND c.id<>? AND EXISTS (SELECT 1 FROM messages m WHERE m.conv_id=c.id)
		ORDER BY c.updated_at DESC LIMIT 1`, uid, exceptConvID).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// autoSummarizePrevious 在开新对话后异步总结「上一段对话」并并入画像。
// 关掉思考、限制输入长度，单次成本很小；CHAT_PROFILE_AUTO_SUMMARIZE=0 可整体关闭。
func autoSummarizePrevious(uid, convID string) {
	if env("CHAT_PROFILE_AUTO_SUMMARIZE", "1") == "0" {
		return
	}
	prev := previousConversation(uid, convID)
	if prev == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		summarizeAndMerge(ctx, uid, prev)
	}()
}
