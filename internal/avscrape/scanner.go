package avscrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"

	"gorm.io/gorm"
)

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".wmv": true,
	".mov": true, ".flv": true, ".ts": true, ".m2ts": true,
	".iso": true, ".rmvb": true, ".strm": true,
}

var invalidPathChars = regexp.MustCompile(`[\\/:*?"<>|]`)

type Scanner struct {
	DB  *gorm.DB
	Svc *Service
}

func NewScanner(db *gorm.DB) *Scanner {
	return &Scanner{DB: db, Svc: NewService(db)}
}

func (s *Scanner) Scan(pathID uint) error {
	var path models.AVPath
	if err := s.DB.First(&path, pathID).Error; err != nil {
		return err
	}
	if !path.Enable {
		return fmt.Errorf("目录未启用: %d", pathID)
	}

	fs, err := NewFileSystem(&path)
	if err != nil {
		return fmt.Errorf("创建文件系统失败: %w", err)
	}

	files, err := fs.List(path.SourcePath)
	if err != nil {
		return fmt.Errorf("列出目录失败: %w", err)
	}

	helpers.AppLogger.Infof("[AV扫描] 目录 %s 共找到 %d 个文件", path.SourcePath, len(files))

	for _, name := range files {
		ext := strings.ToLower(filepath.Ext(name))
		if !videoExts[ext] {
			continue
		}
		fullPath := path.SourcePath + "/" + name
		code := ExtractCode(name)
		if code == "" {
			s.recordTask("", fullPath, "failed", "无法识别番号", "")
			continue
		}
		helpers.AppLogger.Infof("[AV扫描] 处理文件 %s → 番号 %s", fullPath, code)

		result, err := s.Svc.Scrape(code)
		if err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), "")
			continue
		}

		// ===== 本地检测：分辨率、有码无码、中文字幕 =====
		s.detectLocalMeta(fs, &path, result, fullPath)

		// ===== 写 NFO + 图片 =====
		if err := s.writeMediaFiles(fs, &path, result, fullPath); err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
			continue
		}

		// ===== 整理文件 =====
		if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			media := MediaFromResult(result)
			if err := s.organize(fs, &path, media, result, fullPath, name); err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
				continue
			}
		}

		s.recordTask(code, fullPath, "done", "", result.Source)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// detectLocalMeta 本地检测：分辨率、有码无码、中文字幕
func (s *Scanner) detectLocalMeta(fs FileSystem, path *models.AVPath, r *ScrapeResult, videoPath string) {
	cfg, err := LoadConfig(s.DB)
	if err != nil || cfg == nil {
		def := defaultConfig
		cfg = &def
	}

	// 1. 无码判断（番号前缀，零成本）
	r.IsUncensored = detectUncensored(r.Code)
	if r.IsUncensored {
		helpers.AppLogger.Infof("[AV探测] %s 判定为无码", r.Code)
	}

	// 2. 中文字幕检测（文件名 + 外挂字幕）
	r.HasChineseSub = detectChineseSub(fs, videoPath)
	if r.HasChineseSub {
		helpers.AppLogger.Infof("[AV探测] %s 检测到中文字幕", r.Code)
	}

	// 3. 分辨率（ffprobe 读直链，只读头 2MB）
	if cfg.ExtraTagResolution || cfg.Watermark4K || cfg.Watermark8K {
		if url, err := fs.GetURL(videoPath); err == nil && url != "" {
			if pr, err := probeVideo(url); err == nil {
				r.Resolution = pr.Resolution
				r.IsHDR = pr.IsHDR
			} else {
				helpers.AppLogger.Warnf("[AV探测] %s ffprobe 失败: %v", r.Code, err)
			}
		} else if err != nil {
			helpers.AppLogger.Warnf("[AV探测] %s 获取直链失败: %v", r.Code, err)
		}
	}

	// 4. 构建附加 tag
	r.ExtraTags = buildExtraTags(r, cfg)
	if len(r.ExtraTags) > 0 {
		helpers.AppLogger.Infof("[AV探测] %s 附加标签: %v", r.Code, r.ExtraTags)
	}
}

// renderFolderTemplate 渲染文件夹模板
func renderFolderTemplate(tpl string, m *models.AVMedia) string {
	if tpl == "" {
		tpl = "{code}"
	}

	actorName := ""
	if m.Actors != "" {
		var list []Actor
		if err := json.Unmarshal([]byte(m.Actors), &list); err == nil && len(list) > 0 {
			actorName = list[0].Name
		}
	}

	year := ""
	if len(m.ReleaseDate) >= 4 {
		year = m.ReleaseDate[:4]
	}

	r := tpl
	r = strings.ReplaceAll(r, "{actors}", sanitizePathSegment(actorName))
	r = strings.ReplaceAll(r, "{num}", sanitizePathSegment(m.Code))
	r = strings.ReplaceAll(r, "{code}", sanitizePathSegment(m.Code))
	r = strings.ReplaceAll(r, "{title}", sanitizePathSegment(m.Title))
	r = strings.ReplaceAll(r, "{year}", sanitizePathSegment(year))
	r = strings.ReplaceAll(r, "{studio}", sanitizePathSegment(m.Studio))
	r = strings.ReplaceAll(r, "{label}", sanitizePathSegment(m.Label))
	r = strings.ReplaceAll(r, "{series}", sanitizePathSegment(m.Series))
	return r
}

func sanitizePathSegment(s string) string {
	s = invalidPathChars.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// writeMediaFiles 写 NFO、下载图片、打水印、生成 .strm 预告片
func (s *Scanner) writeMediaFiles(fs FileSystem, path *models.AVPath, r *ScrapeResult, videoPath string) error {
	cfg, err := LoadConfig(s.DB)
	if err != nil || cfg == nil {
		def := defaultConfig
		cfg = &def
	}

	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	// 水印列表
	watermarks := buildWatermarks(r, cfg)
	if len(watermarks) > 0 {
		names := make([]string, 0, len(watermarks))
		for _, w := range watermarks {
			names = append(names, w.Text)
		}
		helpers.AppLogger.Infof("[AV水印] %s 准备打水印: %v", r.Code, names)
	}

	// ===== 1. poster（只找竖版）=====
	var posterData []byte
	posterOK := false
	for _, url := range r.ImageCandidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil {
			continue
		}
		if w == 0 || h == 0 || h <= w {
			continue
		}
		posterData = data
		r.Poster = url
		posterOK = true
		break
	}

	// ===== 2. fanart（只找横版）=====
	var fanartData []byte
	fanartOK := false
	for _, url := range r.ImageCandidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil {
			continue
		}
		if w == 0 || h == 0 || h >= w {
			continue
		}
		fanartData = data
		r.Fanart = url
		fanartOK = true
		break
	}

	// ===== 3. poster 兜底 1：DMM 拼接 =====
	if !posterOK {
		if dmmURL := dmmPosterURL(r.Code); dmmURL != "" {
			if data, err := downloadDMMImage(dmmURL); err == nil {
				posterData = data
				r.Poster = dmmURL
				posterOK = true
			}
		}
	}

	// ===== 4. poster 兜底 2：从 fanart 右侧裁剪 =====
	if !posterOK && fanartOK {
		if cropped, ok := cropPosterFromFanart(fanartData); ok {
			posterData = cropped
			r.Poster = "poster.jpg (从 fanart 右侧裁剪)"
			posterOK = true
		}
	}

	// ===== 5. poster 打水印 =====
	if posterOK && len(watermarks) > 0 {
		if wm, err := applyWatermark(posterData, watermarks); err == nil {
			posterData = wm
			helpers.AppLogger.Infof("[AV水印] poster 已打水印")
		} else {
			helpers.AppLogger.Warnf("[AV水印] poster 打水印失败: %v", err)
		}
	}

	// ===== 6. thumb = fanart 复制一份 + 打水印 =====
	var thumbData []byte
	thumbOK := false
	if fanartOK {
		thumbData = append([]byte(nil), fanartData...)
		if len(watermarks) > 0 {
			if wm, err := applyWatermark(thumbData, watermarks); err == nil {
				thumbData = wm
				helpers.AppLogger.Infof("[AV水印] thumb 已打水印")
			}
		}
		thumbOK = true
	}

	// ===== 7. 写图 =====
	if posterOK {
		if err := fs.Write(dir+"/poster.jpg", posterData); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] 写 poster 失败: %v", err)
		} else {
			helpers.AppLogger.Infof("[AV元数据] poster 已写入: %dx%d", len(posterData), 0)
		}
	} else {
		helpers.AppLogger.Warnf("[AV元数据] %s 未生成 poster", r.Code)
		r.Poster = ""
	}

	if fanartOK {
		if err := fs.Write(dir+"/fanart.jpg", fanartData); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] 写 fanart 失败: %v", err)
		}
	} else {
		r.Fanart = ""
	}

	if thumbOK {
		if err := fs.Write(dir+"/thumb.jpg", thumbData); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] 写 thumb 失败: %v", err)
		}
	}

	// ===== 8. 合并 ExtraTags 到 Genres（用于 NFO）=====
	r.Genres = mergeUniqueStrings(r.Genres, r.ExtraTags)

	// ===== 9. 写 NFO =====
	if err := fs.Write(dir+"/"+base+".nfo", []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}

	// ===== 10. 剧照 =====
	if len(r.PreviewImages) > 0 {
		_ = fs.MkdirAll(dir + "/extrafanart")
		for i, url := range r.PreviewImages {
			if data, err := downloadImage(url); err == nil {
				_ = fs.Write(fmt.Sprintf("%s/extrafanart/scene-%02d.jpg", dir, i+1), data)
			}
		}
	}

	// ===== 11. 预告片 =====
	if r.Trailer != "" {
		_ = fs.MkdirAll(dir + "/trailers")
		_ = fs.Write(dir+"/trailers/trailer.strm", []byte(r.Trailer))
	}
	return nil
}

// dmmPosterURL 根据番号拼 DMM 竖版海报 URL
func dmmPosterURL(code string) string {
	var sb strings.Builder
	for _, r := range code {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	cleaned := strings.ToUpper(sb.String())

	i := 0
	for i < len(cleaned) && cleaned[i] >= 'A' && cleaned[i] <= 'Z' {
		i++
	}
	letters := cleaned[:i]
	digits := cleaned[i:]

	if letters == "" || digits == "" || len(digits) > 5 {
		return ""
	}

	var num int
	if _, err := fmt.Sscanf(digits, "%d", &num); err != nil {
		return ""
	}
	filename := strings.ToLower(letters) + fmt.Sprintf("%05d", num)
	return "https://pics.dmm.co.jp/digital/video/" + filename + "/" + filename + "pl.jpg"
}

// downloadDMMImage 从 DMM 下载图片
func downloadDMMImage(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", "https://www.dmm.co.jp/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) < 1000 {
		return nil, fmt.Errorf("数据无效（%d 字节）", len(data))
	}
	return data, nil
}

// downloadImageWithSize 下载图片并读取尺寸
func downloadImageWithSize(url string) ([]byte, int, int, error) {
	data, err := downloadImage(url)
	if err != nil {
		return nil, 0, 0, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, 0, 0, nil
	}
	return data, cfg.Width, cfg.Height, nil
}

// organize 按模板整理文件到目标路径
func (s *Scanner) organize(fs FileSystem, path *models.AVPath, media *models.AVMedia, r *ScrapeResult, videoPath, videoName string) error {
	subPath := renderFolderTemplate(path.NameTemplate, media)
	subPath = strings.Trim(subPath, "/")
	for strings.Contains(subPath, "//") {
		subPath = strings.ReplaceAll(subPath, "//", "/")
	}
	if subPath == "" {
		subPath = media.Code
	}
	targetDir := strings.TrimRight(path.TargetPath, "/") + "/" + subPath
	helpers.AppLogger.Infof("[AV整理] 目标目录: %s (模板=%s)", targetDir, path.NameTemplate)

	if err := fs.MkdirAll(targetDir); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}

	ext := filepath.Ext(videoName)
	// 命名后缀：4K/8K
	suffix := ""
	if r != nil {
		switch r.Resolution {
		case "4K":
			suffix = "-4k"
		case "8K":
			suffix = "-8k"
		}
	}
	newName := media.Code + suffix + ext
	srcDir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), ext)

	switch path.MoveMethod {
	case "copy":
		if err := fs.Copy(videoPath, targetDir); err != nil {
			return fmt.Errorf("复制视频失败: %w", err)
		}
		if filepath.Base(videoPath) != newName {
			newPath := targetDir + "/" + filepath.Base(videoPath)
			if err := fs.Rename(newPath, newName); err != nil {
				return fmt.Errorf("重命名视频失败: %w", err)
			}
		}
	default:
		if err := fs.Move(videoPath, targetDir, newName); err != nil {
			return fmt.Errorf("移动视频失败: %w", err)
		}
	}
	helpers.AppLogger.Infof("[AV整理] 移动视频: %s → %s/%s", filepath.Base(videoPath), targetDir, newName)

	nfoSrc := srcDir + "/" + base + ".nfo"
	if fs.Exists(nfoSrc) {
		if err := fs.Move(nfoSrc, targetDir, ""); err != nil {
			helpers.AppLogger.Warnf("[AV整理] 移动 NFO 失败: %v", err)
		} else {
			helpers.AppLogger.Infof("[AV整理] 移动 NFO → %s/", targetDir)
		}
	}

	for _, img := range []string{"poster.jpg", "fanart.jpg", "thumb.jpg"} {
		src := srcDir + "/" + img
		if fs.Exists(src) {
			if err := fs.Move(src, targetDir, ""); err != nil {
				helpers.AppLogger.Warnf("[AV整理] 移动 %s 失败: %v", img, err)
			} else {
				helpers.AppLogger.Infof("[AV整理] 移动 %s → %s/", img, targetDir)
			}
		}
	}

	s.moveDir(fs, srcDir+"/extrafanart", targetDir+"/extrafanart", "extrafanart")
	s.moveDir(fs, srcDir+"/trailers", targetDir+"/trailers", "trailers")

	return nil
}

func (s *Scanner) moveDir(fs FileSystem, src, dst, label string) {
	if !fs.Exists(src) {
		return
	}
	if err := fs.MkdirAll(dst); err != nil {
		helpers.AppLogger.Warnf("[AV整理] 创建 %s 目录失败: %v", label, err)
		return
	}
	names, err := fs.List(src)
	if err != nil {
		helpers.AppLogger.Warnf("[AV整理] 列出 %s 失败: %v", label, err)
		return
	}
	for _, n := range names {
		if err := fs.Move(src+"/"+n, dst, ""); err != nil {
			helpers.AppLogger.Warnf("[AV整理] 移动 %s/%s 失败: %v", label, n, err)
		}
	}
	_ = fs.DeleteDir(src)
	helpers.AppLogger.Infof("[AV整理] 移动 %s/ (%d 项) → %s/", label, len(names), dst)
}

// mergeUniqueStrings 合并两个字符串切片并去重
func mergeUniqueStrings(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, s := range a {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range b {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func (s *Scanner) recordTask(code, filePath, status, msg, provider string) {
	s.DB.Create(&models.AVTask{
		Code:     code,
		FilePath: filePath,
		Status:   status,
		Message:  msg,
		Provider: provider,
	})
}
