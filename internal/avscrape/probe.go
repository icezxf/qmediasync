package avscrape

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/v115open"
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
	Oshash     string
	FileSize   int64
}

// downloadHead 下载头部 N 字节，同时返回文件总大小
func downloadHead(url string, nBytes int64) (string, int64, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", v115open.DEFAULTUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", nBytes-1))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 从 Content-Range 解析文件总大小
	var fileSize int64
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if slash := strings.LastIndex(cr, "/"); slash > 0 {
			fmt.Sscanf(cr[slash+1:], "%d", &fileSize)
		}
	}
	if fileSize == 0 {
		fmt.Sscanf(resp.Header.Get("Content-Length"), "%d", &fileSize)
	}

	tmpFile, err := os.CreateTemp("", "avprobe-*")
	if err != nil {
		return "", 0, err
	}
	tmpPath := tmpFile.Name()

	written, err := io.Copy(tmpFile, io.LimitReader(resp.Body, nBytes))
	tmpFile.Close()
	if err != nil {
		os.Remove(tmpPath)
		return "", 0, err
	}
	helpers.AppLogger.Infof("[AV探测] 已下载头部 %d 字节，文件总大小 %d", written, fileSize)
	return tmpPath, fileSize, nil
}

// downloadTail 下载文件尾部 64KB
func downloadTail(url string, fileSize int64) ([]byte, error) {
	if fileSize < 128*1024 {
		return nil, fmt.Errorf("文件太小")
	}
	start := fileSize - 64*1024
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", v115open.DEFAULTUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, fileSize-1))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// computeOshash 计算 OpenSubtitles Hash
func computeOshash(head, tail []byte, fileSize int64) string {
	const chunkSize = 64 * 1024
	if fileSize < 2*chunkSize {
		return ""
	}
	var hash uint64 = uint64(fileSize)
	for i := 0; i+8 <= len(head) && i < chunkSize; i += 8 {
		hash += binary.LittleEndian.Uint64(head[i : i+8])
	}
	for i := 0; i+8 <= len(tail) && i < chunkSize; i += 8 {
		hash += binary.LittleEndian.Uint64(tail[i : i+8])
	}
	return fmt.Sprintf("%016x", hash)
}

// probeVideo 下载头部 + 尾部 → ffprobe 读本地 → 算 oshash
func probeVideo(videoURL string) (*probeResult, error) {
	if videoURL == "" {
		return nil, fmt.Errorf("空 URL")
	}

	const headSize = 2 * 1024 * 1024
	tmpPath, fileSize, err := downloadHead(videoURL, headSize)
	if err != nil {
		return nil, fmt.Errorf("下载文件头失败: %w", err)
	}
	defer os.Remove(tmpPath)

	headData, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-analyzeduration", "5000000",
		"-probesize", "2000000",
		tmpPath,
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
		FileSize:   fileSize,
	}

	// 计算 oshash（多一次尾部请求）
	if fileSize > 128*1024 {
		tailData, err := downloadTail(videoURL, fileSize)
		if err == nil {
			pr.Oshash = computeOshash(headData, tailData, fileSize)
			if pr.Oshash != "" {
				helpers.AppLogger.Infof("[AV探测] oshash=%s", pr.Oshash)
			}
		} else {
			helpers.AppLogger.Warnf("[AV探测] 下载尾部失败，跳过 oshash: %v", err)
		}
	}

	helpers.AppLogger.Infof("[AV探测] %dx%d → %s, HDR=%v", s.Width, s.Height, pr.Resolution, pr.IsHDR)
	return pr, nil
}

// classifyResolution 按长边判断，覆盖普通 + VR 各种分辨率
func classifyResolution(w, h int) string {
	if w == 0 && h == 0 {
		return ""
	}
	base := w
	if h > w {
		base = h
	}
	switch {
	case base >= 7680:
		return "8K"
	case base >= 7168:
		return "7K"
	case base >= 5760:
		return "6K"
	case base >= 4800:
		return "5K"
	case base >= 3840:
		return "4K"
	case base >= 2560:
		return "2K"
	case base >= 1920:
		return "1080p"
	case base >= 1280:
		return "720p"
	case base >= 854:
		return "480p"
	default:
		return fmt.Sprintf("%dp", base)
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
