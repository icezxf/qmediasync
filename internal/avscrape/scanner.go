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

// 匹配 cd1/cd2/part1/part2 等多碟后缀
var cdPartRegex = regexp.MustCompile(`(?i)([-_]?(cd|part|disc|disk)\d+)`)

type Scanner struct {
	DB  *gorm.DB
	Svc *Service
}

func NewScanner(db *gorm.DB) *Scanner {
	return &Scanner{DB: db, Svc: NewService(db)}
}

func (s *Scanner) scanDirRecursive(fs FileSystem, dir string) []string {
	entries, err := fs.ListDetailed(dir)
	if err != nil {
		helpers.AppLogger.Warnf("[AV扫描] 列出目录失败 %s: %v", dir, err)
		return nil
	}
	var videos []string
	for _, e := range entries {
		if e.IsDir {
			sub := s.scanDirRecursive(fs, e.Path)
			videos = append(videos, sub...)
			continue
		}
		if videoExts[strings.ToLower(filepath.Ext(e.Name))] {
			videos = append(videos, e.Path)
		}
	}
	return videos
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

	videos := s.scanDirRecursive(fs, path.SourcePath)
	helpers.AppLogger.Infof("[AV扫描] 目录 %s 递归找到 %d 个视频文件", path.SourcePath, len(videos))

	if len(videos) == 0 {
		s.DB.Model(&path).Update("last_scan_at", time.Now())
		return nil
	}

	cfg, cfgErr := LoadConfig(s.DB)
	if cfgErr != nil || cfg == nil {
		def := defaultConfig
		cfg = &def
	}

	// ===== 按番号分组（多碟处理）=====
	groups := make(map[string][]string)
	var order []string
	for _, fullPath := range videos {
		name := filepath.Base(fullPath)
		code := ExtractCode(name)
		if code == "" {
			s.recordTask("", fullPath, "failed", "无法识别番号", "")
			continue
		}
		if _, exists := groups[code]; !exists {
			order = append(order, code)
		}
		groups[code] = append(groups[code], fullPath)
	}

	// ===== 逐组处理 =====
	for _, code := range order {
		files := groups[code]
		primaryFile := files[0]
		helpers.AppLogger.Infof("[AV扫描] 番号 %s 共 %d 个文件，主文件: %s", code, len(files), filepath.Base(primaryFile))

		// 1. 探测主文件（分辨率 + oshash）
		var resolution string
		var isHDR bool
		var oshash string
		if url, err := fs.GetURL(primaryFile); err == nil && url != "" {
			if pr, err := probeVideo(url); err == nil {
				resolution = pr.Resolution
				isHDR = pr.IsHDR
				oshash = pr.Oshash
			} else {
				helpers.AppLogger.Warnf("[AV探测] %s 失败: %v", code, err)
			}
		}

		// 2. 刮削（传入 oshash）
		result, err := s.Svc.Scrape(code, oshash)
		if err != nil {
			s.recordTask(code, primaryFile, "failed", err.Error(), "")
			continue
		}
		result.Resolution = resolution
		result.IsHDR = isHDR
		result.Oshash = oshash

		// 3. 本地检测
		result.IsUncensored = detectUncensored(code)
		result.HasChineseSub = detectChineseSub(fs, primaryFile)
		result.ExtraTags = buildExtraTags(result, cfg)
		if len(result.ExtraTags) > 0 {
			helpers.AppLogger.Infof("[AV探测] %s 附加标签: %v", code, result.ExtraTags)
		}

		// 4. 目标目录
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
			s.recordTask(code, primaryFile, "failed", "创建目标目录失败: "+err.Error(), result.Source)
			continue
		}

		// 5. 写元数据（一份 NFO + 一份图片）
		if err := s.writeMetadataToTarget(fs, targetDir, result, cfg); err != nil {
			s.recordTask(code, primaryFile, "failed", err.Error(), result.Source)
			continue
		}

		// 6. 移动所有 CD 文件（保留 cdN 后缀，分辨率后缀插在中间）
		if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			suffix := ""
			switch resolution {
			case "8K":
				suffix = "-8k"
			case "7K":
				suffix = "-7k"
			case "6K":
				suffix = "-6k"
			case "5K":
				suffix = "-5k"
			case "4K":
				suffix = "-4k"
			}
			for _, f := range files {
				originalName := filepath.Base(f)
				ext := filepath.Ext(originalName)
				baseName := strings.TrimSuffix(originalName, ext)

				// 提取 cdN / partN 后缀，插在分辨率后缀之后
				cdSuffix := ""
				if m := cdPartRegex.FindString(baseName); m != "" {
					cdSuffix = m
					baseName = strings.Replace(baseName, m, "", 1)
				}
				newName := baseName + suffix + cdSuffix + ext

				if err := s.moveVideo(fs, f, targetDir, newName, path.MoveMethod); err != nil {
					helpers.AppLogger.Warnf("[AV整理] 移动 %s 失败: %v", originalName, err)
				}
			}
		}

		// 7. 写 oshash 文件（主文件一份）
		if oshash != "" {
			_ = fs.Write(fmt.Sprintf("%s/%s.oshash", targetDir, code), []byte(oshash))
		}

		s.recordTask(code, primaryFile, "done", fmt.Sprintf("共 %d 个文件", len(files)), result.Source)
	}

	s.DB.Model(&path).Update("last_scan_at", time.Now())
	return nil
}

func (s *Scanner) writeMetadataToTarget(fs FileSystem, targetDir string, r *ScrapeResult, cfg *Config) error {
	posterData, fanartData, thumbData := s.prepareImages(r, cfg)

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

	r.Genres = mergeUniqueStrings(r.Genres, r.ExtraTags)

	nfoName := r.Code + ".nfo"
	if err := fs.Write(targetDir+"/"+nfoName, []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}
	helpers.AppLogger.Infof("[AV元数据] NFO 已上传到目标目录")

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

	if r.Trailer != "" {
		_ = fs.MkdirAll(targetDir + "/trailers")
		_ = fs.Write(targetDir+"/trailers/trailer.strm", []byte(r.Trailer))
	}
	return nil
}

func (s *Scanner) prepareImages(r *ScrapeResult, cfg *Config) (posterData, fanartData, thumbData []byte) {
	watermarks := buildWatermarks(r, cfg)
	if len(watermarks) > 0 {
		names := make([]string, 0, len(watermarks))
		for _, w := range watermarks {
			names = append(names, w.Label)
		}
		helpers.AppLogger.Infof("[AV水印] %s 准备打水印: %v", r.Code, names)
	}

	for _, url := range r.ImageCandidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil || w == 0 || h == 0 || h <= w {
			continue
		}
		posterData = data
		r.Poster = url
		break
	}
	for _, url := range r.ImageCandidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil || w == 0 || h == 0 || h >= w {
			continue
		}
		fanartData = data
		r.Fanart = url
		break
	}

	if posterData == nil {
		if dmmURL := dmmPosterURL(r.Code); dmmURL != "" {
			if data, err := downloadDMMImage(dmmURL); err == nil {
				posterData = data
				r.Poster = dmmURL
			}
		}
	}
	if posterData == nil && fanartData != nil {
		if cropped, ok := cropPosterFromFanart(fanartData); ok {
			posterData = cropped
			r.Poster = "poster.jpg"
		}
	}

	if posterData != nil && len(watermarks) > 0 {
		if wm, err := applyWatermark(posterData, watermarks); err == nil {
			posterData = wm
			helpers.AppLogger.Infof("[AV水印] poster 已打水印")
		}
	}
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

func renderFolderTemplate(tpl string, m *models.AVMedia) string {
	if tpl == "" {
		tpl = "{code}"
	}
	actorName := ""
	if m.Actors != "" {
		var list []Actor
		if err := json.Unmarshal([]byte(m.Actors), &list); err == nil && len(list) > 0 {
			names := make([]string, 0, len(list))
			for _, a := range list {
				if a.Name != "" {
					names = append(names, a.Name)
				}
			}
			switch {
			case len(names) == 1:
				actorName = names[0]
			case len(names) >= 2 && len(names) <= 3:
				actorName = strings.Join(names, ",")
			case len(names) > 3:
				actorName = "多人作品"
			}
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
