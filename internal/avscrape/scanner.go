package avscrape

import (
	"fmt"
	"path/filepath"
	"strings"

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

// Scan 扫描一个 AV 刮削目录
func (s *Scanner) Scan(pathID uint) error {
	var path AVPath
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

		// 已刮削过，跳过刮削，但可能还需要整理
		var existing AVMedia
		if err := s.DB.Where("code = ?", code).First(&existing).Error; err == nil {
			if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
				if err := s.organize(fs, &path, &existing, fullPath, name); err != nil {
					s.recordTask(code, fullPath, "failed", err.Error(), "")
				} else {
					s.recordTask(code, fullPath, "done", "已存在，重新整理完成", "")
				}
			}
			continue
		}

		// 刮削
		result, err := s.Svc.Scrape(code)
		if err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), "")
			continue
		}

		// 写 NFO + 下载图片
		if err := s.writeMediaFiles(fs, &path, result, fullPath); err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
			continue
		}

		// 整理文件
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
func (s *Scanner) writeMediaFiles(fs FileSystem, path *AVPath, r *ScrapeResult, videoPath string) error {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	// 1. 写 NFO
	if err := fs.Write(dir+"/"+base+".nfo", []byte(GenerateNFO(r))); err != nil {
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

// organize 按命名模板整理文件到目标路径
func (s *Scanner) organize(fs FileSystem, path *AVPath, media *AVMedia, videoPath, videoName string) error {
	targetDir := path.TargetPath + "/" + media.Code
	if err := fs.MkdirAll(targetDir); err != nil {
		return err
	}
	ext := filepath.Ext(videoName)
	newName := media.Code + ext

	switch path.MoveMethod {
	case "copy":
		// 复制到目标目录
		if err := fs.Copy(videoPath, targetDir); err != nil {
			return err
		}
		// 再改名
		if filepath.Base(videoPath) != newName {
			newPath := targetDir + "/" + filepath.Base(videoPath)
			return fs.Rename(newPath, newName)
		}
		return nil
	default:
		// move / softlink / hardlink 统一按移动处理
		return fs.Move(videoPath, targetDir, newName)
	}
}

// recordTask 记录任务
func (s *Scanner) recordTask(code, filePath, status, msg, provider string) {
	s.DB.Create(&AVTask{
		Code:     code,
		FilePath: filePath,
		Status:   status,
		Message:  msg,
		Provider: provider,
	})
}