package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// startMockUpstream 模拟上游 Files API：记录每次上传的文件名与尺寸
func startMockUpstream(t *testing.T, uploads *atomic.Int32, seen *[]image.Config) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/files"):
			n := uploads.Add(1)
			mr := multipart.NewReader(r.Body, r.Header.Get("Content-Type"))
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if part.FormName() == "file" {
					b, _ := io.ReadAll(part)
					cfg, _, _ := image.DecodeConfig(bytes.NewReader(b))
					*seen = append(*seen, cfg)
				}
				part.Close()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("file-api-mock-%03d", n), "object": "file"})
		default:
			w.WriteHeader(200)
		}
	}))
}

func setupSliceStore(t *testing.T) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "t.db")
	openDB(dbPath)
	migrateStore()
}

func TestEndToEndSliceAndExpand(t *testing.T) {
	setupSliceStore(t)
	var uploads atomic.Int32
	var seen []image.Config
	srv := startMockUpstream(t, &uploads, &seen)
	defer srv.Close()
	apiBase = srv.URL
	apiKey = "k"

	// 1) 走 handleSliceFiles 上传一张超限长图
	data := mkImage(t, 1095, 21555)
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, err := mw.CreateFormFile("file", "longshot.png")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(data)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/files", &form)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	handled, err := handleSliceFiles(rec, req, "/v1/files", form.Bytes(), "u-test")
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !handled {
		t.Fatal("超限图片应由服务端接管")
	}

	var out struct {
		ID     string `json:"id"`
		Slices int    `json:"slices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, sliceIDPrefix) {
		t.Fatalf("应返回聚合 id，得到 %q", out.ID)
	}
	if out.Slices != 3 {
		t.Errorf("应切 3 片，得到 %d", out.Slices)
	}
	if int(uploads.Load()) != 3 {
		t.Errorf("上游应收到 3 次上传，实际 %d", uploads.Load())
	}
	for i, cfg := range seen {
		if cfg.Width > imageMaxSide || cfg.Height > imageMaxSide {
			t.Errorf("第 %d 片仍超限: %dx%d", i+1, cfg.Width, cfg.Height)
		}
	}
	t.Logf("聚合 id=%s，上游收到 %d 片，尺寸 %v", out.ID, uploads.Load(), seen)

	// 2) messages 请求里引用聚合 id，应展开成 3 个真实 image 块
	messages := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "file", "file_id": out.ID}},
			map[string]any{"type": "text", "text": "看这张图"},
		}},
	}
	expanded, replaced := expandSlicedImages(messages)
	if replaced != 1 || expanded != 3 {
		t.Fatalf("replaced=%d expanded=%d，期望 1/3", replaced, expanded)
	}
	blocks := messages[0].(map[string]any)["content"].([]any)
	if len(blocks) != 4 { // 3 图 + 1 文本
		t.Fatalf("展开后应有 4 个块，得到 %d", len(blocks))
	}
	for i := 0; i < 3; i++ {
		b := blocks[i].(map[string]any)
		src := b["source"].(map[string]any)
		if fid, _ := src["file_id"].(string); !strings.HasPrefix(fid, "file-api-mock-") {
			t.Fatalf("第 %d 块应已换成上游真实 id，得到 %q", i, fid)
		}
	}

	// 3) 合规图片不接管
	var form2 bytes.Buffer
	mw2 := multipart.NewWriter(&form2)
	p2, _ := mw2.CreateFormFile("file", "small.png")
	p2.Write(mkImage(t, 800, 600))
	mw2.Close()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/files", &form2)
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	handled2, err2 := handleSliceFiles(httptest.NewRecorder(), req2, "/v1/files", form2.Bytes(), "u-test")
	if err2 != nil {
		t.Fatal(err2)
	}
	if handled2 {
		t.Fatal("合规图片不该被接管")
	}

	// 4) 非聚合 id 的 DELETE 不接管
	req3 := httptest.NewRequest(http.MethodDelete, "/v1/files/file-api-xxx", nil)
	handled3, _ := handleSliceFiles(httptest.NewRecorder(), req3, "/v1/files/file-api-xxx", nil, "u-test")
	if handled3 {
		t.Fatal("普通 id 删除不该被接管")
	}

	// 5) 聚合 id 的 DELETE 接管
	req4 := httptest.NewRequest(http.MethodDelete, "/v1/files/"+out.ID, nil)
	handled4, err4 := handleSliceFiles(httptest.NewRecorder(), req4, "/v1/files/"+out.ID, nil, "u-test")
	if err4 != nil {
		t.Fatal(err4)
	}
	if !handled4 {
		t.Fatal("聚合 id 删除应被接管")
	}
	if ids := slicedFileIDs(out.ID); len(ids) != 0 {
		t.Fatalf("删除后映射应清空，剩 %v", ids)
	}
}
