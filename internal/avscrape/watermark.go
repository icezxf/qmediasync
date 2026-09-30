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

// 水印字体路径（按优先级探测）
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

// WatermarkItem 单个水印项
type WatermarkItem struct {
	Text     string
	BgColor  color.RGBA
	FgColor  color.RGBA
	FontSize float64
}

// buildWatermarks 根据配置 + tag 生成水印列表
func buildWatermarks(r *ScrapeResult, cfg *Config) []WatermarkItem {
	var items []WatermarkItem

	allTags := append([]string{}, r.Genres...)
	allTags = append(allTags, r.ExtraTags...)
	joined := ""
	for _, t := range allTags {
		joined += t + ","
	}
	joined = toLower(joined)

	if cfg.Watermark8K && (contains(joined, "8k") || r.Resolution == "8K") {
		items = append(items, WatermarkItem{"8K", color.RGBA{255, 140, 0, 255}, color.RGBA{255, 255, 255, 255}, 36})
	} else if cfg.Watermark4K && (contains(joined, "4k") || r.Resolution == "4K") {
		items = append(items, WatermarkItem{"4K", color.RGBA{255, 215, 0, 255}, color.RGBA{0, 0, 0, 255}, 36})
	}

	if cfg.WatermarkSubtitle && (r.HasChineseSub || contains(joined, "字幕") || contains(joined, "中字")) {
		items = append(items, WatermarkItem{"字幕", color.RGBA{60, 179, 113, 255}, color.RGBA{255, 255, 255, 255}, 32})
	}

	if cfg.WatermarkCrack && contains(joined, "破解") {
		items = append(items, WatermarkItem{"破解", color.RGBA{220, 20, 60, 255}, color.RGBA{255, 255, 255, 255}, 32})
	}

	if cfg.WatermarkLeak && contains(joined, "流出") {
		items = append(items, WatermarkItem{"流出", color.RGBA{255, 69, 0, 255}, color.RGBA{255, 255, 255, 255}, 32})
	}

	if cfg.WatermarkUncensored && (r.IsUncensored || contains(joined, "无码")) {
		items = append(items, WatermarkItem{"无码", color.RGBA{30, 144, 255, 255}, color.RGBA{255, 255, 255, 255}, 32})
	}

	return items
}

// 小工具，避免引入 strings 的间接依赖
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

// applyWatermark 在图片左上角叠加水印
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

	if err := dc.LoadFontFace(fontPath, 32); err != nil {
		helpers.AppLogger.Warnf("[AV水印] 加载字体失败: %v", err)
		return imgData, nil
	}

	padX := float64(w) * 0.02
	padY := float64(h) * 0.02
	curX := padX
	curY := padY
	rowH := 0.0

	for _, item := range items {
		if err := dc.LoadFontFace(fontPath, item.FontSize); err != nil {
			continue
		}
		textW, textH := dc.MeasureString(item.Text)
		boxW := textW + item.FontSize*0.8
		boxH := textH + item.FontSize*0.5

		if curX+boxW > float64(w)-padX {
			curX = padX
			curY += rowH + padY*0.3
			rowH = 0
		}

		dc.SetColor(item.BgColor)
		dc.DrawRoundedRectangle(curX, curY, boxW, boxH, boxH*0.25)
		dc.Fill()

		dc.SetColor(item.FgColor)
		dc.DrawStringAnchored(item.Text, curX+boxW/2, curY+boxH/2, 0.5, 0.5)

		if boxH > rowH {
			rowH = boxH
		}
		curX += boxW + padX*0.3
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dc.Image(), &jpeg.Options{Quality: 92}); err != nil {
		return imgData, err
	}
	return out.Bytes(), nil
}
