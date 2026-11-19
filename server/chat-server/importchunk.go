package main

// 分片上传：手机端 90MB 的导出包经 Cloudflare 一次性上传容易在 ~2 分钟处被中断，
// 改成 4MB 一片顺序上传，服务端按 upload id 追加到同一个临时文件，最后再异步解析。

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	chunkMu sync.Mutex
)

func chunkPath(id string) string {
	return filepath.Join(os.TempDir(), "chat-import-"+sanitizeUploadID(id)+".zip")
}

func sanitizeUploadID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "anon"
	}
	return b.String()
}

// handleImportChunk 接收一片数据：?upload=<id>&index=<n>，index=0 时清空重建
func handleImportChunk(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	id := r.URL.Query().Get("upload")
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "缺少 upload 参数"})
		return
	}
	index, _ := strconv.Atoi(r.URL.Query().Get("index"))
	path := chunkPath(id)

	chunkMu.Lock()
	defer chunkMu.Unlock()

	if index == 0 {
		_ = os.Remove(path)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if index == 0 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "服务端写入失败"})
		return
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, 32<<20))
	f.Close()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "写入分片失败"})
		return
	}
	_ = uid
	writeJSON(w, 200, map[string]any{"ok": true, "index": index, "bytes": n})
}

// handleImportFinish 全部分片到齐后启动后台解析
func handleImportFinish(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	id := r.URL.Query().Get("upload")
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "缺少 upload 参数"})
		return
	}
	path := chunkPath(id)
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		writeJSON(w, 400, map[string]any{"error": "没有收到分片数据"})
		return
	}
	log.Printf("  分片上传完成 user=%s 大小=%.1fMB", short(uid), float64(info.Size())/(1<<20))
	setJob(uid, func(j *importJob) { *j = importJob{Running: true} })
	writeJSON(w, 202, map[string]any{"ok": true, "queued": true})
	go importZipAsync(path, uid)
}
