package avscrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"Q115-STRM/internal/helpers"
)

// 无码番号前缀
var uncensoredPrefixes = []string{
	"Carib", "carib", "1Pondo", "1pondo", "Heyzo", "HEYZO",
	"Tokyo-Hot", "tokyo-hot", "TokyoHot", "10musume", "10Musume",
	"PacoPacoMama", "pacopacomama", "Mura", "MURA", "Mesubuta", "mesubuta",
	"OrientalDream", "SMD", "smd", "Kirameki", "kirameki",
	"DSAM", "Heaven", "heaven", "Gachinco", "gachinco",
}

// detectUncensored 按番号前缀判断是否无码
func detectUncensored(code string) bool {
	for _, p := range uncensoredPrefixes {
		if strings.HasPrefix(strings.ToLower(code), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// detectChineseSub 从文件名 + 外挂字幕判断是否有中文
func detectChineseSub(fs FileSystem, videoPath string) bool {
	base := filepath.Base(videoPath)
	lower := strings.ToLower(base)

	if strings.Contains(lower, "-c.") ||
		strings.Contains(lower, "-ch.") ||
		strings.Contains(lower, "-c_mkv") ||
		strings.Contains(base, "中文字幕") ||
		strings.Contains(base, "中字") ||
		strings.Contains(lower, "chs") ||
		strings.Contains(lower, "cht") {
		return true
	}

	dir := filepath.Dir(videoPath)
	baseName := strings.TrimSuffix(base, filepath.Ext(base))
	files, err := fs.List(dir)
	if err != nil {
		return false
	}
	subExts := []string{".chs", ".cht", ".zh", ".zh-cn", ".zh-hans", ".zh-hant"}
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f))
		for _, se := range subExts {
			if ext == se && strings.HasPrefix(f, baseName) {
				return true
			}
		}
	}
	return false
}

// probeResult ffprobe 结果
type probeResult struct {
	Resolution string
	IsHDR      bool
}

// probeVideo 通过 URL 用 ffprobe 读取视频信息
// 严格串行、限制读取范围，避免触发 CDN 风控
func probeVideo(videoURL string) (*probeResult, error) {
	if videoURL == "" {
		return nil, fmt.Errorf("空 URL")
	}

	args := []string{
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		"-select_streams", "v:0",
		"-read_intervals", "%+#2M",
		"-analyzeduration", "5000000",
		"-probesize", "2000000",
		videoURL,
	}

	cmd := exec.Command("ffprobe", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("ffprobe 失败: %v, %s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("ffprobe 超时")
	}

	var result struct {
		Streams []struct {
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			PixFmt    string `json:"pix_fmt"`
			ColorTrc  string `json:"color_transfer"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("解析 ffprobe 输出失败: %w", err)
	}
	if len(result.Streams) == 0 {
		return nil, fmt.Errorf("ffprobe 无视频流")
	}

	s := result.Streams[0]
	pr := &probeResult{
		Resolution: classifyResolution(s.Width, s.Height),
		IsHDR:      isHDRPixelFormat(s.PixFmt, s.ColorTrc),
	}
	helpers.AppLogger.Infof("[AV探测] %dx%d → %s, HDR=%v", s.Width, s.Height, pr.Resolution, pr.IsHDR)
	return pr, nil
}

// classifyResolution 宽高 → 分辨率标签
func classifyResolution(w, h int) string {
	if h == 0 {
		return ""
	}
	switch {
	case h >= 4320:
		return "8K"
	case h >= 2160:
		return "4K"
	case h >= 1440:
		return "2K"
	case h >= 1080:
		return "1080p"
	case h >= 720:
		return "720p"
	case h >= 480:
		return "480p"
	default:
		return fmt.Sprintf("%dp", h)
	}
}

// isHDRPixelFormat 判断是否 HDR
func isHDRPixelFormat(pixFmt, colorTrc string) bool {
	if strings.Contains(pixFmt, "10le") || strings.Contains(pixFmt, "12le") {
		return true
	}
	trc := strings.ToLower(colorTrc)
	if trc == "smpte2084" || trc == "arib-std-b67" {
		return true
	}
	return false
}

// buildExtraTags 根据本地检测结果构建附加 tag
func buildExtraTags(r *ScrapeResult, cfg *Config) []string {
	var tags []string

	if cfg.ExtraTagResolution && r.Resolution != "" {
		tags = append(tags, r.Resolution)
		if r.IsHDR {
			tags = append(tags, "HDR")
		}
	}
	if cfg.ExtraTagUncensored {
		if r.IsUncensored {
			tags = append(tags, "无码")
		} else {
			tags = append(tags, "有码")
		}
	}
	if cfg.ExtraTagChineseSub && r.HasChineseSub {
		tags = append(tags, "中文字幕")
	}
	return tags
}
