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

		// 本地检测
		s.detectLocalMeta(fs, &path, result, fullPath)

		// 计算目标目录
		media := MediaFromResult(result)
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
			s.recordTask(code, fullPath, "failed", "创建目标目录失败: "+err.Error(), result.Source)
			continue
		}

		// 加载配置（水印/tag 开关）
		cfg, cfgErr := LoadConfig(s.DB)
		if cfgErr != nil || cfg == nil {
			def := defaultConfig
			cfg = &def
		}

		// 直接在目标目录写元数据（NFO、图片、剧照、预告片）
		if err := s.writeMetadataToTarget(fs, targetDir, result, cfg); err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
			continue
		}

		// 移动视频文件到目标目录
		if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			suffix := ""
			switch result.Resolution {
			case "4K":
				suffix = "-4k"
			case "8K":
				suffix = "-8k"
			}
			newName := result.Code + suffix + ext
			if err := s.moveVideo(fs, fullPath, targetDir, newName, path.MoveMethod); err != nil {
				s.recordTask(code, fullPath, "failed", "移动视频失败: "+err.Error(), result.Source)
				continue
			}
		}

		s.recordTask(code, fullPath, "done", "", result.Source)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// detectLocalMeta 本地检测
func (s *Scanner) detectLocalMeta(fs FileSystem, path *models.AVPath, r *ScrapeResult, videoPath string) {
	cfg, err := LoadConfig(s.DB)
	if err != nil || cfg == nil {
		def := defaultConfig
		cfg = &def
	}

	r.IsUncensored = detectUncensored(r.Code)
	r.HasChineseSub = detectChineseSub(fs, videoPath)

	if cfg.ExtraTagResolution || cfg.Watermark4K || cfg.Watermark8K {
		if url, err := fs.GetURL(videoPath); err == nil && url != "" {
			if pr, err := probeVideo(url); err == nil {
				r.Resolution = pr.Resolution
				r.IsHDR = pr.IsHDR
			} else {
				helpers.AppLogger.Warnf("[AV探测] %s ffprobe 失败: %v", r.Code, err)
			}
		}
	}

	r.ExtraTags = buildExtraTags(r, cfg)
	if len(r.ExtraTags) > 0 {
		helpers.AppLogger.Infof("[AV探测] %s 附加标签: %v", r.Code, r.ExtraTags)
	}
}

// writeMetadataToTarget 直接把所有元数据写到目标目录
func (s *Scanner) writeMetadataToTarget(fs FileSystem, targetDir string, r *ScrapeResult, cfg *Config) error {
	// ===== 1. 准备图片（下载 + 打水印）=====
	posterData, fanartData, thumbData := s.prepareImages(r, cfg)

	// ===== 2. 上传图片到目标目录 =====
	if posterData != nil {
		if err := fs.Write(targetDir+"/poster.jpg", posterData); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] 写 poster 失败: %v", err)
		} else {
			helpers.AppLogger.Infof("[AV元数据] poster 已上传到目标目录")
		}
	} else {
		r.Poster = ""
	}
	if fanartData != nil {
		if err := fs.Write(targetDir+"/fanart.jpg", fanartData); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] 写 fanart 失败: %v", err)
		}
	} else {
		r.Fanart = ""
	}
	if thumbData != nil {
		if err := fs.Write(targetDir+"/thumb.jpg", thumbData); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] 写 thumb 失败: %v", err)
		}
	}

	// ===== 3. 合并 ExtraTags 到 Genres =====
	r.Genres = mergeUniqueStrings(r.Genres, r.ExtraTags)

	// ===== 4. 上传 NFO 到目标目录 =====
	nfoName := r.Code + ".nfo"
	if err := fs.Write(targetDir+"/"+nfoName, []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}
	helpers.AppLogger.Infof("[AV元数据] NFO 已上传到目标目录")

	// ===== 5. 剧照直接上传到目标目录/extrafanart/ =====
	if len(r.PreviewImages) > 0 {
		_ = fs.MkdirAll(targetDir + "/extrafanart")
		success := 0
		for i, url := range r.PreviewImages {
			if data, err := downloadImage(url); err == nil {
				if err := fs.Write(fmt.Sprintf("%s/extrafanart/scene-%02d.jpg", targetDir, i+1), data); err == nil {
					success++
				}
			}
		}
		helpers.AppLogger.Infof("[AV元数据] 剧照上传完成: %d/%d", success, len(r.PreviewImages))
	}

	// ===== 6. 预告片 =====
	if r.Trailer != "" {
		_ = fs.MkdirAll(targetDir + "/trailers")
		_ = fs.Write(targetDir+"/trailers/trailer.strm", []byte(r.Trailer))
	}

	return nil
}

// prepareImages 下载候选图 + 打水印，返回 poster/fanart/thumb 的字节
func (s *Scanner) prepareImages(r *ScrapeResult, cfg *Config) (posterData, fanartData, thumbData []byte) {
	watermarks := buildWatermarks(r, cfg)
	if len(watermarks) > 0 {
		names := make([]string, 0, len(watermarks))
		for _, w := range watermarks {
			names = append(names, w.Text)
		}
		helpers.AppLogger.Infof("[AV水印] %s 准备打水印: %v", r.Code, names)
	}

	// 1. 找竖版 poster
	for _, url := range r.ImageCandidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil || w == 0 || h == 0 || h <= w {
			continue
		}
		posterData = data
		r.Poster = url
		break
	}
	// 2. 找横版 fanart
	for _, url := range r.ImageCandidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil || w == 0 || h == 0 || h >= w {
			continue
		}
		fanartData = data
		r.Fanart = url
		break
	}

	// 3. poster 兜底 1：DMM 拼接
	if posterData == nil {
		if dmmURL := dmmPosterURL(r.Code); dmmURL != "" {
			if data, err := downloadDMMImage(dmmURL); err == nil {
				posterData = data
				r.Poster = dmmURL
			}
		}
	}
	// 4. poster 兜底 2：从 fanart 右侧裁剪
	if posterData == nil && fanartData != nil {
		if cropped, ok := cropPosterFromFanart(fanartData); ok {
			posterData = cropped
			r.Poster = "poster.jpg"
		}
	}

	// 5. poster 打水印
	if posterData != nil && len(watermarks) > 0 {
		if wm, err := applyWatermark(posterData, watermarks); err == nil {
			posterData = wm
			helpers.AppLogger.Infof("[AV水印] poster 已打水印")
		}
	}
	// 6. thumb = fanart 复制 + 打水印
	if fanartData != nil {
		thumbData = append([]byte(nil), fanartData...)
		if len(watermarks) > 0 {
			if wm, err := applyWatermark(thumbData, watermarks); err == nil {
				thumbData = wm
				helpers.AppLogger.Infof("[AV水印] thumb 已打水印")
			}
		}
	}
	return
}

// moveVideo 移动视频到目标目录
func (s *Scanner) moveVideo(fs FileSystem, srcPath, targetDir, newName, moveMethod string) error {
	switch moveMethod {
	case "copy":
		if err := fs.Copy(srcPath, targetDir); err != nil {
			return err
		}
		if filepath.Base(srcPath) != newName {
			newPath := targetDir + "/" + filepath.Base(srcPath)
			return fs.Rename(newPath, newName)
		}
		return nil
	default:
		return fs.Move(srcPath, targetDir, newName)
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
	r = strings.ReplaceAll(r, "{actor}", sanitizePathSegment(actorName))
	r = strings.ReplaceAll(r, "{num}", sanitizePathSegment(m.Code))
	r = strings.ReplaceAll(r, "{number}", sanitizePathSegment(m.Code))
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
