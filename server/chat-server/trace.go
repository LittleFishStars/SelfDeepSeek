package main

// 附件请求的临时留存：排查「图片 + 联网搜索」偶发断流时，用于拿到客户端真实请求体。
// 只保留最近 5 份，通过 CHAT_TRACE_DIR 开启，不设置则完全不写盘。

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

func traceDirOf() string {
	return env("CHAT_TRACE_DIR", "")
}

func saveAttachTrace(dir, kind string, data []byte) {
	_ = os.MkdirAll(dir, 0o700)
	name := filepath.Join(dir, "attach-"+kind+"-"+time.Now().Format("150405.000")+".txt")
	_ = os.WriteFile(name, data, 0o600)

	// 同类文件只保留最近 5 份
	matches, _ := filepath.Glob(filepath.Join(dir, "attach-"+kind+"-*.txt"))
	if len(matches) > 5 {
		sort.Strings(matches)
		for _, old := range matches[:len(matches)-5] {
			_ = os.Remove(old)
		}
	}
}
