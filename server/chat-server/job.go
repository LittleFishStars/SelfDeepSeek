package main

// 服务端主导的流式任务：上游请求由服务器发起并完整读完，
// 客户端连接断开不影响生成；重连时用 X-Resume: <jobId> 从断点续传。

import (
	"bufio"
	"context"
	"encoding/json"
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

	bodyForAttempt := body
	// 历史检索的工具循环：模型调 history_search 时，服务端自己执行并把结果回灌，
	// 再请求一次，直到它不再调工具（上限 historyMaxRounds 轮）。
	// 合成块的索引从 1000 起：工具轮次之后的正文块又从 index 0 开始，
	// 用低位会和正文撞车，客户端按 index 合并时会把检索块覆盖掉。
	blockBase := 1000
	for round := 0; round < historyMaxRounds; round++ {
		textLen, stopped, aborted, calls := runOnce(j, req, bodyForAttempt, convID, round > 0)
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
			log.Printf("  工具轮次 %d conv=%s 调用=%d（检索 %d，脚本 %d）",
				round+1, short(convID), len(calls), len(history), len(rhai))
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
		bodyForAttempt = stripWebSearch(bodyForAttempt)
		log.Printf("  上游断流且无正文，重试 conv=%s attempt=%d", short(convID), round+1)
	}
	j.append(`data: {"type":"error","error":{"type":"server_error","message":"上游生成中断，请重试"}}`)
	j.append("")
	j.finish("upstream aborted")
}

// runOnce 跑一次上游请求。返回值：正文长度、是否收到 message_stop、是否异常中断。
// 事件先写进本地 pending，确认有正文后再一次性发布，失败的那次客户端看不到。
func runOnce(j *streamJob, req *http.Request, body []byte, convID string, followUp bool) (int, bool, bool, []toolCall) {
	ctx, cancel := context.WithTimeout(context.Background(), reqTimeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL.String(), strings.NewReader(string(body)))
	if err != nil {
		return 0, false, true, nil
	}
	out.Header = req.Header.Clone()
	resp, err := upstreamClient.Do(out)
	if err != nil {
		log.Printf("!! 上游请求失败 conv=%s: %v", short(convID), err)
		return 0, false, true, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("!! 上游状态码 conv=%s: %d", short(convID), resp.StatusCode)
		return 0, false, true, nil
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
		return text.Len(), sawStop, true, nil
	}
	return text.Len(), sawStop, false, toolCalls.calls()
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
