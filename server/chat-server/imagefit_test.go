package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// mkImage 生成一张纯色测试图
func mkImage(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 255), uint8(y % 255), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPlanFit(t *testing.T) {
	type want struct {
		slices int
		over   bool // 重叠是否应 >= imageMinOverlap
		vert   bool
		scale1 bool // 是否不缩放
	}
	cases := []struct {
		name   string
		w, h   int
		side   int
		expect want
	}{
		{"不超限", 1000, 2000, imageMaxSide, want{1, false, false, true}},
		{"仅高超限-长截图", 1095, 21555, imageMaxSide, want{3, true, true, true}},
		// 16384 正好是 2 倍上限：切 2 片则余量为 0、没有重叠，
		// 为满足「图片间要有重叠」改切 3 片（每片 5462，重叠 512）
		{"仅高超限-刚好2倍", 1000, 16384, imageMaxSide, want{3, true, true, true}},
		{"仅宽超限", 20000, 1000, imageMaxSide, want{3, true, false, true}},
		{"两边超限", 20000, 30000, imageMaxSide, want{2, true, true, false}},
		// 正方形两边超限：缩到短边=8192 后长边也正好 8192，无需再切
		// 轴此时无意义（不切），用 vert=false 占位，只校验片数与缩放
		{"两边超限-正方形", 20000, 20000, imageMaxSide, want{1, false, false, false}},
		{"密集上限4096", 1000, 9000, imageMaxSideDense, want{3, true, true, true}},
	}
	for _, c := range cases {
		p := planFit(c.w, c.h, c.side)
		if p.Slices != c.expect.slices {
			t.Errorf("%s: 片数=%d 期望%d", c.name, p.Slices, c.expect.slices)
		}
		if p.AxisVert != c.expect.vert {
			t.Errorf("%s: 切割轴=%v 期望%v", c.name, p.AxisVert, c.expect.vert)
		}
		if c.expect.scale1 != (p.Scale >= 1) {
			t.Errorf("%s: 缩放=%v 期望不缩放=%v", c.name, p.Scale, c.expect.scale1)
		}
		if c.expect.over && p.Overlap < imageMinOverlap {
			t.Errorf("%s: 重叠=%d 太小", c.name, p.Overlap)
		}
		// 每片必须 <= 上限
		length := c.h
		if !p.AxisVert {
			length = c.w
		}
		if p.Scale < 1 {
			short := float64(c.w)
			if float64(c.h) < short {
				short = float64(c.h)
			}
			length = int(float64(length) * p.Scale)
		}
		for _, rg := range sliceRanges(length, p.Slices, p.Overlap) {
			if rg[1]-rg[0] > c.side {
				t.Errorf("%s: 片 %v 超过上限 %d", c.name, rg, c.side)
			}
		}
	}
}

func TestSliceRangesCoverSeamlessly(t *testing.T) {
	for _, tc := range [][2]int{{21555, 3}, {16384, 2}, {9000, 3}, {20000, 5}} {
		length, slices := tc[0], tc[1]
		p := planFit(1000, length, imageMaxSide)
		rg := sliceRanges(length, slices, p.Overlap)
		if rg[0][0] != 0 || rg[len(rg)-1][1] != length {
			t.Fatalf("未覆盖整段: %v", rg)
		}
		for i := 1; i < len(rg); i++ {
			if rg[i][0] > rg[i-1][1] {
				t.Fatalf("有缝隙: %v", rg)
			}
			if overlap := rg[i-1][1] - rg[i][0]; p.Overlap > 0 && overlap < p.Overlap {
				t.Fatalf("重叠不足: %v overlap=%d", rg, p.Overlap)
			}
		}
	}
}

func TestFitImageReal(t *testing.T) {
	// 1095 x 21555 的长截图，单边超 8192
	data := mkImage(t, 1095, 21555)
	slices, err := fitImage(data, imageMaxSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(slices) < 2 {
		t.Fatalf("应该切成多片，得到 %d", len(slices))
	}
	total := 0
	for i, s := range slices {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(s))
		if err != nil {
			t.Fatalf("第 %d 片无法解码: %v", i, err)
		}
		if cfg.Width > imageMaxSide || cfg.Height > imageMaxSide {
			t.Errorf("第 %d 片仍超限: %dx%d", i, cfg.Width, cfg.Height)
		}
		total += cfg.Height
	}
	// 无缝覆盖 + 重叠，累计高度应 >= 原高
	if total < 21555 {
		t.Errorf("累计高度 %d < 原高 21555，有内容丢失", total)
	}
	t.Logf("切成 %d 片，累计高度 %d（原 21555）", len(slices), total)
}

func TestFitImageNoSlice(t *testing.T) {
	data := mkImage(t, 1000, 2000)
	if got, err := fitImage(data, imageMaxSide); err != nil || got != nil {
		t.Fatalf("合规图片不该切: %v %v", got, err)
	}
}

func TestImageOverLimits(t *testing.T) {
	data := mkImage(t, 1095, 21555)
	w, h, over := imageOverLimits(data, imageMaxSide)
	if !over || w != 1095 || h != 21555 {
		t.Fatalf("w=%d h=%d over=%v", w, h, over)
	}
	if _, _, over := imageOverLimits([]byte("not an image"), imageMaxSide); over {
		t.Fatal("非图片不应判为超限")
	}
}
