package main

// 超限图片自动切分。
//
// 上游对图片有「单边最长 8192 像素」的硬限制（单个请求带 15 张及以上图片时
// 降为 4096），长截图（实测 1095x21555）会直接 400：
//   The image is too large to process: its width and height must each be at most 8192 pixels.
//
// 这里在 /anthropic/v1/files 代理层把超限图片按规则切成多片分别上传，
// 客户端只看到一个聚合 file_id；发 /v1/messages 时再把聚合 id 展开成多个
// image 块。放在服务端的好处是全端（安卓 / 鸿蒙 / 网页）自动生效，不用改前端。
//
// 切分规则（2026-11-22 用户定案）：
//   - 仅长或宽超限（另一边合规）：不缩放，沿超限方向**均匀切分成恰好
//     满足条件的最少张数**，片间留重叠；
//   - 两边都超限：先等比压缩到**短边恰好等于上限**，再按上一条切分。
//
// 重叠用「每片的余量」换：片数 = ceil(边长/上限)，每片基准长
// base = ceil(边长/片数)，余量 spare = 上限 - base，重叠 = spare/2（中间片
// 两侧各借一半）。这样加了重叠也不增加张数。余量小到不足 imageMinOverlap
// 时重叠没意义，改为片数 +1 重算。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 手机截图偶见 webp
)

const (
	imageMaxSide      = 8192 // 普通请求的单边像素上限
	imageMaxSideDense = 4096 // 一次带 15 张及以上图片时的上限
	imageDenseTrigger = 15
	imageMinOverlap   = 64  // 小于这个重叠没意义，宁可多切一片
	imageMaxOverlap   = 512 // 重叠上限，避免片间重复内容太多白烧 token
	imageMaxSlices    = 12  // 单张图最多切几片，防极端长图炸出上百张
	imageSliceQuality = 90  // 切片重编码的 JPEG 质量

	// filesAPIBeta 上游 Files API 要求的 beta 标识，与前端 anthropicHeaders 一致。
	// 缺这个头连接会被直接重置（curl 表现为 http=000，
	// Go 表现为 "stream error: stream ID 1; INTERNAL_ERROR"），
	// 不是网络抖动，重试多少次都一样（2026-11-22 实测踩过）。
	filesAPIBeta = "files-api-2025-04-14"
)

// fitPlan 是切分决策，纯计算、可单测
type fitPlan struct {
	Slices   int
	MaxSide  int
	Scale    float64 // 1 表示不缩放
	Overlap  int
	AxisVert bool // true = 沿高度切（上下叠放），false = 沿宽度切
}

// imageMaxSideFor 按本次请求的图片数量选单边上限
func imageMaxSideFor(count int) int {
	if count >= imageDenseTrigger {
		return imageMaxSideDense
	}
	return imageMaxSide
}

// imageOverLimits 只解码头，判断是否超限；返回超限方向
func imageOverLimits(data []byte, maxSide int) (w, h int, over bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, cfg.Width > maxSide || cfg.Height > maxSide
}

// planFit 按用户规则算切分方案
func planFit(w, h, maxSide int) fitPlan {
	plan := fitPlan{MaxSide: maxSide, Slices: 1, Scale: 1}
	overW := w > maxSide
	overH := h > maxSide
	if !overW && !overH {
		return plan
	}

	// 两边都超限：等比压缩到短边恰好等于上限，长边可能仍超限，按新尺寸继续算
	if overW && overH {
		short := math.Min(float64(w), float64(h))
		plan.Scale = float64(maxSide) / short
		w = int(math.Round(float64(w) * plan.Scale))
		h = int(math.Round(float64(h) * plan.Scale))
		overW = w > maxSide
		overH = h > maxSide
	}

	// 选超限的那条边作为切割轴；两条都超（缩放后理论上只剩一条）取更长的那条
	axisVert := overH
	length := h
	if overW && overH && w > h {
		axisVert = false
		length = w
	} else if !overH {
		axisVert = false
		length = w
	}
	plan.AxisVert = axisVert

	// 恰好满足条件的最少片数，再用片内余量换重叠
	slices := int(math.Ceil(float64(length) / float64(maxSide)))
	for slices <= imageMaxSlices {
		base := int(math.Ceil(float64(length) / float64(slices)))
		spare := maxSide - base
		overlap := int(math.Min(float64(spare/2), float64(imageMaxOverlap)))
		if overlap >= imageMinOverlap || slices == 1 {
			plan.Slices = slices
			plan.Overlap = overlap
			return plan
		}
		slices++
	}
	// 片数超上限：不切（让上游明确报错，也比静默丢内容好）
	return fitPlan{MaxSide: maxSide, Slices: 1, Scale: plan.Scale, Overlap: 0, AxisVert: axisVert}
}

// sliceRanges 算每片在超限方向上的 [start, end)，保证无缝覆盖且带重叠
func sliceRanges(length, slices, overlap int) [][2]int {
	if slices <= 1 {
		return [][2]int{{0, length}}
	}
	base := int(math.Ceil(float64(length) / float64(slices)))
	out := make([][2]int, 0, slices)
	for i := 0; i < slices; i++ {
		start := i * base
		end := start + base
		if i > 0 { // 首片不向前借
			start -= overlap
		}
		if i < slices-1 { // 末片不向后借
			end += overlap
		}
		if start < 0 {
			start = 0
		}
		if end > length {
			end = length
		}
		out = append(out, [2]int{start, end})
	}
	// 取整造成的缝隙直接抹平，保证无缝
	for i := 1; i < len(out); i++ {
		if out[i][0] > out[i-1][1] {
			out[i][0] = out[i-1][1]
		}
	}
	return out
}

// fitImage 按规则切分超限图片；不需要切分时返回 nil。
// 输出统一 JPEG（透明像素合成到白底），与前端 toJpeg 的产物一致。
func fitImage(data []byte, maxSide int) ([][]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxSide && h <= maxSide {
		return nil, nil
	}

	plan := planFit(w, h, maxSide)
	if plan.Slices <= 1 && plan.Scale >= 1 {
		return nil, nil
	}

	work := src
	if plan.Scale > 0 && plan.Scale < 1 {
		tw := int(math.Round(float64(w) * plan.Scale))
		th := int(math.Round(float64(h) * plan.Scale))
		if tw < 1 {
			tw = 1
		}
		if th < 1 {
			th = 1
		}
		dst := image.NewRGBA(image.Rect(0, 0, tw, th))
		// CatmullRom：慢但质量最好，缩放只在两边都超限时发生，代价可接受
		draw.CatmullRom.Scale(dst, dst.Bounds(), work, work.Bounds(), draw.Src, nil)
		work = dst
		w, h = tw, th
	}

	length := h
	if !plan.AxisVert {
		length = w
	}
	ranges := sliceRanges(length, plan.Slices, plan.Overlap)

	out := make([][]byte, 0, len(ranges))
	for _, rg := range ranges {
		rect := image.Rect(0, rg[0], w, rg[1])
		if !plan.AxisVert {
			rect = image.Rect(rg[0], 0, rg[1], h)
		}
		buf, err := encodeJPEG(cropRGBA(work, rect))
		if err != nil {
			return nil, err
		}
		out = append(out, buf)
	}
	return out, nil
}

// cropRGBA 裁出子图；RGBA 目标保证后面能安全做白底合成
func cropRGBA(src image.Image, rect image.Rectangle) image.Image {
	rect = rect.Intersect(src.Bounds())
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Over)
	return dst
}

// encodeJPEG 编码为 JPEG；带透明通道的先合成到白底（JPEG 不支持 alpha）
func encodeJPEG(src image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if rgba, ok := src.(*image.RGBA); ok && alphaMightBeOpaque(rgba) {
		if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: imageSliceQuality}); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	white := image.NewRGBA(src.Bounds())
	draw.Draw(white, white.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(white, white.Bounds(), src, src.Bounds().Min, draw.Over)
	if err := jpeg.Encode(&buf, white, &jpeg.Options{Quality: imageSliceQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// alphaMightBeOpaque 抽样看 alpha 通道，全 255 就认为可以不铺白底
func alphaMightBeOpaque(m *image.RGBA) bool {
	b := m.Bounds()
	if b.Empty() {
		return true
	}
	step := 1
	if n := b.Dx() * b.Dy() / 4096; step < n {
		step = n
	}
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			if _, _, _, a := m.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------- 上传拦截

// 聚合 file_id 前缀：客户端拿到的「图片 file_id」可能是这里合成的切片集合 id，
// 真正的上游 file_id 存在 image_slices 表里。
const sliceIDPrefix = "slice-"

// trimExt 去掉文件名扩展名
func trimExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// uploadBytesToUpstream 把一段字节以 multipart 上传上游 Files API，返回 file_id。
//
// 必须走 **/anthropic/v1/files**（Anthropic 风格）：那里图片不需要 purpose 字段，
// 与前端上传路径一致。误用 OpenAI 风格的 /v1/files 会被上游拒
// 「'purpose' field is required」（2026-11-22 实测踩过）。
func uploadBytesToUpstream(name string, data []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// purpose 必须排在 file 之前（上游要求），这里不传 purpose
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, apiBase+"/anthropic/v1/files", bytes.NewReader(buf.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-beta", filesAPIBeta)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateText(strings.TrimSpace(string(body)), 200))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("上游未返回 file_id: %s", truncateText(string(body), 200))
	}
	return out.ID, nil
}

// slicedFileIDs 取聚合 id 对应的上游 file_id 列表；不是聚合 id 时返回 nil
func slicedFileIDs(id string) []string {
	if !strings.HasPrefix(id, sliceIDPrefix) {
		return nil
	}
	var joined string
	if err := db.QueryRow(`SELECT file_ids FROM image_slices WHERE agg_id=?`, id).Scan(&joined); err != nil {
		return nil
	}
	parts := strings.Split(joined, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// expandSlicedImages 把请求里引用聚合 id 的 image 块展开成多个真实 image 块。
// 返回展开后的图片总数（用于日志与密集上限判断）。
func expandSlicedImages(messages []any) (expanded int, replaced int) {
	for _, item := range messages {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		out := make([]any, 0, len(blocks))
		changed := false
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				out = append(out, b)
				continue
			}
			ids := imageBlockSliceIDs(bm)
			if len(ids) == 0 {
				out = append(out, b)
				continue
			}
			changed = true
			replaced++
			for _, id := range ids {
				out = append(out, map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "file", "file_id": id},
				})
				expanded++
			}
		}
		if changed {
			m["content"] = out
		}
	}
	return expanded, replaced
}

// imageBlockSliceIDs 判断这个内容块是否引用了聚合切片 id，是则返回展开后的 id 列表
func imageBlockSliceIDs(bm map[string]any) []string {
	typ, _ := bm["type"].(string)
	if typ != "image" {
		return nil
	}
	src, ok := bm["source"].(map[string]any)
	if !ok {
		return nil
	}
	if st, _ := src["type"].(string); st != "file" {
		return nil
	}
	fid, _ := src["file_id"].(string)
	ids := slicedFileIDs(fid)
	if len(ids) == 0 {
		return nil
	}
	// 表里只剩一条时不必展开，少一次结构改动
	if len(ids) == 1 {
		src["file_id"] = ids[0]
		return nil
	}
	return ids
}

// deleteSlicedFile 删除聚合 id 对应的所有上游切片，返回是否是聚合 id
func deleteSlicedFile(id string) bool {
	ids := slicedFileIDs(id)
	if len(ids) == 0 {
		return false
	}
	for _, one := range ids {
		req, err := http.NewRequest(http.MethodDelete, apiBase+"/anthropic/v1/files/"+url.PathEscape(one), nil)
		if err != nil {
			continue
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("anthropic-beta", filesAPIBeta)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := upstreamClient.Do(req.WithContext(ctx))
		cancel()
		if err == nil {
			resp.Body.Close()
		}
	}
	_, _ = db.Exec(`DELETE FROM image_slices WHERE agg_id=?`, id)
	return true
}

// handleSliceFiles 拦截 Files API：
//
//	POST   /v1/files      图片超限时切分上传，返回聚合 file_id；否则不接管
//	DELETE /v1/files/{id} id 是聚合 id 时删掉全部切片
//
// 返回 handled=false 表示调用方应继续按原逻辑代理。
func handleSliceFiles(w http.ResponseWriter, r *http.Request, path string, body []byte, uid string) (bool, error) {
	// 删除：只有聚合 id 需要我们接管，普通 id 交回代理逻辑
	if r.Method == http.MethodDelete {
		marker := "/v1/files/"
		k := strings.Index(path, marker)
		if k < 0 {
			return false, nil
		}
		id := path[k+len(marker):]
		if id == "" || strings.Contains(id, "/") {
			return false, nil
		}
		if !deleteSlicedFile(id) {
			return false, nil
		}
		log.Printf("  附件切片删除 user=%s agg=%s", short(uid), short(id))
		writeJSON(w, 200, map[string]any{"ok": true, "id": id})
		return true, nil
	}

	if r.Method != http.MethodPost || !strings.HasSuffix(path, "/v1/files") {
		return false, nil
	}

	boundary, err := multipartBoundary(r)
	if err != nil {
		return false, nil // 不是合法 multipart，交回原代理
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var (
		filename string
		data     []byte
		purpose  string
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, nil
		}
		switch part.FormName() {
		case "purpose":
			b, _ := io.ReadAll(io.LimitReader(part, 1<<16))
			purpose = strings.TrimSpace(string(b))
		case "file":
			filename = part.FileName()
			// 单图上限 32MiB（内联），留足余量
			b, err := io.ReadAll(io.LimitReader(part, 48<<20))
			if err != nil {
				return false, err
			}
			data = b
		}
		part.Close()
	}
	if len(data) == 0 {
		return false, nil
	}
	// 非图片（purpose 有值 = 文档类）不切，交回原代理
	if purpose != "" {
		return false, nil
	}

	ids, err := sliceAndUpload(filename, data)
	if err != nil {
		// 切分/上传失败必须回退原样透传，让上游给出权威报错，
		// 不能在这里把用户的附件弄丢
		log.Printf("!! 切分失败，回退原样上传 name=%s: %v", filename, err)
		return false, nil
	}
	if len(ids) == 0 {
		return false, nil // 没超限，交回原代理
	}

	agg := sliceIDPrefix + randomToken()[:24]
	if _, err := db.Exec(`INSERT INTO image_slices(agg_id, file_ids, created_at) VALUES(?,?,?)`,
		agg, strings.Join(ids, ","), float64(time.Now().Unix())); err != nil {
		return false, fmt.Errorf("记录切片失败: %w", err)
	}
	log.Printf("  图片切分上传 user=%s name=%s 片数=%d agg=%s", short(uid), filename, len(ids), short(agg))
	writeJSON(w, 200, map[string]any{
		"id":       agg,
		"object":   "file",
		"filename": filename,
		"bytes":    len(data),
		"slices":   len(ids),
	})
	return true, nil
}

// multipartBoundary 从 Content-Type 里取出 boundary。
// multipart.NewReader 要的是 boundary 本身，直接把整个 Content-Type 传进去
// 会解析不出任何字段（表现为 bufio: buffer full），必须显式拆。
func multipartBoundary(r *http.Request) (string, error) {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return "", errors.New("缺少 Content-Type")
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", err
	}
	b := strings.TrimSpace(params["boundary"])
	if b == "" {
		return "", errors.New("Content-Type 里没有 boundary")
	}
	return b, nil
}

// sliceAndUpload 切分并逐片上传；不需要切分时返回 nil
func sliceAndUpload(filename string, data []byte) ([]string, error) {
	slices, err := fitImage(data, imageMaxSide)
	if err != nil {
		return nil, err
	}
	if len(slices) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(slices))
	for i, s := range slices {
		name := fmt.Sprintf("%s.part%02d.jpg", trimExt(filename), i+1)
		id, err := uploadBytesToUpstream(name, s)
		if err != nil {
			return nil, fmt.Errorf("第 %d 片上传失败: %w", i+1, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
