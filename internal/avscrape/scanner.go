package avscrape

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
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

		if err := s.writeMediaFiles(fs, &path, result, fullPath); err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
			continue
		}

		if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			media := MediaFromResult(result)
			if err := s.organize(fs, &path, media, fullPath, name); err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
				continue
			}
		}

		s.recordTask(code, fullPath, "done", "", result.Source)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// writeMediaFiles 写 NFO、下载图片、生成 .strm 预告片
func (s *Scanner) writeMediaFiles(fs FileSystem, path *models.AVPath, r *ScrapeResult, videoPath string) error {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	// 1. 海报：从候选池里找竖版图（高 > 宽）
	if url, ok := downloadImageByOrientation(fs, r.ImageCandidates, dir+"/poster.jpg", "portrait", "poster"); ok {
		r.Poster = url
	} else {
		helpers.AppLogger.Warnf("[AV元数据] %s poster 下载失败", r.Code)
	}

	// 2. 背景图：从候选池里找横版图（宽 > 高）
	if url, ok := downloadImageByOrientation(fs, r.ImageCandidates, dir+"/fanart.jpg", "landscape", "fanart"); ok {
		r.Fanart = url
	} else {
		helpers.AppLogger.Warnf("[AV元数据] %s fanart 下载失败", r.Code)
	}

	// 3. 写 NFO（此时 r.Poster / r.Fanart 已经更新为实际使用的 URL）
	if err := fs.Write(dir+"/"+base+".nfo", []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}

	// 4. 剧照
	if len(r.PreviewImages) > 0 {
		_ = fs.MkdirAll(dir + "/extrafanart")
		for i, url := range r.PreviewImages {
			if data, err := downloadImage(url); err == nil {
				_ = fs.Write(fmt.Sprintf("%s/extrafanart/scene-%02d.jpg", dir, i+1), data)
			} else {
				helpers.AppLogger.Warnf("[AV元数据] 剧照 %s 下载失败: %v", url, err)
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

// downloadImageWithSize 下载图片并读取尺寸
func downloadImageWithSize(url string) ([]byte, int, int, error) {
	data, err := downloadImage(url)
	if err != nil {
		return nil, 0, 0, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// 解码失败，返回原数据但尺寸未知
		return data, 0, 0, nil
	}
	return data, cfg.Width, cfg.Height, nil
}

// downloadImageByOrientation 按方向从候选池里下载图片
// wantOrientation: "portrait" 竖版（poster）/ "landscape" 横版（fanart）
// 返回实际使用的 URL 和是否成功
func downloadImageByOrientation(fs FileSystem, candidates []string, dstPath, wantOrientation, label string) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	var fallbackData []byte
	var fallbackURL string

	for i, url := range candidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil {
			helpers.AppLogger.Warnf("[AV元数据] %s 候选[%d]下载失败: %s => %v", label, i, url, err)
			continue
		}
		// 尺寸未知，作为 fallback
		if w == 0 || h == 0 {
			if fallbackData == nil {
				fallbackData = data
				fallbackURL = url
			}
			continue
		}
		isPortrait := h > w
		match := (wantOrientation == "portrait" && isPortrait) ||
			(wantOrientation == "landscape" && !isPortrait)
		if match {
			if err := fs.Write(dstPath, data); err != nil {
				helpers.AppLogger.Warnf("[AV元数据] %s 写盘失败: %v", label, err)
				continue
			}
			helpers.AppLogger.Infof("[AV元数据] %s 下载成功（候选[%d]，%dx%d，%s）: %s", label, i, w, h, wantOrientation, url)
			return url, true
		}
		helpers.AppLogger.Infof("[AV元数据] %s 候选[%d]方向不匹配（%dx%d，需要%s），跳过", label, i, w, h, wantOrientation)
	}

	// 没有匹配方向的，用 fallback
	if fallbackData != nil {
		if err := fs.Write(dstPath, fallbackData); err == nil {
			helpers.AppLogger.Infof("[AV元数据] %s 使用 fallback 图片: %s", label, fallbackURL)
			return fallbackURL, true
		}
	}
	return "", false
}

// organize 按命名模板整理文件到目标路径
func (s *Scanner) organize(fs FileSystem, path *models.AVPath, media *models.AVMedia, videoPath, videoName string) error {
	targetDir := path.TargetPath + "/" + media.Code
	if err := fs.MkdirAll(targetDir); err != nil {
		return err
	}
	ext := filepath.Ext(videoName)
	newName := media.Code + ext

	switch path.MoveMethod {
	case "copy":
		if err := fs.Copy(videoPath, targetDir); err != nil {
			return err
		}
		if filepath.Base(videoPath) != newName {
			newPath := targetDir + "/" + filepath.Base(videoPath)
			return fs.Rename(newPath, newName)
		}
		return nil
	default:
		return fs.Move(videoPath, targetDir, newName)
	}
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