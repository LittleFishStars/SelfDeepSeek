package main

// run_rhai：让模型在隔离环境里执行 Rhai 小程序
//
// 执行器是个独立二进制（server/rhai-runner），原因有两条：
//   - Rhai 是 Rust 库，Go 没有原生绑定；
//   - 独立进程超时可以直接 kill，脚本 panic 也伤不到 chat-server。
//
// 隔离边界：标准 Rhai 引擎不注册任何 IO（无文件/网络/进程/环境变量），
// 加上操作数上限与内存类上限，再由本文件按超时 kill 进程。
//
// 环境变量：
//   CHAT_RHAI_BIN     执行器路径（默认 /opt/chat-server/rhai-runner）
//   CHAT_RHAI_ENABLE  设为 0 关闭该工具（默认开启）

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

const rhaiToolName = "run_rhai"

// rhaiMaxScriptBytes 脚本上限：模型没必要写这么大的脚本，挡住异常输入。
const rhaiMaxScriptBytes = 64 << 10

// rhaiMaxOutputChars 回灌给模型的输出上限，避免一次性把上下文撑爆。
const rhaiMaxOutputChars = 8000

// rhaiOutputCap 脚本输出与会话流的截断长度。
type rhaiRequest struct {
	Script        string `json:"script"`
	MaxOperations uint64 `json:"max_operations"`
	MaxStringSize uint64 `json:"max_string_size"`
	MaxArraySize  uint64 `json:"max_array_size"`
	MaxDepth      uint64 `json:"max_depth"`
}

type rhaiResponse struct {
	OK        bool   `json:"ok"`
	Output    string `json:"output"`
	Prints    string `json:"prints"`
	Error     string `json:"error"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

func rhaiBinary() string {
	return env("CHAT_RHAI_BIN", "/opt/chat-server/rhai-runner")
}

func rhaiEnabled() bool {
	if env("CHAT_RHAI_ENABLE", "1") == "0" {
		return false
	}
	_, err := os.Stat(rhaiBinary())
	return err == nil
}

// rhaiToolDef 注入给模型的工具定义。
// 参数都给了默认值，模型只需要写 script，必要时才调超时/限额。
func rhaiToolDef() map[string]any {
	return map[string]any{
		"name": rhaiToolName,
		"description": "在隔离沙箱里执行一段 Rhai 脚本并返回结果。适合做计算、字符串处理、" +
			"遍历/变换数组、验证算法这类『运行一下比心算可靠』的事。" +
			"沙箱内没有文件、网络、进程和环境变量，只能做纯计算；结果通过 return 或 print 输出。" +
			"Rhai 语法接近 JavaScript/Rust：let/const、for、while、if、数组 [1,2]、map " +
			"#{k:1}、字符串 \"..\"、闭包 |x| x*2、字符串拼接用 +。",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"script": map[string]any{
					"type":        "string",
					"description": "要执行的 Rhai 脚本；把结果作为最后一个表达式的值返回，或用 print 输出",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "超时毫秒数，默认 3000，上限 10000",
				},
				"max_operations": map[string]any{
					"type":        "integer",
					"description": "允许的最大操作数，默认 200000；死循环会被它打断",
				},
				"max_string_size": map[string]any{
					"type":        "integer",
					"description": "单个字符串最大长度，默认 64000",
				},
			},
			"required": []string{"script"},
		},
	}
}

// ensureRhaiTool 在启用时把工具塞进 tools（去重）。
func ensureRhaiTool(reqObj map[string]any) bool {
	if !rhaiEnabled() {
		return false
	}
	list, _ := reqObj["tools"].([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if name, _ := m["name"].(string); name == rhaiToolName {
				return false
			}
		}
	}
	reqObj["tools"] = append(list, rhaiToolDef())
	return true
}

// parseRhaiArgs 解析工具入参，并对越界值做钳制（模型可能给离谱数字）。
func parseRhaiArgs(input string) (script string, timeoutMS int, maxOps uint64, maxStr uint64) {
	var in struct {
		Script        string `json:"script"`
		TimeoutMS     int    `json:"timeout_ms"`
		MaxOperations uint64 `json:"max_operations"`
		MaxStringSize uint64 `json:"max_string_size"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", 0, 0, 0
	}
	timeoutMS = in.TimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 3000
	}
	if timeoutMS > 10000 {
		timeoutMS = 10000
	}
	maxOps = in.MaxOperations
	if maxOps == 0 {
		maxOps = 200000
	}
	if maxOps > 5_000_000 {
		maxOps = 5_000_000
	}
	maxStr = in.MaxStringSize
	if maxStr == 0 {
		maxStr = 64000
	}
	if maxStr > 1<<20 {
		maxStr = 1 << 20
	}
	return in.Script, timeoutMS, maxOps, maxStr
}

// runRhai 执行脚本，返回结果文本（给模型看）。
func runRhai(input string) (resp rhaiResponse, text string) {
	script, timeoutMS, maxOps, maxStr := parseRhaiArgs(input)
	if strings.TrimSpace(script) == "" {
		return rhaiResponse{Error: "脚本为空"}, "脚本为空，没有执行。"
	}
	if len(script) > rhaiMaxScriptBytes {
		return rhaiResponse{Error: "脚本过大"}, fmt.Sprintf("脚本超过 %d 字节，拒绝执行。", rhaiMaxScriptBytes)
	}

	reqBody, _ := json.Marshal(rhaiRequest{
		Script:        script,
		MaxOperations: maxOps,
		MaxStringSize: maxStr,
		MaxArraySize:  10000,
		MaxDepth:      24,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, rhaiBinary())
	cmd.Stdin = bytes.NewReader(reqBody)
	// 清环境：执行器不需要任何环境变量，减少信息面
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		text = fmt.Sprintf("脚本执行超时（%d ms），已终止。", timeoutMS)
		return rhaiResponse{Error: "timeout", ElapsedMS: int64(timeoutMS)}, text
	}
	if err != nil && stdout.Len() == 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		text = "执行器启动失败：" + msg
		return rhaiResponse{Error: msg}, text
	}

	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		text = "执行器返回了无法解析的结果：" + truncateText(stdout.String(), 400)
		return resp, text
	}
	return resp, renderRhaiForModel(resp, script)
}

// renderRhaiForModel 把执行结果拼成模型好读的文本。
func renderRhaiForModel(resp rhaiResponse, script string) string {
	var sb strings.Builder
	if resp.OK {
		fmt.Fprintf(&sb, "执行成功（%d ms）。\n", resp.ElapsedMS)
		if strings.TrimSpace(resp.Output) != "" {
			sb.WriteString("返回值：\n")
			sb.WriteString(truncateText(resp.Output, rhaiMaxOutputChars))
			sb.WriteString("\n")
		} else {
			sb.WriteString("脚本没有返回值。\n")
		}
	} else {
		fmt.Fprintf(&sb, "执行失败（%d ms）：%s\n", resp.ElapsedMS, resp.Error)
	}
	if strings.TrimSpace(resp.Prints) != "" {
		sb.WriteString("print 输出：\n")
		sb.WriteString(truncateText(resp.Prints, rhaiMaxOutputChars))
		sb.WriteString("\n")
	}
	return sb.String()
}

func truncateText(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "\n…（已截断）"
}

// rhaiResultBlock 合成为客户端可渲染的结果块。
func rhaiResultBlock(call toolCall, resp rhaiResponse) map[string]any {
	script, _, _, _ := parseRhaiArgs(call.Input)
	return map[string]any{
		"type":        "rhai_tool_result",
		"tool_use_id": call.ID,
		"script":      truncateText(script, 4000),
		"ok":          resp.OK,
		"output":      truncateText(resp.Output, 4000),
		"prints":      truncateText(resp.Prints, 2000),
		"error":       truncateText(resp.Error, 800),
		"elapsed_ms":  resp.ElapsedMS,
	}
}

// handleRhaiToolRound 执行本轮所有脚本调用，返回回灌文本与结果块。
func handleRhaiToolRound(convID string, calls []toolCall) ([]string, []map[string]any) {
	var texts []string
	var blocks []map[string]any
	for _, call := range calls {
		resp, text := runRhai(call.Input)
		log.Printf("  沙箱脚本 conv=%s ok=%v %dms", short(convID), resp.OK, resp.ElapsedMS)
		texts = append(texts, text)
		blocks = append(blocks, rhaiResultBlock(call, resp))
	}
	return texts, blocks
}

// appendRhaiEvents 把脚本执行过程按 SSE 事件写进任务流：
// 一条 tool_use（展示脚本）+ 一条结果块（展示输出/报错）。
// 注意事件之间必须有空行，否则客户端会把下一条事件拼进来一起丢掉。
func appendRhaiEvents(j *streamJob, call toolCall, block map[string]any, baseIndex int) {
	start := map[string]any{
		"type":  "content_block_start",
		"index": baseIndex,
		"content_block": map[string]any{
			"type":   "server_tool_use",
			"id":     call.ID,
			"name":   rhaiToolName,
			"input":  buildRhaiInputView(call.Input),
			"caller": map[string]any{"type": "direct"},
		},
	}
	j.appendSSE("content_block_start", start)
	j.appendSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": baseIndex})

	result := map[string]any{
		"type":          "content_block_start",
		"index":         baseIndex + 1,
		"content_block": block,
	}
	j.appendSSE("content_block_start", result)
	j.appendSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": baseIndex + 1})
}

// buildRhaiInputView 只把必要参数发给界面（脚本 + 实际生效的超时/限额）。
func buildRhaiInputView(input string) map[string]any {
	script, timeoutMS, maxOps, maxStr := parseRhaiArgs(input)
	return map[string]any{
		"script":          truncateText(script, 4000),
		"timeout_ms":      timeoutMS,
		"max_operations":  maxOps,
		"max_string_size": maxStr,
	}
}
