package avscrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"Q115-STRM/internal/helpers"
)

var uncensoredPrefixes = []string{
	"Carib", "carib", "1Pondo", "1pondo", "Heyzo", "HEYZO",
	"Tokyo-Hot", "tokyo-hot", "TokyoHot", "10musume", "10Musume",
	"PacoPacoMama", "pacopacomama", "Mura", "MURA", "Mesubuta", "mesubuta",
	"OrientalDream", "SMD", "smd", "Kirameki", "kirameki",
	"DSAM", "Heaven", "heaven", "Gachinco", "gachinco",
}

func detectUncensored(code string) bool {
	for _, p := range uncensoredPrefixes {
		if strings.HasPrefix(strings.ToLower(code), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

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

type probeResult struct {
	Resolution string
	IsHDR      bool
}

// wrapURL 如果 URL 是 115 CDN，改走本地 /proxy-115 反代，绕过 UA 检查
// 跟原版刮削模块的做法一致
func wrapURL(videoURL string) string {
	if videoURL == "" {
		return ""
	}
	// 115 CDN 域名特征
	if strings.Contains(videoURL, "115cdn.net") ||
		strings.Contains(videoURL, "115.com") ||
		strings.Contains(videoURL, "anxia.com") {
		wrapped := fmt.Sprintf("http://127.0.0.1:12333/proxy-115?url=%s", url.QueryEscape(videoURL))
		helpers.AppLogger.Infof("[AV探测] 115 直链走本地反代")
		return wrapped
	}
	return videoURL
}

// probeVideo 通过 URL 用 ffprobe 读取视频信息
// 参考原版：115 直链走本地 /proxy-115 反代，反代服务用正确 UA 请求 115 CDN
func probeVideo(videoURL string) (*probeResult, error) {
	if videoURL == "" {
		return nil, fmt.Errorf("空 URL")
	}

	// 115 直链包装成本地反代地址
	videoURL = wrapURL(videoURL)

	args := []string{
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
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
	case <-time.After(60 * time.Second):
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
