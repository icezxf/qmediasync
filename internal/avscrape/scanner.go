package avscrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"

	"gorm.io/gorm"
)

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".wmv": true,
	".mov": true, ".flv": true, ".ts": true, ".m2ts": true,
	".iso": true, ".rmvb": true, ".strm": true,
}

// cdPartRegex 匹配多碟后缀：cd1/cd2/part1/disc1/disk1
var cdPartRegex = regexp.MustCompile(`(?i)([-_]?(cd|part|disc|disk)\d+)`)

type Scanner struct {
	DB  *gorm.DB
	Svc *Service
}

func NewScanner(db *gorm.DB) *Scanner {
	return &Scanner{DB: db, Svc: NewService(db)}
}

// groupItem 同一番号下的多个文件（多碟）
type groupItem struct {
	Code  string
	Files []string
}

// ============================================================
// Scan 主流程
// 1. 遍历目录找所有视频
// 2. 按番号分组（cd1/cd2 归为一组）
// 3. 每组刮削一次、探测一次、一份 NFO、所有文件都移动
// ============================================================
func (s *Scanner) Scan(pathID uint) error {
	var path models.AVPath
	if err := s.DB.First(&path, pathID).Error; err != nil {
		return err
	}
	if !path.Enable {
		return fmt.Errorf("目录未启用: %d", pathID)
	}

	cfg, cfgErr := LoadConfig(s.DB)
	if cfgErr != nil || cfg == nil {
		def := defaultConfig
		cfg = &def
	}

	fs, err := NewFileSystem(&path)
	if err != nil {
		return fmt.Errorf("创建文件系统失败: %w", err)
	}

	videoFiles, err := walkVideos(fs, path.SourcePath, 0)
	if err != nil {
		return fmt.Errorf("遍历目录失败: %w", err)
	}
	helpers.AppLogger.Infof("[AV扫描] 目录 %s 共找到 %d 个视频文件", path.SourcePath, len(videoFiles))

	// ===== 按番号分组 =====
	groups := make(map[string]*groupItem)
	var order []string
	for _, fullPath := range videoFiles {
		name := filepath.Base(fullPath)
		code := ExtractCode(name)
		if code == "" {
			helpers.AppLogger.Warnf("[AV扫描] 无法识别番号: %s", fullPath)
			s.recordTask("", fullPath, "failed", "无法识别番号", "")
			continue
		}
		if _, ok := groups[code]; !ok {
			groups[code] = &groupItem{Code: code}
			order = append(order, code)
		}
		groups[code].Files = append(groups[code].Files, fullPath)
	}

	// ===== 逐组处理 =====
	for _, code := range order {
		g := groups[code]
		primaryFile := g.Files[0]
		helpers.AppLogger.Infof("[AV扫描] 番号 %s 共 %d 个文件，主文件: %s",
			code, len(g.Files), filepath.Base(primaryFile))

		var existing models.AVMedia
		hasExisting := s.DB.Where("code = ?", code).First(&existing).Error == nil

		var result *ScrapeResult
		if hasExisting {
			result = mediaToScrapeResult(&existing)
		} else {
			r, err := s.Svc.Scrape(code, "")
			if err != nil {
				s.recordTask(code, primaryFile, "failed", err.Error(), "")
				continue
			}
			result = r
		}

		// 用主文件做探测
		s.detectVideoMeta(fs, primaryFile, result, cfg)

		if path.Mode == "scrape_only" {
			// 仅刮削：每个文件所在目录都写一份元数据
			for _, f := range g.Files {
				fName := filepath.Base(f)
				tmpDir := filepath.Dir(f)
				baseName := strings.TrimSuffix(fName, filepath.Ext(fName))
				files, err := s.prepareMetaFiles(baseName, result, cfg)
				if err != nil {
					helpers.AppLogger.Warnf("[AV扫描] 准备元数据失败 %s: %v", fName, err)
					continue
				}
				if _, err := fs.QueueUploads(files, tmpDir, path.AccountID, path.SourceType); err != nil {
					helpers.AppLogger.Warnf("[AV扫描] 加入上传队列失败 %s: %v", fName, err)
				}
			}
		} else if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			if err := s.organize(fs, &path, MediaFromResult(result), g.Files, result, cfg); err != nil {
				s.recordTask(code, primaryFile, "failed", err.Error(), result.Source)
				continue
			}
		}

		msg := fmt.Sprintf("共 %d 个文件", len(g.Files))
		if hasExisting {
			msg = "已存在，重新整理完成"
		}
		s.recordTask(code, primaryFile, "done", msg, result.Source)
	}

	if path.Mode != "scrape_only" {
		s.cleanupSourceDir(fs, path.SourcePath)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// ============================================================
// 探测视频信息（ffprobe + 附加标签）
// ============================================================
func (s *Scanner) detectVideoMeta(fs FileSystem, fullPath string, r *ScrapeResult, cfg *Config) {
	if url, err := fs.GetURL(fullPath); err == nil && url != "" {
		helpers.AppLogger.Infof("[AV探测] %s 开始 ffprobe", r.Code)
		if pr, err := probeVideo(url); err == nil {
			r.Resolution = pr.Resolution
			r.IsHDR = pr.IsHDR
			r.Oshash = pr.Oshash
			helpers.AppLogger.Infof("[AV探测] %s 分辨率=%s HDR=%v oshash=%s",
				r.Code, pr.Resolution, pr.IsHDR, pr.Oshash)
		} else {
			helpers.AppLogger.Warnf("[AV探测] %s ffprobe 失败: %v", r.Code, err)
		}
	} else {
		helpers.AppLogger.Warnf("[AV探测] %s 获取直链失败: %v", r.Code, err)
	}

	r.IsUncensored = detectUncensored(r.Code)
	r.HasChineseSub = detectChineseSub(fs, fullPath)
	r.ExtraTags = buildExtraTags(r, cfg)
	if len(r.ExtraTags) > 0 {
		helpers.AppLogger.Infof("[AV探测] %s 附加标签: %v", r.Code, r.ExtraTags)
	}
}

// resolutionSuffix 分辨率 → 文件名后缀
func resolutionSuffix(res string) string {
	switch res {
	case "8K":
		return "-8k"
	case "7K":
		return "-7k"
	case "6K":
		return "-6k"
	case "5K":
		return "-5k"
	case "4K":
		return "-4k"
	}
	return ""
}

// ============================================================
// organize 移动所有 CD 文件到目标目录 + 元数据入上传队列
// 文件名规则：{code}{-分辨率}{-cdN}{ext}
// 例：MIDV-192-cd1.mp4 → MIDV-192-4k-cd1.mp4
// ============================================================
func (s *Scanner) organize(fs FileSystem, path *models.AVPath, media *models.AVMedia, videoPaths []string, r *ScrapeResult, cfg *Config) error {
	relDir := renderTemplate(path.NameTemplate, media)
	if relDir == "" {
		relDir = media.Code
	}
	targetDir := strings.TrimRight(path.TargetPath, "/") + "/" + relDir
	if err := fs.MkdirAll(targetDir); err != nil {
		return err
	}

	// 分辨率后缀
	resSuffix := ""
	if r != nil {
		resSuffix = resolutionSuffix(r.Resolution)
	}

	// ===== 移动所有 CD 文件 =====
	for _, srcPath := range videoPaths {
		originalName := filepath.Base(srcPath)
		ext := filepath.Ext(originalName)
		baseName := strings.TrimSuffix(originalName, ext)

		// 提取 cdN / partN / discN / diskN 后缀
		cdSuffix := ""
		if m := cdPartRegex.FindString(baseName); m != "" {
			cdSuffix = strings.ToLower(m)
		}

		// 新文件名：code + 分辨率后缀 + cdN 后缀 + 扩展名
		newName := media.Code + resSuffix + cdSuffix + ext
		newPath := targetDir + "/" + newName

		if fs.Exists(newPath) {
			helpers.AppLogger.Infof("[AV整理] 目标已存在，跳过: %s", newName)
			continue
		}

		switch path.MoveMethod {
		case "copy":
			if err := fs.Copy(srcPath, targetDir); err != nil {
				helpers.AppLogger.Warnf("[AV整理] 复制失败 %s: %v", srcPath, err)
				continue
			}
			if originalName != newName {
				oldPath := targetDir + "/" + originalName
				if err := fs.Rename(oldPath, newName); err != nil {
					helpers.AppLogger.Warnf("[AV整理] 重命名失败 %s: %v", oldPath, err)
				}
			}
		default:
			if err := fs.Move(srcPath, targetDir, newName); err != nil {
				helpers.AppLogger.Warnf("[AV整理] 移动失败 %s: %v", srcPath, err)
				continue
			}
		}
		helpers.AppLogger.Infof("[AV整理] %s → %s", originalName, newName)
	}

	// ===== 元数据入上传队列（一份）=====
	if r != nil {
		files, err := s.prepareMetaFiles(media.Code, r, cfg)
		if err != nil {
			return err
		}
		if _, err := fs.QueueUploads(files, targetDir, path.AccountID, path.SourceType); err != nil {
			return fmt.Errorf("加入上传队列失败: %w", err)
		}
	}

	return nil
}

// ============================================================
// prepareMetaFiles 生成元数据到本地临时目录
// 图片优先级：ImageCandidates 竖图 > DMM > fanart 裁剪
// ============================================================
func (s *Scanner) prepareMetaFiles(baseName string, r *ScrapeResult, cfg *Config) ([]LocalFile, error) {
	if r == nil {
		return nil, nil
	}

	tmpDir := filepath.Join(helpers.ConfigDir, "tmp", "avscrape", baseName)
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}

	files := []LocalFile{}

	// 1. NFO
	nfoPath := filepath.Join(tmpDir, baseName+".nfo")
	if err := os.WriteFile(nfoPath, []byte(GenerateNFO(r)), 0644); err != nil {
		return nil, fmt.Errorf("写 NFO 失败: %w", err)
	}
	files = append(files, LocalFile{LocalPath: nfoPath, RemoteName: baseName + ".nfo"})

	// 2. 构建水印
	watermarks := buildWatermarks(r, cfg)
	if len(watermarks) > 0 {
		names := make([]string, 0, len(watermarks))
		for _, w := range watermarks {
			names = append(names, w.Label)
		}
		helpers.AppLogger.Infof("[AV水印] %s 准备打水印: %v", r.Code, names)
	}

	// 3. 从 ImageCandidates 按宽高比挑 poster / fanart
	var posterData, fanartData []byte
	for _, url := range r.ImageCandidates {
		data, err := downloadImage(url)
		if err != nil {
			continue
		}
		imgCfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			continue
		}
		if posterData == nil && imgCfg.Height > imgCfg.Width {
			posterData = data
			r.Poster = url
		}
		if fanartData == nil && imgCfg.Height < imgCfg.Width {
			fanartData = data
			r.Fanart = url
		}
		if posterData != nil && fanartData != nil {
			break
		}
	}

	// 4. poster 兜底 1：DMM
	if posterData == nil {
		if dmmURL := dmmPosterURL(r.Code); dmmURL != "" {
			if data, err := downloadDMMImage(dmmURL); err == nil {
				posterData = data
				r.Poster = dmmURL
				helpers.AppLogger.Infof("[AV元数据] poster 使用 DMM 兜底")
			}
		}
	}

	// 5. poster 兜底 2：从 fanart 裁剪
	if posterData == nil && fanartData != nil {
		if cropped, ok := cropPosterFromFanart(fanartData); ok {
			posterData = cropped
			r.Poster = "poster.jpg"
			helpers.AppLogger.Infof("[AV元数据] poster 从 fanart 裁剪")
		}
	}

	// 6. 打水印
	if posterData != nil && len(watermarks) > 0 {
		if wm, err := applyWatermark(posterData, watermarks); err == nil {
			posterData = wm
			helpers.AppLogger.Infof("[AV水印] poster 已打水印")
		}
	}
	if fanartData != nil && len(watermarks) > 0 {
		if wm, err := applyWatermark(fanartData, watermarks); err == nil {
			fanartData = wm
			helpers.AppLogger.Infof("[AV水印] fanart 已打水印")
		}
	}

	// 7. 写 poster
	if posterData != nil {
		p := filepath.Join(tmpDir, "poster.jpg")
		if err := os.WriteFile(p, posterData, 0644); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "poster.jpg"})
		}
	}
	// 8. 写 fanart + thumb
	if fanartData != nil {
		p := filepath.Join(tmpDir, "fanart.jpg")
		if err := os.WriteFile(p, fanartData, 0644); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "fanart.jpg"})
		}
		p2 := filepath.Join(tmpDir, "thumb.jpg")
		if err := os.WriteFile(p2, fanartData, 0644); err == nil {
			files = append(files, LocalFile{LocalPath: p2, RemoteName: "thumb.jpg"})
		}
	}

	// 9. 剧照（不打水印）
	for i, url := range r.PreviewImages {
		remoteName := fmt.Sprintf("extrafanart/scene-%02d.jpg", i+1)
		localPath := filepath.Join(tmpDir, fmt.Sprintf("scene-%02d.jpg", i+1))
		if err := helpers.DownloadFile(url, localPath, ""); err == nil {
			files = append(files, LocalFile{LocalPath: localPath, RemoteName: remoteName})
		} else {
			helpers.AppLogger.Warnf("[AV元数据] 下载剧照 %d 失败: %v", i+1, err)
		}
	}

	// 10. 预告片
	if r.Trailer != "" {
		p := filepath.Join(tmpDir, "trailer.strm")
		if err := os.WriteFile(p, []byte(r.Trailer), 0644); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "trailers/trailer.strm"})
		}
	}

	return files, nil
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

func mediaToScrapeResult(m *models.AVMedia) *ScrapeResult {
	var genres []string
	var actors []Actor
	var previews []string
	var urls []string
	if m.Genres != "" {
		_ = json.Unmarshal([]byte(m.Genres), &genres)
	}
	if m.Actors != "" {
		_ = json.Unmarshal([]byte(m.Actors), &actors)
	}
	if m.PreviewImages != "" {
		_ = json.Unmarshal([]byte(m.PreviewImages), &previews)
	}
	if m.Urls != "" {
		_ = json.Unmarshal([]byte(m.Urls), &urls)
	}
	return &ScrapeResult{
		Code:          m.Code,
		Title:         m.Title,
		OriginalTitle: m.OriginalTitle,
		Plot:          m.Plot,
		Runtime:       m.Runtime,
		ReleaseDate:   m.ReleaseDate,
		Director:      m.Director,
		Studio:        m.Studio,
		Label:         m.Label,
		Series:        m.Series,
		Genres:        genres,
		Actors:        actors,
		Poster:        m.Poster,
		Fanart:        m.Fanart,
		PreviewImages: previews,
		Trailer:       m.Trailer,
		Rating:        m.Rating,
		Urls:          urls,
		Source:        m.Source,
		Oshash:        m.Oshash,
	}
}

func walkVideos(fs FileSystem, root string, depth int) ([]string, error) {
	if depth > 10 {
		return nil, nil
	}
	entries, err := fs.ListDetailed(root)
	if err != nil {
		return nil, err
	}
	var videos []string
	for _, e := range entries {
		if e.IsDir {
			lower := strings.ToLower(e.Name)
			if lower == "extrafanart" || lower == "trailers" ||
				lower == "backdrops" || lower == "thumbnails" ||
				lower == "season" || strings.HasPrefix(lower, "season ") {
				continue
			}
			sub, err := walkVideos(fs, e.Path, depth+1)
			if err == nil {
				videos = append(videos, sub...)
			}
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name))
		if videoExts[ext] {
			videos = append(videos, e.Path)
		}
	}
	return videos, nil
}

func renderTemplate(tpl string, media *models.AVMedia) string {
	if tpl == "" {
		return media.Code
	}
	firstActor := ""
	allActors := ""
	var actors []Actor
	if media.Actors != "" {
		_ = json.Unmarshal([]byte(media.Actors), &actors)
	}
	for i, a := range actors {
		name := pickBestActorName(a)
		if name == "" {
			continue
		}
		if firstActor == "" {
			firstActor = name
		}
		if i > 0 {
			allActors += ", "
		}
		allActors += name
	}

	replacer := strings.NewReplacer(
		"{actor}", sanitizePath(firstActor),
		"{actors}", sanitizePath(allActors),
		"{number}", sanitizePath(media.Code),
		"{code}", sanitizePath(media.Code),
		"{title}", sanitizePath(media.Title),
		"{year}", extractYear(media.ReleaseDate),
		"{studio}", sanitizePath(media.Studio),
		"{label}", sanitizePath(media.Label),
		"{series}", sanitizePath(media.Series),
		"{director}", sanitizePath(media.Director),
	)

	result := replacer.Replace(tpl)
	result = strings.Trim(result, "/")
	for strings.Contains(result, "//") {
		result = strings.ReplaceAll(result, "//", "/")
	}
	parts := strings.Split(result, "/")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return strings.Join(cleaned, "/")
}

// pickBestActorName 优先用 a.Name（Scrape 阶段维基百科已翻成中文名）
func pickBestActorName(a Actor) string {
	if a.Name != "" {
		return a.Name
	}
	if len(a.Aliases) > 0 && a.Aliases[0] != "" {
		return a.Aliases[0]
	}
	return ""
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func sanitizePath(s string) string {
	if s == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "：", "*", "_",
		"?", "？", "\"", "'", "<", "《", ">", "》", "|", "_",
	)
	return strings.TrimSpace(replacer.Replace(s))
}

func extractYear(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}
