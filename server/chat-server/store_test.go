package main

import (
	"database/sql"
	"encoding/json"
	"testing"
)

func mkNode(id, parent int64, role, content, thinking string, blockTypes ...string) node {
	n := node{ID: id, Role: role, Content: content, Thinking: thinking}
	if parent != 0 {
		n.ParentID = sql.NullInt64{Int64: parent, Valid: true}
	}
	blocks := make([]map[string]any, 0, len(blockTypes))
	for _, t := range blockTypes {
		blocks = append(blocks, map[string]any{"type": t})
	}
	if raw, err := json.Marshal(blocks); err == nil && len(blocks) > 0 {
		n.Blocks = string(raw)
	}
	return n
}

func blockTypes(t *testing.T, n node) []string {
	t.Helper()
	var blocks []map[string]any
	if n.Blocks == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(n.Blocks), &blocks); err != nil {
		t.Fatalf("blocks 不是合法 JSON: %v", err)
	}
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		s, _ := b["type"].(string)
		out = append(out, s)
	}
	return out
}

// 一条分支上的工具轮：中间两条空正文 assistant 应该被最后那条吸收，
// parent 拉平到 user 上，id 用最后那条（前端据此切分支）。
func TestCoalesceToolStepsLinear(t *testing.T) {
	nodes := []node{
		mkNode(1, 0, "user", "算个杠杆", "", "text"),
		mkNode(2, 1, "assistant", "", "想1", "thinking", "tool_use"),
		mkNode(3, 2, "assistant", "", "想2", "thinking", "tool_use"),
		mkNode(4, 3, "assistant", "结论", "想3", "thinking", "text"),
	}
	out := coalesceToolSteps(nodes)
	if len(out) != 2 {
		t.Fatalf("应合并成 2 条，得到 %d 条", len(out))
	}
	got := out[1]
	if got.ID != 4 {
		t.Errorf("合并后应保留最后一条的 id=4，得到 %d", got.ID)
	}
	if parentOf(got) != 1 {
		t.Errorf("parent 应拉平到 user(1)，得到 %d", parentOf(got))
	}
	if got.Content != "结论" {
		t.Errorf("正文不应被改动，得到 %q", got.Content)
	}
	if got.Thinking != "想1\n\n想2\n\n想3" {
		t.Errorf("思考应顺序拼接，得到 %q", got.Thinking)
	}
	want := []string{"thinking", "tool_use", "thinking", "tool_use", "thinking", "text"}
	have := blockTypes(t, got)
	if len(have) != len(want) {
		t.Fatalf("块数量不对：%v", have)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Fatalf("块顺序不对：%v，期望 %v", have, want)
		}
	}
}

// 这一轮真被中断（链尾没有正文）：保留一条，让前端继续显示「已停止」
func TestCoalesceToolStepsTailStaysEmpty(t *testing.T) {
	nodes := []node{
		mkNode(1, 0, "user", "问题", ""),
		mkNode(2, 1, "assistant", "", "想", "thinking", "tool_use"),
	}
	out := coalesceToolSteps(nodes)
	if len(out) != 2 {
		t.Fatalf("应保持 2 条，得到 %d 条", len(out))
	}
	if out[1].Content != "" || parentOf(out[1]) != 1 || out[1].ID != 2 {
		t.Fatalf("中断的那轮应原样保留：%+v", out[1])
	}
}

// 兄弟分支不能串起来：tree() 按 id 全局排序，同父的两条空消息相邻但不在同一分支
func TestCoalesceToolStepsSiblingsDoNotMerge(t *testing.T) {
	nodes := []node{
		mkNode(1, 0, "user", "问题", ""),
		mkNode(2, 1, "assistant", "", "分支A", "thinking", "tool_use"),
		mkNode(3, 1, "assistant", "", "分支B", "thinking", "tool_use"),
		mkNode(4, 3, "assistant", "B 的结论", "", "thinking", "text"),
	}
	out := coalesceToolSteps(nodes)
	if len(out) != 3 {
		t.Fatalf("应得到 user + 分支A + 分支B 合并后的正文 = 3 条，得到 %d 条", len(out))
	}
	if out[1].ID != 2 || out[1].Thinking != "分支A" {
		t.Errorf("分支A 应独立保留：id=%d thinking=%q", out[1].ID, out[1].Thinking)
	}
	if out[2].ID != 4 || out[2].Thinking != "分支B" || out[2].Content != "B 的结论" {
		t.Errorf("分支B 应吸收 3：%+v", out[2])
	}
}

func TestCoalesceToolStepsNoop(t *testing.T) {
	single := []node{mkNode(1, 0, "user", "hi", "")}
	if out := coalesceToolSteps(single); len(out) != 1 {
		t.Fatalf("单节点不应改变，得到 %d 条", len(out))
	}
	if out := coalesceToolSteps(nil); out != nil {
		t.Fatalf("空输入应返回 nil")
	}
	plain := []node{
		mkNode(1, 0, "user", "q", ""),
		mkNode(2, 1, "assistant", "a", "", "thinking", "text"),
	}
	if out := coalesceToolSteps(plain); len(out) != 2 {
		t.Fatalf("没有工具轮时不应改变，得到 %d 条", len(out))
	}
}

// 版本号要按合并后的结构算，不能再回库查旧父子关系
func TestSiblingIndexFromAfterMerge(t *testing.T) {
	nodes := coalesceToolSteps([]node{
		mkNode(1, 0, "user", "q", ""),
		mkNode(2, 1, "assistant", "", "A1", "thinking", "tool_use"),
		mkNode(3, 2, "assistant", "", "A2", "thinking", "tool_use"),
		mkNode(4, 3, "assistant", "A 结论", "", "thinking", "text"),
		mkNode(5, 1, "assistant", "", "B1", "thinking", "tool_use"),
		mkNode(6, 5, "assistant", "B 结论", "", "thinking", "text"),
	})
	idx := siblingIndexFrom(nodes)
	if v := idx[[2]int64{1, 4}]; v != [2]int{1, 2} {
		t.Errorf("A 分支应是 1/2，得到 %v", v)
	}
	if v := idx[[2]int64{1, 6}]; v != [2]int{2, 2} {
		t.Errorf("B 分支应是 2/2，得到 %v", v)
	}
}
