package avscrape

import (
	"encoding/json"
	"fmt"
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

// Scan 扫描一个 AV 刮削目录（递归遍历所有子目录）
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

	for _, fullPath := range videoFiles {
		name := filepath.Base(fullPath)
		code := ExtractCode(name)
		if code == "" {
			helpers.AppLogger.Warnf("[AV扫描] 无法识别番号: %s", fullPath)
			s.recordTask("", fullPath, "failed", "无法识别番号", "")
			continue
		}
		helpers.AppLogger.Infof("[AV扫描] 处理文件 %s → 番号 %s", fullPath, code)

		// 是否已经刮削过
		var existing models.AVMedia
		hasExisting := s.DB.Where("code = ?", code).First(&existing).Error == nil

		var result *ScrapeResult
		if hasExisting {
			result = mediaToScrapeResult(&existing)
		} else {
			r, err := s.Svc.Scrape(code)
			if err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), "")
				continue
			}
			result = r
		}

		// 按操作方式处理
		if path.Mode == "scrape_only" {
			// 仅刮削：不移动文件，元数据写到源目录
			if err := s.writeMetaFiles(fs, filepath.Dir(fullPath),
				strings.TrimSuffix(name, filepath.Ext(name)), result); err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
				continue
			}
		} else if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			// 刮削和整理 / 仅整理：移动到目标目录，元数据写到目标目录
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

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// organize 按命名模板整理文件到目标路径，并在目标目录里生成 NFO 和图片
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

	// 1. 移动/复制视频到目标目录
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

	// 2. 在目标目录里写 NFO 和图片（覆盖旧文件）
	if r != nil {
		if err := s.writeMetaFiles(fs, targetDir, media.Code, r); err != nil {
			return err
		}
	}

	// 3. 移动模式：尝试清理源目录（含残留的 NFO/图片）
	if path.MoveMethod != "copy" {
		s.cleanupSourceDir(fs, filepath.Dir(videoPath), path.SourcePath)
	}

	return nil
}

// writeMetaFiles 在指定目录里写 NFO、图片、剧照、预告片
func (s *Scanner) writeMetaFiles(fs FileSystem, dir, baseName string, r *ScrapeResult) error {
	// 1. NFO
	if err := fs.Write(dir+"/"+baseName+".nfo", []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}
	// 2. 海报
	if r.Poster != "" {
		if data, err := downloadImage(r.Poster); err == nil {
			_ = fs.Write(dir+"/poster.jpg", data)
		}
	}
	// 3. 背景图
	if r.Fanart != "" {
		if data, err := downloadImage(r.Fanart); err == nil {
			_ = fs.Write(dir+"/fanart.jpg", data)
		}
	}
	// 4. 剧照
	if len(r.PreviewImages) > 0 {
		_ = fs.MkdirAll(dir + "/extrafanart")
		for i, url := range r.PreviewImages {
			if data, err := downloadImage(url); err == nil {
				_ = fs.Write(fmt.Sprintf("%s/extrafanart/scene-%02d.jpg", dir, i+1), data)
			}
		}
	}
	// 5. 预告片
	if r.Trailer != "" {
		_ = fs.MkdirAll(dir + "/trailers")
		_ = fs.Write(dir+"/trailers/trailer.strm", []byte(r.Trailer))
	}
	return nil
}

// cleanupSourceDir 清理源目录
// 如果源目录下已经没有视频文件，就把整个目录删掉（含残留的 NFO/图片）
// 但不会删除 SourcePath 本身
func (s *Scanner) cleanupSourceDir(fs FileSystem, sourceDir, rootSourcePath string) {
	if strings.TrimRight(sourceDir, "/") == strings.TrimRight(rootSourcePath, "/") {
		return
	}

	entries, err := fs.ListDetailed(sourceDir)
	if err != nil {
		helpers.AppLogger.Warnf("[AV扫描] 清理源目录时列出失败: %s, %v", sourceDir, err)
		return
	}

	for _, e := range entries {
		if e.IsDir {
			continue
		}
		if videoExts[strings.ToLower(filepath.Ext(e.Name))] {
			return
		}
	}

	if err := fs.DeleteDir(sourceDir); err != nil {
		helpers.AppLogger.Warnf("[AV扫描] 删除源目录失败: %s, %v", sourceDir, err)
		return
	}
	helpers.AppLogger.Infof("[AV扫描] 已清理源目录: %s", sourceDir)
}

// recordTask 记录任务
func (s *Scanner) recordTask(code, filePath, status, msg, provider string) {
	s.DB.Create(&models.AVTask{
		Code:     code,
		FilePath: filePath,
		Status:   status,
		Message:  msg,
		Provider: provider,
	})
}

// mediaToScrapeResult 从数据库的 AVMedia 反构造 ScrapeResult
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

// walkVideos 递归遍历目录，返回所有视频文件的完整路径
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

// renderTemplate 渲染命名模板
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
		if a.Name == "" {
			continue
		}
		if firstActor == "" {
			firstActor = a.Name
		}
		if i > 0 {
			allActors += ", "
		}
		allActors += a.Name
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

// sanitizePath 去掉路径里不允许的字符
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

// extractYear 从日期字符串里取年份
func extractYear(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}
