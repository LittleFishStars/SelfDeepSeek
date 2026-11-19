package main

// 附件跨端同步：会话里的图片原件存在服务端，网页端/App 打开会话时按需下载。
// 元数据（id/name/mime/isImage）随消息一起存，二进制按 id 落在数据目录下的 files/。

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// storedFile 与客户端 FileAttachment 对应
type storedFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	IsImage  bool   `json:"isImage"`
	Size     int64  `json:"size"`
}

func filesDir() string {
	dir := filepath.Join(filepath.Dir(env("CHAT_DB", "/var/lib/chat-server/chat.db")), "files")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func fileBlobPath(id string) string {
	return filepath.Join(filesDir(), sanitizeUploadID(id))
}

// handleFileUpload 接收 multipart 字段 file 与 form 字段 id（可选，缺省用随机值）
func handleFileUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := userFor(r)
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, 400, map[string]any{"error": "上传失败: " + err.Error()})
		return
	}
	src, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "缺少文件字段 file"})
		return
	}
	defer src.Close()

	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		id = randomToken()[:24]
	}
	dst, err := os.Create(fileBlobPath(id))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "服务端写入失败"})
		return
	}
	n, err := io.Copy(dst, io.LimitReader(src, 64<<20))
	dst.Close()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "写入失败"})
		return
	}
	log.Printf("  附件上传 user=%s id=%s %s %.1fKB", short(uid), short(id), header.Filename, float64(n)/1024)
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "size": n})
}

// handleFileDownload 按 id 取回原件
func handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFor(r); !ok {
		writeJSON(w, 401, map[string]any{"error": "missing token"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/files/")
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, 400, map[string]any{"error": "bad id"})
		return
	}
	path := fileBlobPath(id)
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	if info != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}
