package avscrape

import (
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

// Scan 扫描一个 AV 刮削目录
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

		// ===== 无条件重新刮削 =====
		// 只要文件还在源目录里，不管数据库有没有，都重新刮一遍
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
func (s *Scanner) writeMediaFiles(fs FileSystem, path *models.AVPath, r *ScrapeResult, videoPath string) error {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	// 1. 写 NFO
	if err := fs.Write(dir+"/"+base+".nfo", []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}

	// 2. 海报（多源 fallback，JavStash 优先）
	if !downloadFirstSuccess(fs, r.PosterCandidates, dir+"/poster.jpg", "poster") {
		helpers.AppLogger.Warnf("[AV元数据] %s 所有源的 poster 都下载失败", r.Code)
	}

	// 3. 背景图（多源 fallback）
	if !downloadFirstSuccess(fs, r.FanartCandidates, dir+"/fanart.jpg", "fanart") {
		if len(r.PreviewImages) > 0 {
			if data, err := downloadImage(r.PreviewImages[0]); err == nil {
				_ = fs.Write(dir+"/fanart.jpg", data)
				helpers.AppLogger.Infof("[AV元数据] %s 使用剧照第一张作为 fanart", r.Code)
			}
		}
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

// downloadFirstSuccess 按顺序尝试候选 URL，第一个成功就返回
func downloadFirstSuccess(fs FileSystem, candidates []string, dstPath, label string) bool {
	if len(candidates) == 0 {
		return false
	}
	for i, url := range candidates {
		data, err := downloadImage(url)
		if err != nil {
			helpers.AppLogger.Warnf("[AV元数据] %s 候选[%d]下载失败: %s => %v", label, i, url, err)
			continue
		}
		if err := fs.Write(dstPath, data); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] %s 写盘失败: %v", label, err)
			continue
		}
		helpers.AppLogger.Infof("[AV元数据] %s 下载成功（候选[%d]）: %s", label, i, url)
		return true
	}
	return false
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