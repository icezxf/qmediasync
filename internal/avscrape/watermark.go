package avscrape

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"

	"github.com/fogleman/gg"

	"Q115-STRM/internal/helpers"
)

var watermarkFontPaths = []string{
	"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/noto-cjk/NotoSansCJKsc-Regular.otf",
	"/usr/share/fonts/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
}

func findFontPath() string {
	for _, p := range watermarkFontPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

type WatermarkItem struct {
	Text     string
	BgColor  color.RGBA
	FgColor  color.RGBA
	FontSize float64
	Bold     bool
}

// buildWatermarks 按 MDC-NG 风格构建水印
// 4K/8K：黄底黑字（徽章感）
// 其他：深色圆角矩形 + 白字
func buildWatermarks(r *ScrapeResult, cfg *Config) []WatermarkItem {
	var items []WatermarkItem
	allTags := append([]string{}, r.Genres...)
	allTags = append(allTags, r.ExtraTags...)
	joined := toLower(strings_Join(allTags, ","))

	if cfg.Watermark8K && (contains(joined, "8k") || r.Resolution == "8K") {
		items = append(items, WatermarkItem{
			Text: "8K", BgColor: color.RGBA{255, 102, 0, 255}, FgColor: color.RGBA{255, 255, 255, 255}, FontSize: 40, Bold: true,
		})
	} else if cfg.Watermark4K && (contains(joined, "4k") || r.Resolution == "4K") {
		items = append(items, WatermarkItem{
			Text: "4K", BgColor: color.RGBA{255, 204, 0, 255}, FgColor: color.RGBA{0, 0, 0, 255}, FontSize: 40, Bold: true,
		})
	}
	if cfg.WatermarkSubtitle && (r.HasChineseSub || contains(joined, "字幕") || contains(joined, "中字")) {
		items = append(items, WatermarkItem{
			Text: "字幕", BgColor: color.RGBA{34, 170, 68, 255}, FgColor: color.RGBA{255, 255, 255, 255}, FontSize: 28,
		})
	}
	if cfg.WatermarkCrack && contains(joined, "破解") {
		items = append(items, WatermarkItem{
			Text: "破解", BgColor: color.RGBA{204, 0, 34, 255}, FgColor: color.RGBA{255, 255, 255, 255}, FontSize: 28,
		})
	}
	if cfg.WatermarkLeak && contains(joined, "流出") {
		items = append(items, WatermarkItem{
			Text: "流出", BgColor: color.RGBA{255, 68, 0, 255}, FgColor: color.RGBA{255, 255, 255, 255}, FontSize: 28,
		})
	}
	if cfg.WatermarkUncensored && (r.IsUncensored || contains(joined, "无码")) {
		items = append(items, WatermarkItem{
			Text: "无码", BgColor: color.RGBA{0, 102, 204, 255}, FgColor: color.RGBA{255, 255, 255, 255}, FontSize: 28,
		})
	}
	return items
}

// 自写的小工具（避免 import strings 与项目其他包冲突）
func strings_Join(arr []string, sep string) string {
	if len(arr) == 0 {
		return ""
	}
	var b []byte
	for i, s := range arr {
		if i > 0 {
			b = append(b, sep...)
		}
		b = append(b, s...)
	}
	return string(b)
}

func toLower(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c = c + 32
		}
		out = append(out, c)
	}
	return string(out)
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// applyWatermark 左上角紧凑排列
func applyWatermark(imgData []byte, items []WatermarkItem) ([]byte, error) {
	if len(items) == 0 {
		return imgData, nil
	}
	fontPath := findFontPath()
	if fontPath == "" {
		helpers.AppLogger.Warnf("[AV水印] 未找到中文字体，跳过水印")
		return imgData, nil
	}

	src, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return imgData, err
	}
	bounds := src.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()

	dc := gg.NewContext(w, h)
	dc.DrawImage(src, 0, 0)

	// 边距按图片宽度比例，MDC-NG 是左上角紧贴
	padX := float64(w) * 0.015
	padY := float64(h) * 0.015
	curX := padX
	curY := padY
	rowH := 0.0
	gap := padX * 0.4

	for _, item := range items {
		if err := dc.LoadFontFace(fontPath, item.FontSize); err != nil {
			continue
		}
		textW, textH := dc.MeasureString(item.Text)
		// 4K/8K 做成正方形徽章感
		var boxW, boxH float64
		if item.Text == "4K" || item.Text == "8K" {
			size := item.FontSize * 1.7
			boxW = size
			boxH = size
		} else {
			boxW = textW + item.FontSize*0.9
			boxH = textH + item.FontSize*0.45
		}

		if curX+boxW > float64(w)-padX {
			curX = padX
			curY += rowH + gap
			rowH = 0
		}

		// 背景
		dc.SetColor(item.BgColor)
		if item.Text == "4K" || item.Text == "8K" {
			dc.DrawRectangle(curX, curY, boxW, boxH)
		} else {
			dc.DrawRoundedRectangle(curX, curY, boxW, boxH, boxH*0.2)
		}
		dc.Fill()

		// 文字
		dc.SetColor(item.FgColor)
		dc.DrawStringAnchored(item.Text, curX+boxW/2, curY+boxH/2, 0.5, 0.5)

		if boxH > rowH {
			rowH = boxH
		}
		curX += boxW + gap
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dc.Image(), &jpeg.Options{Quality: 92}); err != nil {
		return imgData, err
	}
	return out.Bytes(), nil
}
