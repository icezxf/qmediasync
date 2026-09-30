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

// VideoFile 扫描到的视频文件（带 115 fileId，避免后续重复查询）
type VideoFile struct {
	Path     string
	Name     string
	ID       string // 115 fileId
	PickCode string
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

	for _, vf := range videoFiles {
		code := ExtractCode(vf.Name)
		if code == "" {
			helpers.AppLogger.Warnf("[AV扫描] 无法识别番号: %s", vf.Path)
			s.recordTask("", vf.Path, "failed", "无法识别番号", "")
			continue
		}
		helpers.AppLogger.Infof("[AV扫描] 处理文件 %s → 番号 %s", vf.Path, code)

		var existing models.AVMedia
		hasExisting := s.DB.Where("code = ?", code).First(&existing).Error == nil

		var result *ScrapeResult
		if hasExisting {
			result = mediaToScrapeResult(&existing)
		} else {
			r, err := s.Svc.Scrape(code)
			if err != nil {
				s.recordTask(code, vf.Path, "failed", err.Error(), "")
				continue
			}
			result = r
		}

		if path.Mode == "scrape_only" {
			tmpDir := filepath.Dir(vf.Path)
			baseName := strings.TrimSuffix(vf.Name, filepath.Ext(vf.Name))
			files, err := s.prepareMetaFiles(baseName, result)
			if err != nil {
				s.recordTask(code, vf.Path, "failed", err.Error(), result.Source)
				continue
			}
			if _, err := fs.QueueUploads(files, tmpDir, path.AccountID, path.SourceType); err != nil {
				s.recordTask(code, vf.Path, "failed", err.Error(), result.Source)
				continue
			}
		} else if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			if err := s.organize(fs, &path, MediaFromResult(result), vf, result); err != nil {
				s.recordTask(code, vf.Path, "failed", err.Error(), result.Source)
				continue
			}
		}

		msg := ""
		if hasExisting {
			msg = "已存在，重新整理完成"
		}
		s.recordTask(code, vf.Path, "done", msg, result.Source)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// organize 视频移动 + 元数据入上传队列
func (s *Scanner) organize(fs FileSystem, path *models.AVPath, media *models.AVMedia, vf VideoFile, r *ScrapeResult) error {
	relDir := renderTemplate(path.NameTemplate, media)
	if relDir == "" {
		relDir = media.Code
	}
	targetDir := path.TargetPath + "/" + relDir
	if err := fs.MkdirAll(targetDir); err != nil {
		return err
	}

	ext := filepath.Ext(vf.Name)
	newName := media.Code + ext
	newVideoPath := targetDir + "/" + newName

	if !fs.Exists(newVideoPath) {
		switch path.MoveMethod {
		case "copy":
			if err := fs.Copy(vf.Path, targetDir); err != nil {
				return fmt.Errorf("复制视频失败: %w", err)
			}
			if filepath.Base(vf.Path) != newName {
				oldPath := targetDir + "/" + filepath.Base(vf.Path)
				if err := fs.Rename(oldPath, newName); err != nil {
					return fmt.Errorf("重命名视频失败: %w", err)
				}
			}
		default:
			// 传入 vf.ID，115 跳过源文件 detail 查询
			if err := fs.Move(vf.Path, vf.ID, targetDir, newName); err != nil {
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

	if path.MoveMethod != "copy" {
		s.cleanupSourceDir(fs, filepath.Dir(vf.Path), path.SourcePath)
	}

	return nil
}

// prepareMetaFiles 生成元数据到本地临时目录
// 剧照不再限制数量，全部下载（走上传队列，不占同步 API）
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

	// NFO
	nfoPath := filepath.Join(tmpDir, baseName+".nfo")
	if err := os.WriteFile(nfoPath, []byte(GenerateNFO(r)), 0644); err != nil {
		return nil, fmt.Errorf("写 NFO 失败: %w", err)
	}
	files = append(files, LocalFile{LocalPath: nfoPath, RemoteName: baseName + ".nfo"})

	// 海报
	if r.Poster != "" {
		p := filepath.Join(tmpDir, "poster.jpg")
		if err := helpers.DownloadFile(r.Poster, p, ""); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "poster.jpg"})
		} else {
			helpers.AppLogger.Warnf("[AV元数据] 下载 poster 失败: %v", err)
		}
	}
	// 背景图
	if r.Fanart != "" {
		p := filepath.Join(tmpDir, "fanart.jpg")
		if err := helpers.DownloadFile(r.Fanart, p, ""); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "fanart.jpg"})
		} else {
			helpers.AppLogger.Warnf("[AV元数据] 下载 fanart 失败: %v", err)
		}
	}
	// 剧照（不限制数量，全部下载到本地临时目录）
	for i, url := range r.PreviewImages {
		remoteName := fmt.Sprintf("extrafanart/scene-%02d.jpg", i+1)
		localPath := filepath.Join(tmpDir, fmt.Sprintf("scene-%02d.jpg", i+1))
		if err := helpers.DownloadFile(url, localPath, ""); err == nil {
			files = append(files, LocalFile{LocalPath: localPath, RemoteName: remoteName})
		} else {
			helpers.AppLogger.Warnf("[AV元数据] 下载剧照 %d 失败: %v", i+1, err)
		}
	}
	// 预告片
	if r.Trailer != "" {
		p := filepath.Join(tmpDir, "trailer.strm")
		if err := os.WriteFile(p, []byte(r.Trailer), 0644); err == nil {
			files = append(files, LocalFile{LocalPath: p, RemoteName: "trailers/trailer.strm"})
		}
	}

	return files, nil
}

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

// walkVideos 递归遍历目录，返回带 fileId 的视频列表
func walkVideos(fs FileSystem, root string, depth int) ([]VideoFile, error) {
	if depth > 10 {
		return nil, nil
	}
	entries, err := fs.ListDetailed(root)
	if err != nil {
		return nil, err
	}
	var videos []VideoFile
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
			videos = append(videos, VideoFile{
				Path:     e.Path,
				Name:     e.Name,
				ID:       e.ID,       // 115: fileId，其他源为空
				PickCode: e.PickCode, // 115: pickcode
			})
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