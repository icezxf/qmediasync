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

		// 1. 刮削（无条件重新刮）
		result, err := s.Svc.Scrape(code)
		if err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), "")
			continue
		}

		// 2. 用命名模板计算目标子目录，例如 {actor}/{number} → 新有菜/MIDV-192
		subDir := renderNameTemplate(path.NameTemplate, result, code)
		targetDir := path.TargetPath
		if subDir != "" {
			targetDir = path.TargetPath + "/" + subDir
		}
		helpers.AppLogger.Infof("[AV扫描] 目标目录: %s", targetDir)

		// 3. 创建目标目录
		if err := fs.MkdirAll(targetDir); err != nil {
			s.recordTask(code, fullPath, "failed", "创建目标目录失败: "+err.Error(), result.Source)
			continue
		}

		// 4. 元数据（NFO/图片/剧照/预告片）全部写到目标目录
		if err := s.writeMediaFiles(fs, targetDir, result); err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
			continue
		}

		// 5. 移动视频文件到目标目录
		if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			newName := code + ext
			if err := fs.Move(fullPath, targetDir, newName); err != nil {
				s.recordTask(code, fullPath, "failed", "移动视频失败: "+err.Error(), result.Source)
				continue
			}
		}

		s.recordTask(code, fullPath, "done", "", result.Source)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// renderNameTemplate 解析命名模板，把 {actor}、{number} 等变量替换为实际值
func renderNameTemplate(tmpl string, r *ScrapeResult, code string) string {
	if tmpl == "" {
		return code
	}
	out := tmpl

	// {actor} 首个演员
	actor := ""
	if len(r.Actors) > 0 {
		actor = r.Actors[0].Name
	}
	out = strings.ReplaceAll(out, "{actor}", sanitizePath(actor))

	// {actors} 全部演员（逗号分隔）
	var actorNames []string
	for _, a := range r.Actors {
		if a.Name != "" {
			actorNames = append(actorNames, a.Name)
		}
	}
	out = strings.ReplaceAll(out, "{actors}", sanitizePath(strings.Join(actorNames, ", ")))

	// {number} 番号
	out = strings.ReplaceAll(out, "{number}", code)

	// {title} 标题
	out = strings.ReplaceAll(out, "{title}", sanitizePath(r.Title))

	// {year} 年份
	year := ""
	if len(r.ReleaseDate) >= 4 {
		year = r.ReleaseDate[:4]
	}
	out = strings.ReplaceAll(out, "{year}", year)

	// {studio} 片商
	out = strings.ReplaceAll(out, "{studio}", sanitizePath(r.Studio))

	// {label} 厂牌
	out = strings.ReplaceAll(out, "{label}", sanitizePath(r.Label))

	// {series} 系列
	out = strings.ReplaceAll(out, "{series}", sanitizePath(r.Series))

	// 清理多余的斜杠
	out = strings.Trim(out, "/")
	for strings.Contains(out, "//") {
		out = strings.ReplaceAll(out, "//", "/")
	}
	return out
}

// sanitizePath 去掉文件名/目录名里不能用的字符
func sanitizePath(s string) string {
	if s == "" {
		return ""
	}
	// Windows + Linux 都不允许的字符
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	s = replacer.Replace(s)
	s = strings.TrimSpace(s)
	return s
}

// writeMediaFiles 把 NFO、海报、背景图、缩略图、剧照、预告片全部写到目标目录
func (s *Scanner) writeMediaFiles(fs FileSystem, targetDir string, r *ScrapeResult) error {
	// 1. 写 NFO
	nfoPath := targetDir + "/" + r.Code + ".nfo"
	if err := fs.Write(nfoPath, []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}
	helpers.AppLogger.Infof("[AV元数据] NFO 写入: %s", nfoPath)

	// 2. 海报（JavStash 优先，失败 fallback）
	if !downloadFirstSuccess(fs, r.PosterCandidates, targetDir+"/poster.jpg", "poster") {
		helpers.AppLogger.Warnf("[AV元数据] %s 所有源的 poster 都下载失败", r.Code)
	}

	// 3. 缩略图（用 poster 的候选，内容相同）
	if !downloadFirstSuccess(fs, r.PosterCandidates, targetDir+"/thumb.jpg", "thumb") {
		helpers.AppLogger.Warnf("[AV元数据] %s 所有源的 thumb 都下载失败", r.Code)
	}

	// 4. 背景图（仅用真实 fanart，不再用剧照拼接）
	if len(r.FanartCandidates) > 0 {
		if !downloadFirstSuccess(fs, r.FanartCandidates, targetDir+"/fanart.jpg", "fanart") {
			helpers.AppLogger.Warnf("[AV元数据] %s 所有源的 fanart 都下载失败", r.Code)
		}
	} else {
		helpers.AppLogger.Infof("[AV元数据] %s 没有 fanart 来源，跳过", r.Code)
	}

	// 5. 剧照
	if len(r.PreviewImages) > 0 {
		_ = fs.MkdirAll(targetDir + "/extrafanart")
		okCount := 0
		for i, url := range r.PreviewImages {
			if data, err := downloadImage(url); err == nil {
				_ = fs.Write(fmt.Sprintf("%s/extrafanart/scene-%02d.jpg", targetDir, i+1), data)
				okCount++
			} else {
				helpers.AppLogger.Warnf("[AV元数据] 剧照 %s 下载失败: %v", url, err)
			}
		}
		helpers.AppLogger.Infof("[AV元数据] %s 剧照 %d/%d 张下载完成", r.Code, okCount, len(r.PreviewImages))
	}

	// 6. 预告片
	if r.Trailer != "" {
		_ = fs.MkdirAll(targetDir + "/trailers")
		_ = fs.Write(targetDir+"/trailers/trailer.strm", []byte(r.Trailer))
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

func (s *Scanner) recordTask(code, filePath, status, msg, provider string) {
	s.DB.Create(&models.AVTask{
		Code:     code,
		FilePath: filePath,
		Status:   status,
		Message:  msg,
		Provider: provider,
	})
}