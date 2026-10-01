package avscrape

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	videoFiles, err := walkVideos(fs, path.SourcePath, 0)
	if err != nil {
		return fmt.Errorf("遍历目录失败: %w", err)
	}
	helpers.AppLogger.Infof("[AV扫描] 目录 %s 共找到 %d 个视频文件", path.SourcePath, len(videoFiles))

	cfg, cfgErr := LoadConfig(s.DB)
	if cfgErr != nil || cfg == nil {
		def := defaultConfig
		cfg = &def
	}

	for _, fullPath := range videoFiles {
		name := filepath.Base(fullPath)
		code := ExtractCode(name)
		if code == "" {
			helpers.AppLogger.Warnf("[AV扫描] 无法识别番号: %s", fullPath)
			s.recordTask("", fullPath, "failed", "无法识别番号", "")
			continue
		}
		helpers.AppLogger.Infof("[AV扫描] 处理文件 %s → 番号 %s", fullPath, code)

		var existing models.AVMedia
		hasExisting := s.DB.Where("code = ?", code).First(&existing).Error == nil

		var result *ScrapeResult
		if hasExisting {
			result = mediaToScrapeResult(&existing)
		} else {
			r, err := s.Svc.Scrape(code, "")
			if err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), "")
				continue
			}
			result = r
		}

		// ===== 探测视频信息（ffprobe）+ 附加标签 =====
		s.detectVideoMeta(fs, fullPath, result, cfg)
		// ============================================

		if path.Mode == "scrape_only" {
			tmpDir := filepath.Dir(fullPath)
			baseName := strings.TrimSuffix(name, filepath.Ext(name))
			files, err := s.prepareMetaFiles(baseName, result)
			if err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
				continue
			}
			if _, err := fs.QueueUploads(files, tmpDir, path.AccountID, path.SourceType); err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
				continue
			}
		} else if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			if err := s.organize(fs, &path, MediaFromResult(result), fullPath, name, result); err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
				continue
			}
		}

		msg := ""
		if hasExisting {
			msg = "已存在，重新整理完成"
		}
		s.recordTask(code, fullPath, "done", msg, result.Source)
	}

	if path.Mode != "scrape_only" {
		s.cleanupSourceDir(fs, path.SourcePath)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// detectVideoMeta 探测视频分辨率/HDR/oshash + 检测有码/中文字幕 + 生成附加标签
func (s *Scanner) detectVideoMeta(fs FileSystem, fullPath string, r *ScrapeResult, cfg *Config) {
	// 1. ffprobe 探测
	if url, err := fs.GetURL(fullPath); err == nil && url != "" {
		helpers.AppLogger.Infof("[AV探测] %s 开始 ffprobe, URL=%s", r.Code, redactURL(url))
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

	// 2. 检测有码/无码
	r.IsUncensored = detectUncensored(r.Code)

	// 3. 检测中文字幕
	r.HasChineseSub = detectChineseSub(fs, fullPath)

	// 4. 生成附加标签
	r.ExtraTags = buildExtraTags(r, cfg)
	if len(r.ExtraTags) > 0 {
		helpers.AppLogger.Infof("[AV探测] %s 附加标签: %v", r.Code, r.ExtraTags)
	}
}

func (s *Scanner) organize(fs FileSystem, path *models.AVPath, media *models.AVMedia, videoPath, videoName string, r *ScrapeResult) error {
	relDir := renderTemplate(path.NameTemplate, media)
	if relDir == "" {
		relDir = media.Code
	}
	targetDir := path.TargetPath + "/" + relDir
	if err := fs.MkdirAll(targetDir); err != nil {
		return err
	}

	ext := filepath.Ext(videoName)
	newName := media.Code + ext
	newVideoPath := targetDir + "/" + newName

	if !fs.Exists(newVideoPath) {
		switch path.MoveMethod {
		case "copy":
			if err := fs.Copy(videoPath, targetDir); err != nil {
				return fmt.Errorf("复制视频失败: %w", err)
			}
			if filepath.Base(videoPath) != newName {
				oldPath := targetDir + "/" + filepath.Base(videoPath)
				if err := fs.Rename(oldPath, newName); err != nil {
					return fmt.Errorf("重命名视频失败: %w", err)
				}
			}
		default:
			if err := fs.Move(videoPath, targetDir, newName); err != nil {
				return fmt.Errorf("移动视频失败: %w", err)
			}
		}
	}

	if r != nil {
		files, err := s.prepareMetaFiles(media.Code, r)
		if err != nil {
			return err
		}
		if _, err := fs.QueueUploads(files, targetDir, path.AccountID, path.SourceType); err != nil {
			return fmt.Errorf("加入上传队列失败: %w", err)
		}
	}

	return nil
}

func (s *Scanner) prepareMetaFiles(baseName string, r *ScrapeResult) ([]LocalFile, error) {
	if r == nil {
		return nil, nil
	}

	tmpDir := filepath.Join(helpers.ConfigDir, "tmp", "avscrape", baseName)
	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}

	files := []LocalFile{}

	nfoPath := filepath.Join(tmpDir, baseName+".nfo")
	if err := os.WriteFile(nfoPath, []byte(GenerateNFO(r)), 0644); err != nil {
		return nil, fmt.Errorf("写 NFO 失败: %w", err)
	}
	files = append(files, LocalFile{LocalPath: nfoPath, RemoteName: baseName + ".nfo"})

	if r.Poster != "" {
		p := filepath.Join(tmpDir, "poster.jpg")
		if err := helpers.DownloadFile(r.Poster, p, ""); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "poster.jpg"})
		} else {
			helpers.AppLogger.Warnf("[AV元数据] 下载 poster 失败: %v", err)
		}
	}
	if r.Fanart != "" {
		p := filepath.Join(tmpDir, "fanart.jpg")
		if err := helpers.DownloadFile(r.Fanart, p, ""); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "fanart.jpg"})
		} else {
			helpers.AppLogger.Warnf("[AV元数据] 下载 fanart 失败: %v", err)
		}
	}
	for i, url := range r.PreviewImages {
		remoteName := fmt.Sprintf("extrafanart/scene-%02d.jpg", i+1)
		localPath := filepath.Join(tmpDir, fmt.Sprintf("scene-%02d.jpg", i+1))
		if err := helpers.DownloadFile(url, localPath, ""); err == nil {
			files = append(files, LocalFile{LocalPath: localPath, RemoteName: remoteName})
		} else {
			helpers.AppLogger.Warnf("[AV元数据] 下载剧照 %d 失败: %v", i+1, err)
		}
	}
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

func pickBestActorName(a Actor) string {
	for _, alias := range a.Aliases {
		if isASCII(alias) && len(alias) > 0 {
			return alias
		}
	}
	if len(a.Aliases) > 0 && a.Aliases[0] != "" {
		return a.Aliases[0]
	}
	return a.Name
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
