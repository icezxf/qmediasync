package avscrape

import (
	"bytes"
	"encoding/json"
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

// renderFolderTemplate 根据模板生成目标子路径
// 支持变量：{actors} {num} {code} {title} {year} {studio} {label} {series}
// 模板可以包含 /，例如 "{actors}/{num}"
func renderFolderTemplate(tpl string, m *models.AVMedia) string {
	if tpl == "" {
		tpl = "{code}"
	}

	// 解析演员
	actorName := ""
	if m.Actors != "" {
		var list []Actor
		if err := json.Unmarshal([]byte(m.Actors), &list); err == nil && len(list) > 0 {
			actorName = list[0].Name // 取第一位演员
		}
	}

	// 年份
	year := ""
	if len(m.ReleaseDate) >= 4 {
		year = m.ReleaseDate[:4]
	}

	result := tpl
	result = strings.ReplaceAll(result, "{actors}", actorName)
	result = strings.ReplaceAll(result, "{num}", m.Code)
	result = strings.ReplaceAll(result, "{code}", m.Code)
	result = strings.ReplaceAll(result, "{title}", m.Title)
	result = strings.ReplaceAll(result, "{year}", year)
	result = strings.ReplaceAll(result, "{studio}", m.Studio)
	result = strings.ReplaceAll(result, "{label}", m.Label)
	result = strings.ReplaceAll(result, "{series}", m.Series)
	return result
}

// writeMediaFiles 写 NFO、下载图片、生成 .strm 预告片
func (s *Scanner) writeMediaFiles(fs FileSystem, path *models.AVPath, r *ScrapeResult, videoPath string) error {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	// 1. 海报：优先竖版；没有就用横版 fallback
	if url, ok := downloadImageByOrientation(fs, r.ImageCandidates, dir+"/poster.jpg", "portrait", "poster"); ok {
		r.Poster = url
	} else {
		helpers.AppLogger.Warnf("[AV元数据] %s poster 下载失败", r.Code)
	}

	// 2. 背景图：优先横版；没有就用竖版 fallback
	if url, ok := downloadImageByOrientation(fs, r.ImageCandidates, dir+"/fanart.jpg", "landscape", "fanart"); ok {
		r.Fanart = url
	} else {
		helpers.AppLogger.Warnf("[AV元数据] %s fanart 下载失败", r.Code)
	}

	// 3. 写 NFO（此时 r.Poster / r.Fanart 已更新为实际使用的 URL）
	if err := fs.Write(dir+"/"+base+".nfo", []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
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

// downloadImageByOrientation 按方向从候选池里下载图片
// 找不到目标方向时用第一张已下载的兜底
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
		// 方向不匹配，记为 fallback
		if fallbackData == nil {
			fallbackData = data
			fallbackURL = url
			helpers.AppLogger.Infof("[AV元数据] %s 候选[%d]方向不匹配（%dx%d），记为 fallback", label, i, w, h)
		}
	}

	// 没找到目标方向，用 fallback 兜底
	if fallbackData != nil {
		if err := fs.Write(dstPath, fallbackData); err == nil {
			helpers.AppLogger.Infof("[AV元数据] %s 未找到 %s 方向图，使用 fallback: %s", label, wantOrientation, fallbackURL)
			return fallbackURL, true
		}
	}
	return "", false
}

// organize 按命名模板整理文件到目标路径
func (s *Scanner) organize(fs FileSystem, path *models.AVPath, media *models.AVMedia, videoPath, videoName string) error {
	// ===== 1. 用模板生成目标子路径 =====
	subPath := renderFolderTemplate(path.NameTemplate, media)
	// 清理路径中可能出现的多余斜杠和空段
	subPath = strings.Trim(subPath, "/")
	for strings.Contains(subPath, "//") {
		subPath = strings.ReplaceAll(subPath, "//", "/")
	}
	if subPath == "" {
		subPath = media.Code
	}
	targetDir := path.TargetPath + "/" + subPath
	helpers.AppLogger.Infof("[AV整理] 目标目录: %s (模板=%s)", targetDir, path.NameTemplate)

	if err := fs.MkdirAll(targetDir); err != nil {
		return err
	}

	ext := filepath.Ext(videoName)
	newName := media.Code + ext
	srcDir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), ext)

	// ===== 2. 移动视频 =====
	switch path.MoveMethod {
	case "copy":
		if err := fs.Copy(videoPath, targetDir); err != nil {
			return err
		}
		if filepath.Base(videoPath) != newName {
			newPath := targetDir + "/" + filepath.Base(videoPath)
			if err := fs.Rename(newPath, newName); err != nil {
				return err
			}
		}
	default:
		if err := fs.Move(videoPath, targetDir, newName); err != nil {
			return err
		}
	}

	// ===== 3. 移动 NFO =====
	nfoSrc := srcDir + "/" + base + ".nfo"
	if fs.Exists(nfoSrc) {
		if err := fs.Move(nfoSrc, targetDir, ""); err != nil {
			helpers.AppLogger.Warnf("[AV整理] 移动 NFO 失败: %v", err)
		} else {
			helpers.AppLogger.Infof("[AV整理] 移动 NFO → %s/", targetDir)
		}
	}

	// ===== 4. 移动 poster / fanart / thumb =====
	for _, imgName := range []string{"poster.jpg", "fanart.jpg", "thumb.jpg"} {
		src := srcDir + "/" + imgName
		if fs.Exists(src) {
			if err := fs.Move(src, targetDir, ""); err != nil {
				helpers.AppLogger.Warnf("[AV整理] 移动 %s 失败: %v", imgName, err)
			} else {
				helpers.AppLogger.Infof("[AV整理] 移动 %s → %s/", imgName, targetDir)
			}
		}
	}

	// ===== 5. 移动 extrafanart 目录 =====
	extraSrc := srcDir + "/extrafanart"
	extraDst := targetDir + "/extrafanart"
	if fs.Exists(extraSrc) {
		if err := fs.MkdirAll(extraDst); err == nil {
			names, _ := fs.List(extraSrc)
			for _, n := range names {
				if err := fs.Move(extraSrc+"/"+n, extraDst, ""); err != nil {
					helpers.AppLogger.Warnf("[AV整理] 移动 extrafanart/%s 失败: %v", n, err)
				}
			}
			_ = fs.DeleteDir(extraSrc)
			helpers.AppLogger.Infof("[AV整理] 移动 extrafanart/ (%d 张)", len(names))
		}
	}

	// ===== 6. 移动 trailers 目录 =====
	trailerSrc := srcDir + "/trailers"
	trailerDst := targetDir + "/trailers"
	if fs.Exists(trailerSrc) {
		if err := fs.MkdirAll(trailerDst); err == nil {
			names, _ := fs.List(trailerSrc)
			for _, n := range names {
				if err := fs.Move(trailerSrc+"/"+n, trailerDst, ""); err != nil {
					helpers.AppLogger.Warnf("[AV整理] 移动 trailers/%s 失败: %v", n, err)
				}
			}
			_ = fs.DeleteDir(trailerSrc)
			helpers.AppLogger.Infof("[AV整理] 移动 trailers/ (%d 个)", len(names))
		}
	}

	return nil
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
