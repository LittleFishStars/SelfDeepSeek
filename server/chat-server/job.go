package main

// 服务端主导的流式任务：上游请求由服务器发起并完整读完，
// 客户端连接断开不影响生成；重连时用 X-Resume: <jobId> 从断点续传。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type streamJob struct {
	mu     sync.Mutex
	events []string
	done   bool
	err    string
	// delivered 表示至少有一个客户端完整收到过本轮回答（含正常结束）
	delivered bool
	uid       string
	subs      map[chan struct{}]struct{}
	// fileIDs 是本轮请求里引用的上游 file_id（图片块），
	// 收到「图片超限」时用来构造前端可自愈的「file_id 失效」错误（见 appendUpstreamError）。
	fileIDs []string
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*streamJob{}
)

func jobKey(convID, body string) string {
	if convID == "" {
		convID = "anon"
	}
	sum := hashToken(body)
	return convID + ":" + sum[:16]
}

func newJob(uid string) *streamJob {
	return &streamJob{subs: map[chan struct{}]struct{}{}, uid: uid}
}

// append 追加一条事件并唤醒所有订阅者
// appendSSE 追加一条完整的 SSE 事件（event 行 + data 行）。
// 服务端自己合成的块（历史检索）用它写进同一条流里，客户端照常解析。
func (j *streamJob) appendSSE(event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	j.append("event: " + event)
	j.append("data: " + string(raw))
	// SSE 事件以空行结束；少了这一行，客户端会把下一个事件拼进来一起丢掉
	j.append("")
}

func (j *streamJob) append(line string) {
	j.mu.Lock()
	j.events = append(j.events, line)
	subs := make([]chan struct{}, 0, len(j.subs))
	for ch := range j.subs {
		subs = append(subs, ch)
	}
	j.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (j *streamJob) finish(errMsg string) {
	j.mu.Lock()
	j.done = true
	j.err = errMsg
	subs := make([]chan struct{}, 0, len(j.subs))
	for ch := range j.subs {
		subs = append(subs, ch)
	}
	j.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (j *streamJob) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	j.mu.Lock()
	j.subs[ch] = struct{}{}
	j.mu.Unlock()
	return ch, func() {
		j.mu.Lock()
		delete(j.subs, ch)
		j.mu.Unlock()
	}
}

func (j *streamJob) snapshot() ([]string, bool, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.events))
	copy(out, j.events)
	return out, j.done, j.err
}

// runJob 在后台跑完整个上游请求，事件进 job，结束后落库。
// 上游偶发「搜索后直接断流」时，在推送前就重试（客户端看不到失败的那次）；
// 重试会去掉联网搜索工具。
func runJob(j *streamJob, req *http.Request, body []byte, convID string) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("!! 任务 panic: %v", rec)
			j.finish("internal error")
		}
	}()

	// 记录本轮引用的图片 file_id：收到「图片超限」时用它们构造
	// 前端可自愈的「file_id 失效」错误，触发重新上传（走切分链路）。
	{
		var probe map[string]any
		if err := json.Unmarshal(body, &probe); err == nil {
			if msgs, ok := probe["messages"].([]any); ok {
				j.fileIDs = collectImageFileIDs(msgs)
			}
		}
	}

	bodyForAttempt := body
	// 历史检索的工具循环：模型调 history_search 时，服务端自己执行并把结果回灌，
	// 再请求一次，直到它不再调工具（不设业务预算，只留 toolRoundHardLimit 硬上限）。
	// 合成块的索引从 1000 起：工具轮次之后的正文块又从 index 0 开始，
	// 用低位会和正文撞车，客户端按 index 合并时会把检索块覆盖掉。
	blockBase := 1000
	// 工具轮次不设业务预算：run_rhai 脚本出错后模型改脚本重试是正常行为，
	// 写多少轮由模型自己决定，服务端只负责执行并回灌结果。
	// 只有一个很宽松的硬上限 toolRoundHardLimit 防跑飞；真到了上限也是
	// 优雅降级——注入提示 + 去掉服务端工具让模型直接回答，绝不发 error
	// 把输出中断掉。断流重试（上游无 message_stop 且无正文）另算，
	// 最多 abortRetryLimit 次，超过才发 error 兜底。
	toolRounds := 0
	abortRetries := 0
	for round := 0; round < toolRoundHardLimit+abortRetryLimit; round++ {
		textLen, stopped, aborted, calls, rejected := runOnce(j, req, bodyForAttempt, convID, round > 0)
		if rejected != "" {
			// 上游明确拒绝：重试无意义，直接把真实原因作为 error 事件下发，
			// 客户端会原样显示这句话，用户能看到「图片单边超过 8192 像素」这类具体原因。
			j.appendUpstreamError(rejected)
			log.Printf("  上游拒绝，已透传原因 conv=%s", short(convID))
			j.finish("upstream rejected")
			return
		}
		if stopped && len(calls) > 0 {
			if round == 0 {
				// 工具轮次的原始事件不转发，客户端就收不到 message_start；
				// 没有它客户端不会初始化这条回复，后面的检索块会全部被丢掉。
				j.appendSSE("message_start", map[string]any{
					"type": "message_start",
					"message": map[string]any{
						"id":      "msg_hist_" + convID[:min(8, len(convID))],
						"type":    "message",
						"role":    "assistant",
						"model":   "deepseek-flash",
						"content": []any{},
						"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
					},
				})
			}
			results := make([]string, len(calls))
			// 按原顺序回填：tool_result 必须和 tool_use 一一对应
			byID := map[string]string{}
			history, rhai := splitToolCalls(calls)
			if len(history) > 0 {
				texts, hits := handleHistoryToolRound(j.uid, convID, history)
				appendHistoryEvents(j, history, hits, blockBase)
				blockBase += len(history) * 2
				for i, call := range history {
					byID[call.ID] = texts[i]
				}
			}
			if len(rhai) > 0 {
				texts, blocks := handleRhaiToolRound(convID, rhai)
				for i, call := range rhai {
					byID[call.ID] = texts[i]
					appendRhaiEvents(j, call, blocks[i], blockBase)
					blockBase += 2
				}
			}
			for i, call := range calls {
				if text, ok := byID[call.ID]; ok {
					results[i] = text
				} else {
					results[i] = "未知工具，没有执行。"
				}
			}
			bodyForAttempt = buildToolRound(bodyForAttempt, calls, results, "")
			toolRounds++
			if toolRounds >= toolRoundHardLimit {
				// 触到硬上限：本轮的错误信息已回灌，优雅降级为直接回答，
				// 绝不发 error 中断输出。去掉服务端工具，并合并一条明确提示
				// （见 appendNoToolNotice），防止模型把工具调用标签当正文吐出。
				bodyForAttempt = appendNoToolNotice(bodyForAttempt)
				bodyForAttempt = stripServerTools(bodyForAttempt)
				log.Printf("  工具轮次触到硬上限 %d，去掉服务端工具强制直接回答 conv=%s", toolRoundHardLimit, short(convID))
			} else {
				log.Printf("  工具轮次 %d conv=%s 调用=%d（检索 %d，脚本 %d）",
					toolRounds, short(convID), len(calls), len(history), len(rhai))
			}
			continue
		}
		if stopped || textLen > 0 {
			j.cacheIfUndelivered(req, convID)
			j.finish("")
			return
		}
		if !aborted {
			j.finish("")
			return
		}
		// 断流重试：换掉联网搜索工具，减少再次触发同一问题
		abortRetries++
		if abortRetries > abortRetryLimit {
			break
		}
		bodyForAttempt = stripWebSearch(bodyForAttempt)
		log.Printf("  上游断流且无正文，重试 conv=%s attempt=%d", short(convID), abortRetries)
	}
	j.append(`data: {"type":"error","error":{"type":"server_error","message":"上游生成中断，请重试"}}`)
	j.append("")
	j.finish("upstream aborted")
}

// runOnce 跑一次上游请求。返回值：正文长度、是否收到 message_stop、是否异常中断、
// 工具调用、上游拒绝说明。
// 第五个返回值非空表示上游**明确拒绝**（4xx，除 408/429）：重试也不会成功，
// 必须把真实原因报给客户端，不能当成断流去重试（2026-11-22 踩过：一张
// 1095x21555 的长截图超了上游「单边最长 8192 像素」，返回 400，
// 却被当断流重试 3 次，最后显示「上游生成中断，请重试」，用户完全不知道原因）。
// 事件先写进本地 pending，确认有正文后再一次性发布，失败的那次客户端看不到。
func runOnce(j *streamJob, req *http.Request, body []byte, convID string, followUp bool) (int, bool, bool, []toolCall, string) {
	ctx, cancel := context.WithTimeout(context.Background(), reqTimeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL.String(), strings.NewReader(string(body)))
	if err != nil {
		return 0, false, true, nil, ""
	}
	out.Header = req.Header.Clone()
	resp, err := upstreamClient.Do(out)
	if err != nil {
		log.Printf("!! 上游请求失败 conv=%s: %v", short(convID), err)
		return 0, false, true, nil, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg := upstreamErrorMessage(resp)
		log.Printf("!! 上游状态码 conv=%s: %d %s", short(convID), resp.StatusCode, truncateText(msg, 300))
		if retryableStatus(resp.StatusCode) {
			return 0, false, true, nil, ""
		}
		return 0, false, true, nil, msg
	}

	var text, thinking strings.Builder
	var meta streamMeta
	blockAcc := newBlockCollector()
	toolCalls := newToolCallCollector()
	var event string
	var pending []string
	published := false
	sawStop := false
	// 首轮一旦出现 tool_use，这一轮就是「工具轮」：服务端会用检索结果合成完整的
	// 块序列，原始 tool_use 与 message_stop(stop_reason=tool_use) 一律不外发 ——
	// 否则客户端把工具轮当成一次被中断的回答（显示「已停止」），紧接着补发的
	// 合成 message_start 又让它整段重来。
	skipRest := false

	// emit 把缓冲一次性倒出，之后进入实时模式
	emit := func() {
		if published {
			return
		}
		published = true
		for _, l := range pending {
			j.append(l)
		}
		pending = nil
	}

	// publish：正文出现前先攒着，避免「上游空流」时客户端看到半截就干等；
	// 但攒的条件不能太宽（见下面 thinking 的提前 emit）。
	publish := func(line string) {
		if !published && (text.Len() > 0 || thinking.Len() > 0) {
			emit()
		}
		if published {
			j.append(line)
			return
		}
		pending = append(pending, line)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	// 工具轮次的后续请求：跳过 message_start（含它的 data 行），
	// 客户端看到新的 message_start 会当作新消息，把已渲染的检索块丢掉。
	skipNextData := false
	for sc.Scan() {
		line := sc.Text()
		if followUp {
			if strings.HasPrefix(line, "event: message_start") {
				skipNextData = true
				continue
			}
			if skipNextData {
				skipNextData = false
				continue
			}
		}
		if !skipRest && !followUp && strings.HasPrefix(line, "data: ") &&
			strings.Contains(line, `"content_block_start"`) && strings.Contains(line, `"tool_use"`) {
			skipRest = true
		}
		if !skipRest {
			publish(line)
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if strings.Contains(payload, `"type":"message_stop"`) {
				sawStop = true
			}
			collect(payload, event, &text, &thinking, &meta, blockAcc)
			toolCalls.feed(payload)
			// 首轮的思考也要实时外发：原来要等正文出现才 flush，客户端表现为
			// 「思考憋很久 → 一次性刷出全部思考 → 正文才开始流式」。
			// 工具轮次（followUp）不这么做：整轮缓冲后由服务端合成检索块，
			// 提前外发会让原始 tool_use 和合成块重复。
			// 失败重试时 runJob 会重新走首轮，客户端收到新的 message_start 会重置。
			if !followUp && strings.Contains(payload, `"thinking_delta"`) {
				emit()
			}
		}
	}
	if text.Len() == 0 && !sawStop {
		tail := pending
		if len(tail) > 6 {
			tail = tail[len(tail)-6:]
		}
		log.Printf("  断流尾部 conv=%s:\n    %s", short(convID), strings.Join(tail, "\n    "))
	}
	if !published {
		// 工具轮次不转发上游原始事件：这一轮没有正文，runJob 会用检索结果
		// 合成一条完整的块序列给客户端，两边都发会出现重复块。
		if sawStop && len(toolCalls.calls()) == 0 {
			// 正常结束但没正文（如思考耗尽 max_tokens）：把缓冲照常发出去
			for _, l := range pending {
				j.append(l)
			}
			published = true
		}
		pending = nil
	}
	if text.Len() > 0 || thinking.Len() > 0 {
		appendAssistant(convID, text.String(), thinking.String(), meta, blockAcc.build())
	}
	log.Printf("  任务尝试结束 conv=%s 正文=%d字 stop=%v", short(convID), text.Len(), sawStop)
	// 只有正常结束（sawStop）才认工具调用，断流的半截不算
	if !sawStop {
		return text.Len(), sawStop, true, nil, ""
	}
	return text.Len(), sawStop, false, toolCalls.calls(), ""
}

// retryableStatus 判断上游状态码是否值得重试：
// 408/429 与 5xx 是临时性的（超时、限流、上游抖动），可以退避后重来；
// 其余 4xx 是请求本身有问题（参数、图片超限、鉴权），重试必然还是同样的错。
func retryableStatus(code int) bool {
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return true
	}
	return code >= 500
}

// upstreamErrorMessage 从上游错误响应里取出人类可读的原因。
// 优先 JSON 的 error.message（Anthropic 兼容格式），退回纯文本，都读不到给状态码。
func upstreamErrorMessage(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := strings.TrimSpace(string(data))
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &ev); err == nil && strings.TrimSpace(ev.Error.Message) != "" {
		return strings.TrimSpace(ev.Error.Message)
	}
	if text != "" {
		return truncateText(text, 600)
	}
	return fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
}

// appendUpstreamError 把上游拒绝的原因按 Anthropic 的 error 帧下发，
// 客户端 parseEvent/streamMessage 会抛 ApiError('stream', message) 并显示这句话。
func (j *streamJob) appendUpstreamError(message string) {
	// 「图片单边超限」说明这个 file_id 指向的是未切分的老图：前端本地有原件，
	// 只要按「file_id 失效」重传一次就能走新的切分上传链路自愈。
	// 前端 getFileErrorCode 认这个固定文案，extractFileIds 会从文案里提取
	// file-api-xxx 并重置对应附件的 serverId（见 errors.ts / reuploadInvalidatedFiles）。
	if strings.Contains(message, "image is too large") || strings.Contains(message, "at most 8192 pixels") {
		ids := j.fileIDs
		if len(ids) > 0 {
			parts := make([]string, 0, len(ids))
			for _, id := range ids {
				if strings.HasPrefix(id, "file-api-") {
					parts = append(parts, id)
				}
			}
			if len(parts) > 0 {
				message = "file_ids do not exist or are not created under your account: " +
					strings.Join(parts, ", ") +
					"。该图片超过 8192 像素，需重新上传以自动切分。"
			}
		}
	}
	j.appendSSE("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"message": truncateText(message, 600),
		},
	})
}

// collectImageFileIDs 从请求 messages 里收集图片块的 file_id，写进 job
func collectImageFileIDs(messages []any) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok || bm["type"] != "image" {
				continue
			}
			src, ok := bm["source"].(map[string]any)
			if !ok || src["type"] != "file" {
				continue
			}
			if id, _ := src["file_id"].(string); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// stripWebSearch 去掉请求体里的联网搜索工具与提示
func stripWebSearch(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if list, ok := obj["tools"].([]any); ok {
		kept := make([]any, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				if name, _ := m["name"].(string); name == "deepseek_web_search" {
					continue
				}
			}
			kept = append(kept, item)
		}
		obj["tools"] = kept
	}
	if patched, err := json.Marshal(obj); err == nil {
		return patched
	}
	return body
}

// stripServerTools 去掉请求体里服务端执行的工具（run_rhai / history_search），
// 保留上游自己执行的联网搜索。工具轮次预算用尽时调用：错误信息已经回灌给
// 模型，去掉工具后下一轮模型只能直接回答，避免「脚本出错 → 轮次耗尽 →
// 直接发 error 事件把输出中断掉」。
func stripServerTools(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if list, ok := obj["tools"].([]any); ok {
		kept := make([]any, 0, len(list))
		for _, t := range list {
			if m, ok := t.(map[string]any); ok {
				if name, _ := m["name"].(string); name == rhaiToolName || name == historyToolName {
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

// appendNoToolNotice 在工具轮次预算耗尽后，把「工具不可用、直接回答」的提示
// 并进 buildToolRound 刚追加的最后一条 user(tool_result) 消息里。
// 去掉 tools 定义本身不够：实测模型偶尔会把工具调用格式（<｜DSML｜ calls>
// 之类的标签）当正文输出，前端把全角 ｜ 当 Markdown 表格分隔渲染成乱码。
// 关键是**不能新追加一条独立的 user 消息**——那会形成连续两条 user，
// 上游遇到连续 user 会立刻断流（见 AGENTS.md 已知坑）。
func appendNoToolNotice(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return body
	}
	notice := "\n\n【系统提示】沙箱工具已达到本轮调用上限，当前不可再调用任何工具。" +
		"请不要输出任何工具调用标签或伪装的调用文本，直接用自然语言给出最终答案。" +
		"如果脚本执行有报错，就根据已知信息直接回答或说明无法得出结果。"
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok || last["role"] != "user" {
		return body
	}
	switch c := last["content"].(type) {
	case string:
		last["content"] = c + notice
	case []any:
		// tool_result 块数组：把提示作为一个 text 块并进去，仍是同一条 user 消息
		last["content"] = append(c, map[string]any{"type": "text", "text": notice})
	default:
		return body
	}
	obj["messages"] = msgs
	if patched, err := json.Marshal(obj); err == nil {
		return patched
	}
	return body
}

// cacheIfUndelivered 如果没有任何客户端完整收下本轮回答，就把内容放进离线缓存
func (j *streamJob) cacheIfUndelivered(req *http.Request, convID string) {
	j.mu.Lock()
	delivered := j.delivered
	j.mu.Unlock()
	if delivered {
		return
	}
	var text, thinking, stop, blocks string
	storeMu.Lock()
	// 取本轮最新落库的助手消息
	_ = db.QueryRow(`SELECT content, thinking, stop_reason, COALESCE(blocks,'')
		FROM messages WHERE conv_id=? AND role='assistant' ORDER BY id DESC LIMIT 1`, convID).
		Scan(&text, &thinking, &stop, &blocks)
	storeMu.Unlock()
	var raw []json.RawMessage
	if blocks != "" {
		_ = json.Unmarshal([]byte(blocks), &raw)
	}
	saveCachedReply(j.uid, convID, text, thinking, stop, raw)
}

// serveMessageJob 把请求交给后台任务，并把事件按序推给当前客户端
func serveMessageJob(w http.ResponseWriter, r *http.Request, req *http.Request, body []byte, convID string) {
	uid, _, _ := resolveToken(tokenOf(r))
	key := jobKey(convID, string(body))

	var job *streamJob
	if id := r.Header.Get("X-Resume"); id != "" {
		jobsMu.Lock()
		job = jobs[id]
		jobsMu.Unlock()
		if job == nil {
			jobsMu.Lock()
			job = jobs[key]
			jobsMu.Unlock()
		}
	}
	if job == nil {
		job = newJob(uid)
		jobsMu.Lock()
		jobs[key] = job
		jobsMu.Unlock()
		go runJob(job, req, body, convID)
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Job-Id", key)
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	ch, unsubscribe := job.subscribe()
	defer unsubscribe()

	startedAt := time.Now()
	written := 0
	for {
		events, done, errMsg := job.snapshot()
		for written < len(events) {
			if _, err := io.WriteString(w, events[written]+"\n"); err != nil {
				// 客户端断开：任务继续在后台跑完，下次可用 X-Resume 续传
				log.Printf("  客户端断开 conv=%s 已发 %d 事件，任务继续", short(convID), written)
				return
			}
			written++
		}
		if flusher != nil {
			flusher.Flush()
		}
		if done {
			job.mu.Lock()
			job.delivered = true
			job.mu.Unlock()
			if errMsg != "" {
				log.Printf("  任务异常结束 conv=%s: %s", short(convID), errMsg)
			}
			log.Printf("  任务推流完成 conv=%s 事件=%d 用时 %.1fs", short(convID), written, time.Since(startedAt).Seconds())
			return
		}
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			// 心跳：保持连接活跃
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			log.Printf("  客户端取消 conv=%s 已发 %d 事件，任务继续", short(convID), written)
			return
		}
	}
}
